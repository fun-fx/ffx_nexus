package router

import (
	"context"
	"math"
	"time"
)

// BenchmarkStats carries the aggregate quality from a single
// benchmark run for one model. We deliberately keep the slot
// optional: a model with no recent benchmark run simply has no
// BenchQuality contribution, and the weighted blend falls back to
// the judge-only signal.
//
// CompletedAt is the wall-clock time the benchmark settled. The
// blend uses it to compute decay — a 14-day-old score is half as
// influential as a fresh one — so the provider that returns these
// MUST use the row's completed_at, not updated_at or created_at.
type BenchmarkStats struct {
	AvgScore    float64
	CompletedAt time.Time
}

// BenchmarkScoreSource supplies the latest *settled* benchmark score
// per model. Implementations are expected to be safe for concurrent
// use. A nil source disables the bench-blend path entirely (the
// CombinedStatsProvider checks for nil and returns unblended stats).
type BenchmarkScoreSource interface {
	// BenchmarkSnapshot returns the most recent settled benchmark
	// quality per model. Models without a settled run are absent
	// from the map. The snapshot is a point-in-time view; the
	// caller may cache it briefly.
	BenchmarkSnapshot(ctx context.Context) (map[string]BenchmarkStats, error)
}

// CombinedWeights controls how benchmark scores mix with judge
// scores.
//
// BenchmarkWeight is clamped to [0,1]:
//
//	1.0 — judge scores are ignored for any model with a benchmark;
//	      the benchmark is treated as ground truth (high-trust
//	      deployment).
//	0.5 — equal influence (default).
//	0.0 — benchmarks are ignored entirely; judge wins.
//
// JudgeWeight is implicit (1 - BenchmarkWeight) and intentionally
// not exposed: exposing it would invite configs that do not sum
// to 1, which the router would then have to detect and refuse.
type CombinedWeights struct {
	BenchmarkWeight float64
}

// combinedStatsProvider blends a StatsProvider (judge / heuristic /
// trace aggregates) with a BenchmarkScoreSource (external model
// benchmarks). The blend is computed here so the Router stays
// oblivious — it still receives a map[string]ModelStats and uses
// ModelStats.Quality as always; the only difference is that
// Quality is now a decay-weighted blend when a benchmark exists
// for the model.
//
// This is the smallest viable diff: we don't change Router or any
// caller's interface, we just hand them pre-blended numbers.
//
// Exported as CombinedProvider so cmd/nexus (the wiring layer) can
// reach its getters without an awkward type assertion at the call
// site. The internal field names stay lowercase because they are
// protected by the methods; callers should always read via
// Weights() / HalfLife() / BenchSource().
type CombinedProvider struct {
	primary       StatsProvider
	bench         BenchmarkScoreSource
	weights       CombinedWeights
	decayHalfLife time.Duration
}

// NewCombinedStatsProvider builds the wrapper. Weights are clamped
// to [0,1] so a misconfiguration cannot produce nonsense (a
// negative weight would invert signals, which would be very
// confusing in dashboards). A nil bench disables the blend.
func NewCombinedStatsProvider(primary StatsProvider, bench BenchmarkScoreSource, w CombinedWeights, decayHalfLife time.Duration) *CombinedProvider {
	if w.BenchmarkWeight < 0 {
		w.BenchmarkWeight = 0
	}
	if w.BenchmarkWeight > 1 {
		w.BenchmarkWeight = 1
	}
	return &CombinedProvider{
		primary:       primary,
		bench:         bench,
		weights:       w,
		decayHalfLife: decayHalfLife,
	}
}

// BenchSource returns the wrapped BenchmarkScoreSource so callers
// outside the router (the console's /api/quality handler, an
// exporter) can stay in sync with what the router reads. Returning
// the underlying field directly is fine because the field is
// already concurrency-safe per the BenchmarkScoreSource contract.
func (c *CombinedProvider) BenchSource() BenchmarkScoreSource { return c.bench }

// Weights returns the clamped weights the provider is using. Useful
// for operator-facing endpoints that want to render the same
// numbers the router resolves on each request without reloading
// env vars.
func (c *CombinedProvider) Weights() CombinedWeights { return c.weights }

// HalfLife returns the configured decay half-life. A zero value
// means decay is disabled (a benchmark's influence does not wane).
func (c *CombinedProvider) HalfLife() time.Duration { return c.decayHalfLife }

// ModelStats fuses the two sources. If bench is nil, returns
// unblended. If bench returns no row for a model, that model
// passes through untouched (judge wins by default). If both
// sources have a row, Quality is replaced with the decay-weighted
// blend; QualitySamples is incremented by one so a model with
// both judges and a benchmark signals it has more data than
// either alone.
func (c *CombinedProvider) ModelStats(ctx context.Context, window time.Duration) (map[string]ModelStats, error) {
	if c.primary == nil {
		return map[string]ModelStats{}, nil
	}
	stats, err := c.primary.ModelStats(ctx, window)
	if err != nil {
		return nil, err
	}
	if c.bench == nil || c.weights.BenchmarkWeight == 0 {
		return stats, nil
	}
	bench, err := c.bench.BenchmarkSnapshot(ctx)
	if err != nil {
		// A failing bench query must not blank out the judge
		// signal. Returning unblended stats keeps availability;
		// the caller (cmd/nexus/main.go) attaches a slog and a
		// warn-level log if it wants visibility.
		return stats, nil
	}
	for model, b := range bench {
		s := stats[model]
		s.Quality = blendQuality(s.Quality, b, c.weights.BenchmarkWeight, c.decayHalfLife)
		if s.QualitySamples > 0 {
			s.QualitySamples++
		} else {
			s.QualitySamples = 1
		}
		stats[model] = s
	}
	return stats, nil
}

// blendQuality is the per-model core of the blend math. Exposed
// at package scope so tests can pin the formula.
//
// judge   : StatsProvider's rolling mean (already 0..1).
// bench   : aggregate, with CompletedAt for decay.
// wB      : Benchmark weight in [0,1].
// halfLife: time after which benchmark influence is half what it
//
//	was at completion. Zero disables decay (always 1.0).
//
// Decay: an exponential with the given half-life. Fresh (t == 0)
//
//	→ 1.0; one half-life later → 0.5; two half-lives → 0.25.
//
// Blend:
//
//	freshness := 2^(-age / halfLife)
//	effectiveWBench := wB * freshness
//	blended := judge * (1 - effectiveWBench) + bench.AvgScore * effectiveWBench
func blendQuality(judge float64, bench BenchmarkStats, wB float64, halfLife time.Duration) float64 {
	if wB <= 0 {
		return judge
	}
	freshness := 1.0
	if halfLife > 0 {
		age := time.Since(bench.CompletedAt)
		if age < 0 {
			age = 0
		}
		freshness = math.Exp(-math.Ln2 * age.Hours() / halfLife.Hours())
	}
	wBench := wB * freshness
	if wBench > 1 {
		wBench = 1
	}
	return judge*(1-wBench) + bench.AvgScore*wBench
}
