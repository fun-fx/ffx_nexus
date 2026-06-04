// Package router implements quality-aware model selection. Given a group of
// candidate models, it picks the one with the best blend of rolling evaluation
// quality, cost, and latency. Stats come from a StatsProvider (ClickHouse in
// production) and are refreshed periodically in the background so selection is
// a fast in-memory lookup on the request path.
package router

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// ModelStats are rolling per-model metrics over a recent window.
type ModelStats struct {
	Model          string  `json:"model"`
	Quality        float64 `json:"quality"`          // avg judge "quality" score, 0..1
	QualitySamples int64   `json:"quality_samples"`  // number of judge evals
	PassRate       float64 `json:"pass_rate"`        // fraction of judge evals passed
	SafetyPassRate float64 `json:"safety_pass_rate"` // avg heuristic pass (PII/completeness), 0..1
	SafetySamples  int64   `json:"safety_samples"`   // number of heuristic evals
	AvgLatencyMs   float64 `json:"avg_latency_ms"`   // lower is better
	AvgCostUSD     float64 `json:"avg_cost_usd"`     // per-request, lower is better
	Samples        int64   `json:"samples"`          // trace count in window
	EffQuality     float64 `json:"eff_quality"`      // blended routing signal (computed)
}

// StatsProvider supplies rolling per-model stats keyed by model id.
type StatsProvider interface {
	ModelStats(ctx context.Context, window time.Duration) (map[string]ModelStats, error)
}

// Weights control how quality, cost, and latency trade off. They are
// normalized internally, so relative magnitudes are what matter.
type Weights struct {
	Quality float64
	Cost    float64
	Latency float64
}

// DefaultWeights favors quality while still rewarding cheap, fast models.
func DefaultWeights() Weights { return Weights{Quality: 0.6, Cost: 0.2, Latency: 0.2} }

// explorationQuality is the optimistic quality assigned to candidates with no
// observed stats yet, so new/unmeasured models still get routed traffic.
const explorationQuality = 0.75

// Quality-blend weights: how much the LLM-as-judge score vs. heuristic safety
// pass rate contribute to the routing quality signal when both are present.
const (
	judgeWeight  = 0.7
	safetyWeight = 0.3
)

// effectiveQuality blends the judge quality score with the heuristic safety
// pass rate into a single 0..1 routing signal. This lets routing react to
// heuristic evals (PII/completeness) even when the SLM judge is disabled.
// A model with traces but no eval data yet gets the optimistic exploration
// value so it still receives traffic to build up a measurement.
func effectiveQuality(s ModelStats) float64 {
	hasJudge := s.QualitySamples > 0
	hasSafety := s.SafetySamples > 0
	switch {
	case hasJudge && hasSafety:
		return judgeWeight*s.Quality + safetyWeight*s.SafetyPassRate
	case hasJudge:
		return s.Quality
	case hasSafety:
		return s.SafetyPassRate
	default:
		return explorationQuality
	}
}

// Router selects models using cached rolling stats.
type Router struct {
	provider StatsProvider
	weights  Weights
	window   time.Duration
	log      *slog.Logger

	mu    sync.RWMutex
	stats map[string]ModelStats

	done chan struct{}
	wg   sync.WaitGroup
}

// New builds a Router and starts a background refresher. refresh<=0 disables
// periodic refresh (stats then only update via Refresh()).
func New(provider StatsProvider, w Weights, window, refresh time.Duration, log *slog.Logger) *Router {
	if window <= 0 {
		window = time.Hour
	}
	r := &Router{
		provider: provider,
		weights:  normalize(w),
		window:   window,
		log:      log,
		stats:    map[string]ModelStats{},
		done:     make(chan struct{}),
	}
	// Prime synchronously so the first requests have data if available.
	if err := r.Refresh(context.Background()); err != nil {
		log.Warn("initial router stats refresh failed", "err", err)
	}
	if refresh > 0 {
		r.wg.Add(1)
		go r.refreshLoop(refresh)
	}
	return r
}

func (r *Router) refreshLoop(every time.Duration) {
	defer r.wg.Done()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := r.Refresh(ctx); err != nil {
				r.log.Warn("router stats refresh failed", "err", err)
			}
			cancel()
		}
	}
}

// Refresh pulls fresh stats from the provider into the in-memory cache.
func (r *Router) Refresh(ctx context.Context) error {
	if r.provider == nil {
		return nil
	}
	s, err := r.provider.ModelStats(ctx, r.window)
	if err != nil {
		return err
	}
	// Precompute the blended quality signal so the console and Select share one
	// definition.
	for m, st := range s {
		st.EffQuality = effectiveQuality(st)
		s[m] = st
	}
	r.mu.Lock()
	r.stats = s
	r.mu.Unlock()
	return nil
}

// Snapshot returns a copy of the current cached stats (for the console).
func (r *Router) Snapshot() map[string]ModelStats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]ModelStats, len(r.stats))
	for k, v := range r.stats {
		out[k] = v
	}
	return out
}

// Select returns the best candidate model and whether a choice was made.
// It is equivalent to the first element of Rank.
func (r *Router) Select(candidates []string, minQuality float64) (string, bool) {
	ranked := r.Rank(candidates, minQuality)
	if len(ranked) == 0 {
		return "", false
	}
	return ranked[0], true
}

// Rank returns candidates ordered best-first by the composite score (quality,
// cost, latency). Candidates whose blended quality is below minQuality are
// dropped; minQuality <= 0 disables the gate. Candidates without observed stats
// are treated optimistically (exploration) so new models still receive traffic.
// The ordered result enables provider fallback: callers try models in order.
func (r *Router) Rank(candidates []string, minQuality float64) []string {
	if len(candidates) == 0 {
		return nil
	}

	r.mu.RLock()
	stats := r.stats
	r.mu.RUnlock()

	// Build per-candidate metric vectors, filling gaps with exploration values.
	type cand struct {
		model              string
		quality, lat, cost float64
		score              float64
		known              bool
	}
	cs := make([]cand, 0, len(candidates))
	for _, m := range candidates {
		s, ok := stats[m]
		known := ok && s.Samples > 0
		quality := explorationQuality
		if ok {
			quality = effectiveQuality(s)
		}
		// Minimum-quality gate (min_quality_score policy).
		if minQuality > 0 && quality < minQuality {
			continue
		}
		c := cand{model: m, quality: quality, known: known}
		if known {
			c.lat = s.AvgLatencyMs
			c.cost = s.AvgCostUSD
		}
		cs = append(cs, c)
	}
	if len(cs) == 0 {
		return nil
	}
	if len(cs) == 1 {
		return []string{cs[0].model}
	}

	// Latency/cost neutral fill for unknowns = mean of known values.
	meanLat, meanCost, n := 0.0, 0.0, 0.0
	for _, c := range cs {
		if c.known {
			meanLat += c.lat
			meanCost += c.cost
			n++
		}
	}
	if n > 0 {
		meanLat /= n
		meanCost /= n
	}
	for i := range cs {
		if !cs[i].known {
			cs[i].lat = meanLat
			cs[i].cost = meanCost
		}
	}

	// Min-max ranges for normalization (lower latency/cost is better).
	minLat, maxLat := minMax(cs, func(c cand) float64 { return c.lat })
	minCost, maxCost := minMax(cs, func(c cand) float64 { return c.cost })

	for i := range cs {
		latScore := invNorm(cs[i].lat, minLat, maxLat)
		costScore := invNorm(cs[i].cost, minCost, maxCost)
		cs[i].score = r.weights.Quality*cs[i].quality +
			r.weights.Latency*latScore +
			r.weights.Cost*costScore
	}

	sort.SliceStable(cs, func(i, j int) bool { return cs[i].score > cs[j].score })
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.model
	}
	return out
}

// invNorm maps v in [min,max] to [0,1] where the minimum maps to 1 (best) and
// the maximum to 0. A zero range yields 1 (all equal).
func invNorm(v, min, max float64) float64 {
	if max <= min {
		return 1
	}
	return (max - v) / (max - min)
}

func minMax[T any](items []T, f func(T) float64) (min, max float64) {
	if len(items) == 0 {
		return 0, 0
	}
	min, max = f(items[0]), f(items[0])
	for _, it := range items[1:] {
		v := f(it)
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	return min, max
}

func normalize(w Weights) Weights {
	sum := w.Quality + w.Cost + w.Latency
	if sum <= 0 {
		return DefaultWeights()
	}
	return Weights{Quality: w.Quality / sum, Cost: w.Cost / sum, Latency: w.Latency / sum}
}

// Close stops the background refresher.
func (r *Router) Close() {
	close(r.done)
	r.wg.Wait()
}
