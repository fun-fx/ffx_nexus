package router

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

type fakeProvider struct{ stats map[string]ModelStats }

func (f fakeProvider) ModelStats(context.Context, time.Duration) (map[string]ModelStats, error) {
	return f.stats, nil
}

func newTestRouter(stats map[string]ModelStats, w Weights) *Router {
	return New(fakeProvider{stats}, w, time.Hour, 0, slog.Default())
}

func TestSelectPrefersQuality(t *testing.T) {
	r := newTestRouter(map[string]ModelStats{
		"good": {Model: "good", Quality: 0.95, QualitySamples: 50, AvgLatencyMs: 800, AvgCostUSD: 0.01, Samples: 100},
		"bad":  {Model: "bad", Quality: 0.40, QualitySamples: 50, AvgLatencyMs: 300, AvgCostUSD: 0.001, Samples: 100},
	}, Weights{Quality: 0.9, Cost: 0.05, Latency: 0.05})

	got, ok := r.Select([]string{"good", "bad"}, 0)
	if !ok || got != "good" {
		t.Fatalf("quality-weighted routing should pick 'good', got %q ok=%v", got, ok)
	}
}

func TestSelectPrefersCheapWhenCostWeighted(t *testing.T) {
	r := newTestRouter(map[string]ModelStats{
		"expensive": {Model: "expensive", Quality: 0.80, QualitySamples: 50, AvgLatencyMs: 500, AvgCostUSD: 0.10, Samples: 100},
		"cheap":     {Model: "cheap", Quality: 0.78, QualitySamples: 50, AvgLatencyMs: 500, AvgCostUSD: 0.001, Samples: 100},
	}, Weights{Quality: 0.1, Cost: 0.8, Latency: 0.1})

	got, _ := r.Select([]string{"expensive", "cheap"}, 0)
	if got != "cheap" {
		t.Fatalf("cost-weighted routing should pick 'cheap', got %q", got)
	}
}

func TestSelectSingleAndEmpty(t *testing.T) {
	r := newTestRouter(map[string]ModelStats{}, DefaultWeights())

	if got, ok := r.Select([]string{"only"}, 0); !ok || got != "only" {
		t.Fatalf("single candidate should return itself, got %q ok=%v", got, ok)
	}
	if _, ok := r.Select(nil, 0); ok {
		t.Fatal("empty candidates should return ok=false")
	}
}

func TestSelectExplorationForUnknown(t *testing.T) {
	// "known" is mediocre; "unknown" has no stats and gets optimistic quality,
	// so it should win when quality dominates.
	r := newTestRouter(map[string]ModelStats{
		"known": {Model: "known", Quality: 0.5, QualitySamples: 50, AvgLatencyMs: 500, AvgCostUSD: 0.01, Samples: 100},
	}, Weights{Quality: 1, Cost: 0, Latency: 0})

	got, _ := r.Select([]string{"known", "unknown"}, 0)
	if got != "unknown" {
		t.Fatalf("unknown model should get exploratory traffic, got %q", got)
	}
}

func TestSelectMinQualityGate(t *testing.T) {
	// Only "hi" clears the 0.8 bar; "lo" is dropped.
	r := newTestRouter(map[string]ModelStats{
		"hi": {Model: "hi", Quality: 0.90, QualitySamples: 50, AvgLatencyMs: 900, AvgCostUSD: 0.05, Samples: 100},
		"lo": {Model: "lo", Quality: 0.50, QualitySamples: 50, AvgLatencyMs: 100, AvgCostUSD: 0.001, Samples: 100},
	}, Weights{Quality: 0.2, Cost: 0.4, Latency: 0.4}) // cost/latency favor "lo"

	got, ok := r.Select([]string{"hi", "lo"}, 0.8)
	if !ok || got != "hi" {
		t.Fatalf("min-quality gate should force 'hi', got %q ok=%v", got, ok)
	}

	// Nothing clears a 0.99 bar -> no selection.
	if _, ok := r.Select([]string{"hi", "lo"}, 0.99); ok {
		t.Fatal("no candidate should clear 0.99 min quality")
	}
}

func TestEffectiveQualityBlend(t *testing.T) {
	// Judge only.
	if q := effectiveQuality(ModelStats{Quality: 0.8, QualitySamples: 10}); q != 0.8 {
		t.Fatalf("judge-only quality want 0.8, got %v", q)
	}
	// Safety only (no judge) -> heuristic pass rate drives routing.
	if q := effectiveQuality(ModelStats{SafetyPassRate: 0.6, SafetySamples: 10}); q != 0.6 {
		t.Fatalf("safety-only quality want 0.6, got %v", q)
	}
	// Both -> weighted blend.
	q := effectiveQuality(ModelStats{Quality: 1.0, QualitySamples: 10, SafetyPassRate: 0.0, SafetySamples: 10})
	if q != judgeWeight {
		t.Fatalf("blended quality want %v, got %v", judgeWeight, q)
	}
	// Neither -> exploration value.
	if q := effectiveQuality(ModelStats{}); q != explorationQuality {
		t.Fatalf("no-eval quality want %v, got %v", explorationQuality, q)
	}
}

func TestRankOrdersBestFirst(t *testing.T) {
	r := newTestRouter(map[string]ModelStats{
		"top": {Model: "top", Quality: 0.95, QualitySamples: 50, AvgLatencyMs: 500, AvgCostUSD: 0.01, Samples: 100},
		"mid": {Model: "mid", Quality: 0.70, QualitySamples: 50, AvgLatencyMs: 500, AvgCostUSD: 0.01, Samples: 100},
		"low": {Model: "low", Quality: 0.40, QualitySamples: 50, AvgLatencyMs: 500, AvgCostUSD: 0.01, Samples: 100},
	}, Weights{Quality: 1, Cost: 0, Latency: 0})

	ranked := r.Rank([]string{"low", "mid", "top"}, 0)
	want := []string{"top", "mid", "low"}
	if len(ranked) != 3 {
		t.Fatalf("want 3 ranked, got %v", ranked)
	}
	for i := range want {
		if ranked[i] != want[i] {
			t.Fatalf("rank order want %v, got %v", want, ranked)
		}
	}

	// min-quality gate drops "low".
	gated := r.Rank([]string{"low", "mid", "top"}, 0.5)
	if len(gated) != 2 || gated[0] != "top" || gated[1] != "mid" {
		t.Fatalf("gated rank want [top mid], got %v", gated)
	}
}

func TestSelectSafetyFeedsRoutingWithoutJudge(t *testing.T) {
	// No judge data; "clean" has perfect safety, "leaky" fails heuristics.
	// Even though "leaky" is cheaper/faster, safety should win at high quality weight.
	r := newTestRouter(map[string]ModelStats{
		"clean": {Model: "clean", SafetyPassRate: 1.0, SafetySamples: 20, AvgLatencyMs: 800, AvgCostUSD: 0.02, Samples: 50},
		"leaky": {Model: "leaky", SafetyPassRate: 0.2, SafetySamples: 20, AvgLatencyMs: 200, AvgCostUSD: 0.001, Samples: 50},
	}, Weights{Quality: 0.8, Cost: 0.1, Latency: 0.1})

	got, _ := r.Select([]string{"clean", "leaky"}, 0)
	if got != "clean" {
		t.Fatalf("heuristic safety should drive routing to 'clean', got %q", got)
	}
}
