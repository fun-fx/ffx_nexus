package evals

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ProfileStore is the durable home for EvalProfile rows. The interface
// intentionally exposes only what the console + worker need today so
// we can swap ClickHouse ⇄ Postgres without changing call sites.
//
// Implementations live in pg.go / clickhouse.go (added in PR #135 along
// with the score history tables). There is also the ephemeral
// MemoryStore below used in tests.
type ProfileStore interface {
	// List returns profiles visible to `scope`. When `ownerUserID` is
	// empty, org-scoped profiles are returned; otherwise the caller
	// gets their own user-scoped rows too. Admins are filtered in at
	// the callerCanSee layer (PR #136).
	List(ctx context.Context, ownerUserID string) ([]EvalProfile, error)
	// Get returns a single profile by id (admin / owner only; caller
	// enforces this).
	Get(ctx context.Context, id string) (*EvalProfile, error)
	// Save persists a profile. The implementation is responsible for
	// ID assignment on create; updates use the same call with the
	// existing ID.
	Save(ctx context.Context, p *EvalProfile) error
	// Delete removes a profile by id. Missing is not an error (idempotent).
	Delete(ctx context.Context, id string) error
}

// ErrProfileNotFound is returned by Get / Delete when the row is
// absent. Call sites should distinguish "not yours to see" from "really
// missing" via permission checks upstream.
var ErrProfileNotFound = errors.New("eval profile not found")

// MemoryStore holds profiles in-process and is intended for unit tests
// where spinning up Postgres/ClickHouse is overkill. Mirrors the
// semantics expected of the durable stores so production code can be
// tested against it.
type MemoryStore struct {
	mu       sync.RWMutex
	profiles map[string]*EvalProfile
	clock    func() time.Time
	counter  uint64
}

// NewMemoryStore creates a deterministic, in-memory ProfileStore. Tests
// pass a custom clock when they care about CreatedAt ordering; otherwise
// it uses time.Now.
func NewMemoryStore(clock func() time.Time) *MemoryStore {
	if clock == nil {
		clock = time.Now
	}
	return &MemoryStore{profiles: make(map[string]*EvalProfile), clock: clock}
}

// nextID produces a sortable id without depending on rand —
// sufficiently unique for an in-memory store. Database-backed
// stores replace this with UUIDv7 once PR #137 lands.
func (m *MemoryStore) nextID() string {
	return fmt.Sprintf("ep_%d_%d", m.clock().UnixNano(), atomic.AddUint64(&m.counter, 1))
}

func (m *MemoryStore) List(_ context.Context, ownerUserID string) ([]EvalProfile, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]EvalProfile, 0, len(m.profiles))
	for _, p := range m.profiles {
		if p.Scope == ScopeUser && ownerUserID != "" && p.OwnerUserID != ownerUserID {
			continue
		}
		out = append(out, *p.Clone())
	}
	return out, nil
}

func (m *MemoryStore) Get(_ context.Context, id string) (*EvalProfile, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.profiles[id]
	if !ok {
		return nil, ErrProfileNotFound
	}
	return p.Clone(), nil
}

func (m *MemoryStore) Save(_ context.Context, p *EvalProfile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.TrimSpace(p.ID) == "" {
		p.ID = m.nextID()
	} else {
		existing, ok := m.profiles[p.ID]
		if ok {
			p.CreatedAt = existing.CreatedAt
		}
	}
	now := m.clock().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	m.profiles[p.ID] = p.Clone()
	return nil
}

func (m *MemoryStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.profiles, id)
	return nil
}
