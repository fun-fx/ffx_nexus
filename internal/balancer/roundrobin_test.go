package balancer

import "testing"

func TestRoundRobinCycles(t *testing.T) {
	rr := NewRoundRobin()
	seen := map[int]bool{}
	for i := 0; i < 6; i++ {
		seen[rr.Next("g", 3)] = true
	}
	if len(seen) != 3 {
		t.Fatalf("expected all 3 indices over 6 calls, got %v", seen)
	}
}

func TestRoundRobinIndependentGroups(t *testing.T) {
	rr := NewRoundRobin()
	if a, b := rr.Next("a", 2), rr.Next("b", 2); a != 0 || b != 0 {
		t.Fatalf("each group starts at 0, got a=%d b=%d", a, b)
	}
	if rr.Next("a", 2) != 1 {
		t.Fatal("group a should advance independently")
	}
}

func TestRotateChain(t *testing.T) {
	rr := NewRoundRobin()
	ranked := []string{"best", "mid", "cheap"}
	var primaries []string
	for i := 0; i < 6; i++ {
		chain := RotateChain("fast", ranked, rr)
		if len(chain) != 3 {
			t.Fatalf("want 3, got %d", len(chain))
		}
		primaries = append(primaries, chain[0])
	}
	// Each model should have been primary at least once over 6 rotations of 3.
	for _, want := range ranked {
		found := false
		for _, p := range primaries {
			if p == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%q never became primary: %v", want, primaries)
		}
	}
}

func TestRotateChainNilOrSingleton(t *testing.T) {
	ranked := []string{"only"}
	if got := RotateChain("g", ranked, nil); len(got) != 1 || got[0] != "only" {
		t.Fatalf("nil balancer should pass through, got %v", got)
	}
}
