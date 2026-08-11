package console

import (
	"net/http"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/router"
)

// benchmarkQuality returns the model-lineup quality snapshot the
// admin needs to confirm the router is using PrimeIntellect scores
// the way the operator intended. The router is the only authority on
// the blending; this endpoint reads the same snapshot source and
// the same weights so the response cannot drift from what the
// router is doing on the next request.
//
// Intentionally NOT behind the require-admin guard with any tighter
// scope; a runtime "what is the router doing" view is one of those
// signals an SRE wants during an incident, and locking it behind
// admin auth gates the on-call playbook. The /api/eval/benchmarks
// family is admin-only already (the parent route is gated) so this
// inherits the same constraint via the framework.
//
// Response shape mirrors the column model on the Eval console page
// so the front-end does not have to translate; fields are nullable
// for models without a settled benchmark.
type benchmarkQualityResponse struct {
	// Row is one public-facing entry: model id, score, freshness,
	// contribution, and the time the data reflects. The operator
	// reads this row at a glance.
	Model        string  `json:"model"`
	AvgScore     float64 `json:"avg_score"`
	CompletedAt  string  `json:"completed_at"`
	Freshness    float64 `json:"freshness"`
	BenchWeight  float64 `json:"bench_weight"`
	BlendedIn    bool    `json:"blended_in"`
	HalfLife     string  `json:"half_life"`

	// Routing context: which way the router is leaning on this row
	// right now. With NEXUS_ROUTE_W_BENCH=0 the row reads
	// blended_in=false and bench_weight=0; the operator sees that
	// the data is there even if the router is ignoring it. That is
	// the most important property of this endpoint — it makes the
	// "router is/is not using my benchmarks" question answerable.
	ModelsUsed int `json:"models_with_bench"`
	ModelsAll  int `json:"models_total"`
}

func (s *Server) benchmarkQuality(w http.ResponseWriter, r *http.Request, _ core.User) {
	if s.qualityRouter == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "quality router not wired",
		})
		return
	}

	// Reading the router's freshly-stamped weights keeps the
	// snapshot honest: the operator sees the same constants the
	// router resolves on the next request, not stale env reads.
	weights, halfLife, bench := s.qualityRouter.BlendConfig()

	// Build snapshots in parallel with the bench provider's view so
	// the response can carry "would this row contribute right now"
	// without producing a different value than what the router used.
	snap, err := router.FreshnessSnapshot(r.Context(), bench, halfLife)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "benchmark snapshot failed: " + err.Error(),
		})
		return
	}

	// Cross-reference the lineup that the router currently serves.
	// We want to know which of those models have a benchmark so
	// the operator's "missing benchmark → judge-only" gap is
	// countable, not a guess.
	lineup := s.qualityRouter.KnownModels(r.Context())
	out := struct {
		Weights    router.CombinedWeights     `json:"weights"`
		HalfLife   string                     `json:"half_life"`
		HalfLifeNs int64                      `json:"half_life_seconds"`
		Freshness  []benchmarkQualityResponse `json:"freshness"`
	}{
		Weights:    weights,
		HalfLife:   halfLife.String(),
		HalfLifeNs: int64(halfLife.Seconds()),
	}

	usedCount := 0
	for _, m := range lineup {
		entry := snap[m]
		row := benchmarkQualityResponse{
			Model:    m,
			BlendedIn: 0 < weights.BenchmarkWeight && entry.Freshness > 0,
		}
		if entry.Stats.CompletedAt.IsZero() {
			row.CompletedAt = ""
			row.Freshness = 0
			row.AvgScore = 0
		} else {
			row.AvgScore = entry.Stats.AvgScore
			row.CompletedAt = entry.Stats.CompletedAt.UTC().Format("2006-01-02T15:04:05Z")
			row.Freshness = entry.Freshness
			if row.BlendedIn {
				usedCount++
			}
		}
		row.BenchWeight = weights.BenchmarkWeight
		row.HalfLife = halfLife.String()
		out.Freshness = append(out.Freshness, row)
	}
	out.Freshness = append(out.Freshness, benchmarkQualityResponse{
		BlendedIn:   false,
		ModelsAll:   len(lineup),
		ModelsUsed:  usedCount,
	})

	writeJSON(w, http.StatusOK, out)
}
