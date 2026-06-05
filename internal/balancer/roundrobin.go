// Package balancer distributes traffic across equivalent backends within a
// routing tier. It complements quality-aware ranking: the router still filters
// and orders candidates by eval/cost/latency, but load balancing rotates which
// quality-qualified model receives the primary attempt so one winner does not
// absorb all traffic.
package balancer

import "sync"

// RoundRobin picks the next index in a cyclic sequence for a named group.
// Safe for concurrent use.
type RoundRobin struct {
	mu  sync.Mutex
	seq map[string]uint64
}

// NewRoundRobin creates a round-robin counter table.
func NewRoundRobin() *RoundRobin {
	return &RoundRobin{seq: make(map[string]uint64)}
}

// Next returns an index in [0, n) for the given group key, advancing the
// counter each call. When n <= 0 it returns 0.
func (rr *RoundRobin) Next(groupKey string, n int) int {
	if n <= 0 {
		return 0
	}
	rr.mu.Lock()
	defer rr.mu.Unlock()
	idx := int(rr.seq[groupKey] % uint64(n))
	rr.seq[groupKey]++
	return idx
}

// RotateChain moves the round-robin pick to the front of a ranked candidate
// chain. The remaining models keep their relative order for failover.
func RotateChain(groupKey string, ranked []string, rr *RoundRobin) []string {
	if rr == nil || len(ranked) <= 1 {
		return ranked
	}
	i := rr.Next(groupKey, len(ranked))
	primary := ranked[i]
	out := make([]string, 0, len(ranked))
	out = append(out, primary)
	for j, m := range ranked {
		if j != i {
			out = append(out, m)
		}
	}
	return out
}
