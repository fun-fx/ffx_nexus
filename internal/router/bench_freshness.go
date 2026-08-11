package router

import (
	"context"
	"math"
	"time"
)

// BenchmarkStatsWithFreshness augments BenchmarkStats with the
// exponential decay weight the router will apply right now for the
// configured half-life. Exposed to the console so an operator can
// see "this row is contributing 38% to the Judge blend weight today"
// without having to compute the formula themselves.
//
// A score's effective contribution follows the half-life curve:
//
//	freshness(t) = 2^(-age / halfLife)
//
// where age is time.Since(CompletedAt). The router uses the same
// formula in blendQuality; exposing it on the snapshot path lets the
// admin console show operators what the router actually sees.
func Freshness(bench BenchmarkStats, halfLife time.Duration, now time.Time) float64 {
	if halfLife <= 0 {
		return 1.0
	}
	age := now.Sub(bench.CompletedAt)
	if age < 0 {
		age = 0
	}
	return math.Exp(-math.Ln2 * age.Hours() / halfLife.Hours())
}

// SnapshotWithFreshness is the view the /api/quality endpoint
// surfaces to operators. It is computed on top of a plain
// BenchmarkSnapshot so production callers who only want the blend
// (the router) keep using the cheaper, smaller struct.
type SnapshotWithFreshness struct {
	Stats      BenchmarkStats
	Freshness  float64 // 0..1: weight the blend applies right now
	HalfLife   time.Duration
	BlendedIn  bool // true when Stats.AvgScore is actually mixed in
	SampleTime time.Time
}

// FreshnessSnapshot is a free function so callers that do not use
// the provider directly (the console, an exporter, a one-off tool)
// still get the same math the router uses. The provider does not
// need to wrap this; only callers that already hold a source do.
func FreshnessSnapshot(ctx context.Context, src BenchmarkScoreSource, halfLife time.Duration) (map[string]SnapshotWithFreshness, error) {
	if src == nil {
		return map[string]SnapshotWithFreshness{}, nil
	}
	if halfLife < 0 {
		halfLife = 0
	}
	bench, err := src.BenchmarkSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make(map[string]SnapshotWithFreshness, len(bench))
	for model, b := range bench {
		out[model] = SnapshotWithFreshness{
			Stats:      b,
			Freshness:  Freshness(b, halfLife, now),
			HalfLife:   halfLife,
			BlendedIn:  false, // caller sets after consulting weights
			SampleTime: now,
		}
	}
	return out, nil
}
