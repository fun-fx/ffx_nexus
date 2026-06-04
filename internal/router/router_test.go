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
		"good": {Model: "good", Quality: 0.95, AvgLatencyMs: 800, AvgCostUSD: 0.01, Samples: 100},
		"bad":  {Model: "bad", Quality: 0.40, AvgLatencyMs: 300, AvgCostUSD: 0.001, Samples: 100},
	}, Weights{Quality: 0.9, Cost: 0.05, Latency: 0.05})

	got, ok := r.Select([]string{"good", "bad"})
	if !ok || got != "good" {
		t.Fatalf("quality-weighted routing should pick 'good', got %q ok=%v", got, ok)
	}
}

func TestSelectPrefersCheapWhenCostWeighted(t *testing.T) {
	r := newTestRouter(map[string]ModelStats{
		"expensive": {Model: "expensive", Quality: 0.80, AvgLatencyMs: 500, AvgCostUSD: 0.10, Samples: 100},
		"cheap":     {Model: "cheap", Quality: 0.78, AvgLatencyMs: 500, AvgCostUSD: 0.001, Samples: 100},
	}, Weights{Quality: 0.1, Cost: 0.8, Latency: 0.1})

	got, _ := r.Select([]string{"expensive", "cheap"})
	if got != "cheap" {
		t.Fatalf("cost-weighted routing should pick 'cheap', got %q", got)
	}
}

func TestSelectSingleAndEmpty(t *testing.T) {
	r := newTestRouter(map[string]ModelStats{}, DefaultWeights())

	if got, ok := r.Select([]string{"only"}); !ok || got != "only" {
		t.Fatalf("single candidate should return itself, got %q ok=%v", got, ok)
	}
	if _, ok := r.Select(nil); ok {
		t.Fatal("empty candidates should return ok=false")
	}
}

func TestSelectExplorationForUnknown(t *testing.T) {
	// "known" is mediocre; "unknown" has no stats and gets optimistic quality,
	// so it should win when quality dominates.
	r := newTestRouter(map[string]ModelStats{
		"known": {Model: "known", Quality: 0.5, AvgLatencyMs: 500, AvgCostUSD: 0.01, Samples: 100},
	}, Weights{Quality: 1, Cost: 0, Latency: 0})

	got, _ := r.Select([]string{"known", "unknown"})
	if got != "unknown" {
		t.Fatalf("unknown model should get exploratory traffic, got %q", got)
	}
}
