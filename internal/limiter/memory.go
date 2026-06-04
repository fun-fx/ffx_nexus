package limiter

import (
	"context"
	"sync"
	"time"
)

// Memory is an in-process Limiter for single-node / zero-dependency mode.
// State is not shared across replicas; use Redis in production.
type Memory struct {
	mu    sync.Mutex
	rpm   map[string]int     // rpmKey -> count in current minute
	spend map[string]float64 // spendKey -> month spend
	now   func() time.Time
}

// NewMemory creates an in-memory limiter.
func NewMemory() *Memory {
	return &Memory{
		rpm:   make(map[string]int),
		spend: make(map[string]float64),
		now:   time.Now,
	}
}

// Allow implements Limiter.
func (m *Memory) Allow(_ context.Context, keyID string, rpmLimit int) (bool, error) {
	if rpmLimit <= 0 {
		return true, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	k := rpmKey(keyID, minuteWindow(m.now()))
	// Opportunistically drop stale minute buckets to bound memory.
	for existing := range m.rpm {
		if existing != k && len(m.rpm) > 10000 {
			delete(m.rpm, existing)
		}
	}
	m.rpm[k]++
	return m.rpm[k] <= rpmLimit, nil
}

// MonthlySpend implements Limiter.
func (m *Memory) MonthlySpend(_ context.Context, keyID string) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spend[spendKey(keyID, monthWindow(m.now()))], nil
}

// AddSpend implements Limiter.
func (m *Memory) AddSpend(_ context.Context, keyID string, costUSD float64) error {
	if costUSD <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spend[spendKey(keyID, monthWindow(m.now()))] += costUSD
	return nil
}
