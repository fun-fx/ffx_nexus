package router

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fakeBench is a BenchmarkScoreSource that returns whatever the
// caller wired. The PointInTime struct lets each test simulate a
// benchmark that settled at a known wall-clock time, which is
// what the decay branch needs.
type fakeBench struct {
	row map[string]BenchmarkStats
}

func (f fakeBench) BenchmarkSnapshot(context.Context) (map[string]BenchmarkStats, error) {
	out := map[string]BenchmarkStats{}
	for k, v := range f.row {
		out[k] = v
	}
	return out, nil
}

// TestBlendQualityPureJudgeIsReturnedUnchanged locks down the
// "bench is nil" path: when no benchmark is configured, the
// blended Quality must equal the judge-only Quality — no random
// drift, no caps, no clamps. A regression here would make every
// Nexus cluster return wrong routing signals the moment bench
// env vars are left at default.
func TestBlendQualityPureJudgeIsReturnedUnchanged(t *testing.T) {
	primary := fakeStats{row: map[string]ModelStats{"thegrid": {Model: "thegrid", Quality: 0.8}}}
	c := NewCombinedStatsProvider(primary, nil, CombinedWeights{BenchmarkWeight: 0.5}, 7*24*time.Hour)
	out, err := c.ModelStats(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("ModelStats: %v", err)
	}
	if got := out["thegrid"].Quality; got != 0.8 {
		t.Errorf("Quality without bench: got %v want 0.8", got)
	}
}

// TestBlendQualityBenchmarkDominatesAtWeightOne pins the
// highest-trust deployment: weight=1 means the benchmark is the
// truth, judge is ignored. Freshness=1 (recently settled), so
// the blended Quality must equal the benchmark's avg_score.
func TestBlendQualityBenchmarkDominatesAtWeightOne(t *testing.T) {
	primary := fakeStats{row: map[string]ModelStats{"thegrid": {Model: "thegrid", Quality: 0.2}}}
	bench := fakeBench{row: map[string]BenchmarkStats{
		"thegrid": {AvgScore: 0.95, CompletedAt: time.Now()},
	}}
	c := NewCombinedStatsProvider(primary, bench, CombinedWeights{BenchmarkWeight: 1.0}, 7*24*time.Hour)
	out, err := c.ModelStats(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("ModelStats: %v", err)
	}
	// At w=1.0 + freshness effectively 1.0 (just-completed), the
	// blend's effective bench weight is ~1.0 minus the residual
	// lapse the for-loop's wall-clock read already consumed (a
	// few microseconds). Asserting "approximately the bench score"
	// is the contract that matters: bench dominates the signal.
	if got := out["thegrid"].Quality; got < 0.94 || got > 0.95 {
		t.Errorf("Quality at w=1 (fresh): got %v want ~0.95 (bench dominates)", got)
	}
}

// TestBlendQualityEqualWeightsSplitByHalf at w=0.5 and freshness=1
// the blend must split the difference: judge*0.5 + bench*0.5.
// This is the default-state shape the operator sees on the
// routing dashboard right after enabling the bench blend.
func TestBlendQualityEqualWeightsSplitByHalf(t *testing.T) {
	primary := fakeStats{row: map[string]ModelStats{"thegrid": {Model: "thegrid", Quality: 0.6}}}
	bench := fakeBench{row: map[string]BenchmarkStats{
		"thegrid": {AvgScore: 0.9, CompletedAt: time.Now().Add(-1 * time.Minute)},
	}}
	c := NewCombinedStatsProvider(primary, bench, CombinedWeights{BenchmarkWeight: 0.5}, 7*24*time.Hour)
	out, err := c.ModelStats(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("ModelStats: %v", err)
	}
	// 0.6 * 0.5 + 0.9 * 0.5 = 0.75 exactly
	if got := out["thegrid"].Quality; !approx(got, 0.75) {
		t.Errorf("Quality at w=0.5 (fresh): got %v want 0.75", got)
	}
}

// TestBlendQualityDecaysOverTime pins the half-life branch. A
// benchmark that settled one half-life ago must contribute
// exactly half of its weight to the blend. A regression in the
// decay math would either over-trust very old runs (stale
// model, leadership change) or under-trust them (good model
// rotated out for arbitrary reasons).
func TestBlendQualityDecaysOverTime(t *testing.T) {
	primary := fakeStats{row: map[string]ModelStats{"thegrid": {Model: "thegrid", Quality: 0.5}}}
	bench := fakeBench{row: map[string]BenchmarkStats{
		"thegrid": {AvgScore: 1.0, CompletedAt: time.Now().Add(-7 * 24 * time.Hour)},
	}}
	// 7-day half-life, benchmark settled exactly one half-life
	// ago → freshness = 0.5 → effective bench weight = 0.5 * 0.5 = 0.25.
	// blend = 0.5*0.75 + 1.0*0.25 = 0.625.
	c := NewCombinedStatsProvider(primary, bench, CombinedWeights{BenchmarkWeight: 0.5}, 7*24*time.Hour)
	out, err := c.ModelStats(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("ModelStats: %v", err)
	}
	if got := out["thegrid"].Quality; !approx(got, 0.625) {
		t.Errorf("Quality at w=0.5, t=half-life: got %v want ~0.625", got)
	}
}

// TestBlendQualityDecayDisabledAtZeroHalfLife locks down the
// explicit "no decay" knob. A zero half-life is the operator's
// opt-in to last-known-wins behaviour; the freshness must stay
// at 1.0 regardless of how stale the row is.
func TestBlendQualityDecayDisabledAtZeroHalfLife(t *testing.T) {
	primary := fakeStats{row: map[string]ModelStats{"thegrid": {Model: "thegrid", Quality: 0.4}}}
	bench := fakeBench{row: map[string]BenchmarkStats{
		"thegrid": {AvgScore: 0.8, CompletedAt: time.Now().Add(-365 * 24 * time.Hour)},
	}}
	c := NewCombinedStatsProvider(primary, bench, CombinedWeights{BenchmarkWeight: 0.5}, 0)
	out, err := c.ModelStats(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("ModelStats: %v", err)
	}
	// 0.4*0.5 + 0.8*0.5 = 0.6 exactly
	if got := out["thegrid"].Quality; !approx(got, 0.6) {
		t.Errorf("Quality at w=0.5, half-life=0: got %v want 0.6", got)
	}
}

// TestBlendQualityClampsWeight pins the safety clamp. A bad env
// config (e.g. ROUTE_W_BENCH=2.0 from a typo) MUST be clamped at
// the constructor, not silently produce inverted signals
// downstream.
func TestBlendQualityClampsWeight(t *testing.T) {
	c := NewCombinedStatsProvider(nil, nil, CombinedWeights{BenchmarkWeight: -0.5}, 7*24*time.Hour)
	if c.weights.BenchmarkWeight != 0 {
		t.Errorf("negative weight not clamped: got %v want 0", c.weights.BenchmarkWeight)
	}
	c = NewCombinedStatsProvider(nil, nil, CombinedWeights{BenchmarkWeight: 5.0}, 7*24*time.Hour)
	if c.weights.BenchmarkWeight != 1 {
		t.Errorf("over-unity weight not clamped: got %v want 1", c.weights.BenchmarkWeight)
	}
}

// TestBlendQualityOnlyForModelsInBench asserts the pass-through
// behaviour for models that have no benchmark row: judge wins
// (because bench has no signal to add). Otherwise a single stale
// benchmark for one model would corrupt every other model's
// routing signal — an alarming failure mode.
func TestBlendQualityOnlyForModelsInBench(t *testing.T) {
	primary := fakeStats{row: map[string]ModelStats{
		"thegrid": {Model: "thegrid", Quality: 0.7},
		"other":   {Model: "other", Quality: 0.55},
	}}
	bench := fakeBench{row: map[string]BenchmarkStats{
		"thegrid": {AvgScore: 0.95, CompletedAt: time.Now().Add(-1 * time.Minute)},
		// 'other' deliberately absent
	}}
	c := NewCombinedStatsProvider(primary, bench, CombinedWeights{BenchmarkWeight: 0.5}, 7*24*time.Hour)
	out, err := c.ModelStats(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("ModelStats: %v", err)
	}
	if got := out["other"].Quality; got != 0.55 {
		t.Errorf("model without bench: got %v want 0.55 (judge pass-through)", got)
	}
	if got := out["thegrid"].Quality; !approx(got, 0.825) {
		t.Errorf("model with bench: got %v want ~0.825", got)
	}
}

// TestBlendQualityBenchFailureFallsBackToJudge pins the
// availability contract: a failing BenchmarkSnapshot MUST NOT
// blank out the routing signal. We model the failure with a
// sentinel-throwing source and assert the judge-only Quality
// comes through.
func TestBlendQualityBenchFailureFallsBackToJudge(t *testing.T) {
	primary := fakeStats{row: map[string]ModelStats{"thegrid": {Model: "thegrid", Quality: 0.6}}}
	c := NewCombinedStatsProvider(primary, errBench{}, CombinedWeights{BenchmarkWeight: 0.5}, 7*24*time.Hour)
	out, err := c.ModelStats(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("ModelStats: %v", err)
	}
	if got := out["thegrid"].Quality; got != 0.6 {
		t.Errorf("bench failure: got %v want 0.6 (judge fallback)", got)
	}
}

// TestBlendQualitySamplesAreCountedHonestly pins the
// QualitySamples increment: when blend happens, the row's
// QualitySamples MUST go up by one. Operators see this number on
// the routing dashboard and use it to gauge confidence — a
// regression that hides the bench contribution would make a
// "model with bench" indistinguishable from "model with one
// judge sample", which is misleading.
func TestBlendQualitySamplesAreCountedHonestly(t *testing.T) {
	primary := fakeStats{row: map[string]ModelStats{
		"judge-only": {Model: "judge-only", Quality: 0.5, QualitySamples: 4},
		"blended":    {Model: "blended", Quality: 0.5, QualitySamples: 7},
	}}
	bench := fakeBench{row: map[string]BenchmarkStats{
		"blended": {AvgScore: 0.5, CompletedAt: time.Now().Add(-1 * time.Minute)},
	}}
	c := NewCombinedStatsProvider(primary, bench, CombinedWeights{BenchmarkWeight: 0.5}, 7*24*time.Hour)
	out, err := c.ModelStats(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("ModelStats: %v", err)
	}
	if out["judge-only"].QualitySamples != 4 {
		t.Errorf("judge-only QualitySamples: got %d want 4 (unchanged)", out["judge-only"].QualitySamples)
	}
	if out["blended"].QualitySamples != 8 {
		t.Errorf("blended QualitySamples: got %d want 8 (incremented)", out["blended"].QualitySamples)
	}
}

// TestBlendQualityNilPrimaryReturnsEmpty pins the constructor
// side: providing a nil primary is a misconfiguration but should
// not crash. The empty map is the safest fallback so the Router
// receives valid Stats.
func TestBlendQualityNilPrimaryReturnsEmpty(t *testing.T) {
	c := NewCombinedStatsProvider(nil, nil, CombinedWeights{}, 7*24*time.Hour)
	out, err := c.ModelStats(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("ModelStats: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("nil primary: got %d entries want 0", len(out))
	}
}

// TestBlendQualityJSONEncodesCleanly makes sure the field shape
// of ModelStats round-trips through encoding/json with the same
// field names the console expects. A regression here would
// silently rename (e.g. snake -> camel) and break the dashboard.
func TestBlendQualityJSONEncodesCleanly(t *testing.T) {
	s := ModelStats{
		Model:          "thegrid",
		Quality:        0.91,
		QualitySamples: 14,
		PassRate:       0.86,
		SafetyPassRate: 0.98,
		SafetySamples:  51,
		AvgLatencyMs:   220,
		AvgCostUSD:     0.0009,
		Samples:        200,
		EffQuality:     0.91,
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{
		`"quality":0.91`,
		`"quality_samples":14`,
		`"avg_latency_ms":220`,
		`"avg_cost_usd":0.0009`,
		`"eff_quality":0.91`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON shape missing %q in %s", want, string(b))
		}
	}
}

// fakeStats is an in-memory StatsProvider for tests that only
// care about Quality.
type fakeStats struct {
	row map[string]ModelStats
}

func (f fakeStats) ModelStats(_ context.Context, _ time.Duration) (map[string]ModelStats, error) {
	out := map[string]ModelStats{}
	for k, v := range f.row {
		out[k] = v
	}
	return out, nil
}

// errBench is a BenchmarkScoreSource that always errors. Used by
// the availability test.
type errBench struct{}

func (errBench) BenchmarkSnapshot(context.Context) (map[string]BenchmarkStats, error) {
	return nil, context.DeadlineExceeded
}

// approx compares floats within a small tolerance — the decay
// math uses time.Now() under mock benches, so we accept any
// result within 0.001 of the expected.
func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-3
}
