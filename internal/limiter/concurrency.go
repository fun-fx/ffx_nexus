// Package limiter — per-key in-flight concurrency caps.
//
// Allow() and MonthlySpend() already keep one virtual key from running
// away with the cluster's request budget (RPM / $). But they only
// police SCALAR rate. A misbehaving caller can still POST 50 prompts
// in <1ms, all within RPM, and starve every other caller behind them
// at the upstream provider (where the queue actually matters).
//
// Concurrency caps answer the "too many in-flight at once" question.
// Each Cap tracks `keyID -> inflight`. Acquire increments, Release
// decrements. Returning false blocks (or 429s) — caller decides.
//
// Concurrency caps are *local* to the process. That's deliberate: a
// cluster-wide cap would be tightest, but it would also need a
// distributed lock dance at every request which is the opposite of
// what high-concurrency requirements want. In practice the per-process
// cap is multiplied by replica count, which is a known multiplier.

package limiter

import (
	"context"
	"sync"
)

// ConcurrencyCap bounds how many requests can be in-flight for a single
// virtual key, on a single replica. Pass nil to disable (default
// behaviour preserved).
type ConcurrencyCap struct {
	mu        sync.Mutex
	perKey    map[string]int
	maxPerKey int
}

// NewConcurrencyCap builds a cap with maxPerKey == max concurrent
// in-flight per virtual key. Pass 0 / negative to disable (the
// resulting cap is a no-op).
func NewConcurrencyCap(maxPerKey int) *ConcurrencyCap {
	return &ConcurrencyCap{
		perKey:    make(map[string]int),
		maxPerKey: maxPerKey,
	}
}

// Acquire records one in-flight request against keyID. Returns true
// if the request is permitted (under or at cap), false if the cap has
// been reached and the caller should reject.
//
// The caller MUST pair every successful Acquire with a Release, ideally
// via defer.
func (c *ConcurrencyCap) Acquire(_ context.Context, keyID string) bool {
	if c == nil || c.maxPerKey <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.perKey[keyID] >= c.maxPerKey {
		return false
	}
	c.perKey[keyID]++
	return true
}

// Release decrements the in-flight counter for keyID. Calling Release
// without a prior Acquire is a programming error but safely clamped:
// floor at 0 so we never end up with negative counts.
func (c *ConcurrencyCap) Release(_ context.Context, keyID string) {
	if c == nil || c.maxPerKey <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.perKey[keyID] > 0 {
		c.perKey[keyID]--
	}
	if c.perKey[keyID] == 0 {
		// Reap empty buckets so the map doesn't grow unbounded over
		// time. Cheap because this is inside the lock we already hold.
		delete(c.perKey, keyID)
	}
}

// Max returns the configured per-key cap. Useful for surfacing the
// value to /admin endpoints so operators can introspect.
func (c *ConcurrencyCap) Max() int {
	if c == nil {
		return 0
	}
	return c.maxPerKey
}

// Inflight returns the current in-flight count for keyID. Used by the
// observability adapter to expose gauge `nexus_concurrency_inflight`.
func (c *ConcurrencyCap) Inflight(keyID string) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.perKey[keyID]
}
