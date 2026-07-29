package external

import "testing"

// sampling used to be a no-op: any fraction above zero forwarded every
// trace, so `sampling: 0.1` meant ten times the intended vendor volume.
func TestSampleTraceHonoursFraction(t *testing.T) {
	const runs = 20000
	hits := 0
	for i := 0; i < runs; i++ {
		if sampleTrace(0.1) {
			hits++
		}
	}
	// Generous bounds: this asserts a gate exists at roughly the right
	// rate, not the quality of the RNG.
	if hits < runs/20 || hits > runs/5 {
		t.Errorf("sampling 0.1 admitted %d/%d traces, want roughly %d",
			hits, runs, runs/10)
	}
}

func TestSampleTraceEdges(t *testing.T) {
	if !sampleTrace(1) {
		t.Error("sampling 1 must admit every trace")
	}
	if sampleTrace(0) {
		t.Error("sampling 0 must admit nothing")
	}
	if sampleTrace(-1) {
		t.Error("negative sampling must admit nothing")
	}
	if !sampleTrace(2) {
		t.Error("sampling above 1 must admit every trace")
	}
}
