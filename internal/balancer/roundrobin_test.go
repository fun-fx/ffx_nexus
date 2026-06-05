package balancer

import "testing"

func TestWeightedDistributionByRank(t *testing.T) {
	w := NewWeightedRR()
	ranked := []string{"best", "mid", "cheap"}
	counts := map[string]int{}
	// Over one full SWRR cycle (sum of weights 3+2+1 = 6) the distribution
	// should match the rank weights exactly.
	for i := 0; i < 6; i++ {
		idx := w.Next("fast", ranked)
		counts[ranked[idx]]++
	}
	if counts["best"] != 3 || counts["mid"] != 2 || counts["cheap"] != 1 {
		t.Fatalf("want best:3 mid:2 cheap:1, got %v", counts)
	}
}

func TestWeightedFavorsTopButSpreads(t *testing.T) {
	w := NewWeightedRR()
	ranked := []string{"a", "b"}
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		idx := w.Next("g", ranked)
		seen[ranked[idx]]++
	}
	// weights 2:1 -> a should get more but b must still appear.
	if seen["a"] <= seen["b"] {
		t.Fatalf("top model should get more traffic, got %v", seen)
	}
	if seen["b"] == 0 {
		t.Fatalf("lower model must still receive traffic, got %v", seen)
	}
}

func TestWeightedResetsOnCandidateChange(t *testing.T) {
	w := NewWeightedRR()
	_ = w.Next("g", []string{"a", "b", "c"})
	// Changing the candidate set must not panic (state re-inits to new length).
	idx := w.Next("g", []string{"x", "y"})
	if idx < 0 || idx > 1 {
		t.Fatalf("index out of range after set change: %d", idx)
	}
}

func TestWeightedIndependentGroups(t *testing.T) {
	w := NewWeightedRR()
	if a := w.Next("a", []string{"m1", "m2"}); a != 0 {
		t.Fatalf("first pick should be top-ranked, got %d", a)
	}
	if b := w.Next("b", []string{"m1", "m2"}); b != 0 {
		t.Fatalf("independent group should start fresh, got %d", b)
	}
}

func TestRotateChainWeighted(t *testing.T) {
	w := NewWeightedRR()
	ranked := []string{"best", "mid", "cheap"}
	primaries := map[string]int{}
	for i := 0; i < 6; i++ {
		chain := RotateChain("fast", ranked, w)
		if len(chain) != 3 {
			t.Fatalf("want 3, got %d", len(chain))
		}
		primaries[chain[0]]++
	}
	for _, want := range ranked {
		if primaries[want] == 0 {
			t.Fatalf("%q never became primary: %v", want, primaries)
		}
	}
	if primaries["best"] <= primaries["cheap"] {
		t.Fatalf("best should lead as primary more often: %v", primaries)
	}
}

func TestRotateChainNilOrSingleton(t *testing.T) {
	ranked := []string{"only"}
	if got := RotateChain("g", ranked, nil); len(got) != 1 || got[0] != "only" {
		t.Fatalf("nil balancer should pass through, got %v", got)
	}
	w := NewWeightedRR()
	if got := RotateChain("g", []string{"solo"}, w); got[0] != "solo" {
		t.Fatalf("singleton should pass through, got %v", got)
	}
}
