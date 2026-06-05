// Package balancer distributes traffic across equivalent backends within a
// routing tier. It complements quality-aware ranking: the router still filters
// and orders candidates by eval/cost/latency, but load balancing spreads the
// primary attempt across qualified models so one winner does not absorb all
// traffic — weighted by rank so better models still receive proportionally more.
package balancer

import (
	"strings"
	"sync"
)

// WeightedRR performs smooth weighted round-robin selection. Candidates are
// weighted by rank (the first/best gets the highest weight), so the best model
// receives proportionally more primary traffic while lower-ranked models still
// get a fair share. Selection is deterministic and proportional (nginx-style
// SWRR), avoiding the thundering-herd of pure random. Safe for concurrent use.
type WeightedRR struct {
	mu     sync.Mutex
	groups map[string]*swrrState
}

type swrrState struct {
	fingerprint string // identity of the candidate set; reset when it changes
	current     []int  // running SWRR weights
}

// NewWeightedRR creates a weighted round-robin balancer.
func NewWeightedRR() *WeightedRR {
	return &WeightedRR{groups: make(map[string]*swrrState)}
}

// Next returns the index in ranked to use as the primary attempt, advancing the
// per-group SWRR state. Weights are derived from rank: index i gets weight
// (n - i), so the top candidate is weighted highest. Returns 0 for n <= 1.
func (w *WeightedRR) Next(groupKey string, ranked []string) int {
	n := len(ranked)
	if n <= 1 {
		return 0
	}
	fp := strings.Join(ranked, "\x00")

	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.groups[groupKey]
	if st == nil || st.fingerprint != fp {
		st = &swrrState{fingerprint: fp, current: make([]int, n)}
		w.groups[groupKey] = st
	}

	// Weights by rank (best first) and their total.
	total := 0
	best := 0
	for i := 0; i < n; i++ {
		wi := n - i
		total += wi
		st.current[i] += wi
		if st.current[i] > st.current[best] {
			best = i
		}
	}
	st.current[best] -= total
	return best
}

// RotateChain moves the weighted pick to the front of a ranked candidate chain.
// The remaining models keep their relative order for failover.
func RotateChain(groupKey string, ranked []string, w *WeightedRR) []string {
	if w == nil || len(ranked) <= 1 {
		return ranked
	}
	i := w.Next(groupKey, ranked)
	out := make([]string, 0, len(ranked))
	out = append(out, ranked[i])
	for j, m := range ranked {
		if j != i {
			out = append(out, m)
		}
	}
	return out
}
