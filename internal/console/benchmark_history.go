package console

import (
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/router"
)

// benchmarkHistory returns the chronological view of one model's
// settled runs. The shape mirrors what a CI dashboard expects:
// per-row avg/min/max/samples so a trending chart can be drawn
// from a single response, plus a simple "previous delta" column
// to make the slope visible without a follow-up call.
//
// The operator calls this when "this model is ranking poorly
// recently, did something regress?" reaches their desk. The
// response either confirms a regression or shows a stable line
// they can put in front of an exec.
type benchmarkHistoryResponse struct {
	Model       string                     `json:"model"`
	GeneratedAt time.Time                  `json:"generated_at"`
	HalfLife    string                     `json:"half_life"`
	Weights     router.CombinedWeights     `json:"weights"`
	Rows        []benchmarkHistoryEntry    `json:"rows"`
	Trend       benchmarkHistoryTrend      `json:"trend"`
}

type benchmarkHistoryEntry struct {
	RunID        string    `json:"run_id"`
	AvgScore     float64   `json:"avg_score"`
	MinScore     float64   `json:"min_score"`
	MaxScore     float64   `json:"max_score"`
	CompletedAt  time.Time `json:"completed_at"`
	TotalSamples int       `json:"total_samples"`
	DeltaPrev    float64   `json:"delta_prev"`
	// FreshnessWeight is the contribution weight the router applies
	// to this row right now. Useful for triage: a 0.95 weight on a
	// row at the head means the router is reading this row hard;
	// successive rows drop off exponentially with the half-life.
	FreshnessWeight float64 `json:"freshness_weight"`
}

type benchmarkHistoryTrend struct {
	Direction string  `json:"direction"` // "up" | "down" | "flat"
	AvgLast5  float64 `json:"avg_last_5"`
	AvgPrev5  float64 `json:"avg_prev_5"`
	Delta     float64 `json:"delta"`
}

const benchmarkHistoryLimitDefault = 50

func (s *Server) benchmarkHistory(w http.ResponseWriter, r *http.Request, _ core.User) {
	if s.benchmarks == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "benchmarks not configured",
		})
		return
	}
	model := chi.URLParam(r, "model")
	if model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "model is required",
		})
		return
	}
	limit := benchmarkHistoryLimitDefault
	rows, err := s.benchmarks.ListRecentSettledByModel(r.Context(), model, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "history query failed: " + err.Error(),
		})
		return
	}

	// Sort newest-first so the front-end sees the same order the
	// router does; the trend calculation keeps the column order
	// independent.
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].CompletedAt.After(*rows[j].CompletedAt)
	})
	weights := router.CombinedWeights{BenchmarkWeight: 0}
	halfLife := time.Duration(0)
	if s.qualityRouter != nil {
		w, hl, _ := s.qualityRouter.BlendConfig()
		weights, halfLife = w, hl
	}
	now := time.Now().UTC()

	resp := benchmarkHistoryResponse{
		Model:       model,
		GeneratedAt: now,
		HalfLife:    halfLife.String(),
		Weights:     weights,
		Rows:        make([]benchmarkHistoryEntry, 0, len(rows)),
	}
	prev := 0.0
	for i, row := range rows {
		entry := benchmarkHistoryEntry{
			RunID:        row.ID,
			AvgScore:     dereqFloatPtr(row.AvgScore),
			MinScore:     dereqFloatPtr(row.MinScore),
			MaxScore:     dereqFloatPtr(row.MaxScore),
			TotalSamples: dereqIntPtr(row.TotalSamples),
			CompletedAt:  dereqTimePtr(row.CompletedAt),
		}
		if i > 0 {
			entry.DeltaPrev = entry.AvgScore - prev
		}
		prev = entry.AvgScore
		entry.FreshnessWeight = router.Freshness(router.BenchmarkStats{
			AvgScore:    entry.AvgScore,
			CompletedAt: entry.CompletedAt,
		}, halfLife, now)
		resp.Rows = append(resp.Rows, entry)
	}

	// Trend calculation: average the most recent 5 and the previous 5.
	// Empty slots are skipped silently — a brand-new model shows
	// zero windows, not 50% of nothing.
	resp.Trend = benchmarkHistoryTrend{Direction: "flat"}
	last, prevWindow := splitWindows(resp.Rows)
	if len(last) > 0 {
		resp.Trend.AvgLast5 = mean(last)
	}
	if len(prevWindow) > 0 {
		resp.Trend.AvgPrev5 = mean(prevWindow)
	}
	resp.Trend.Delta = resp.Trend.AvgLast5 - resp.Trend.AvgPrev5
	switch {
	case resp.Trend.Delta > 0.01:
		resp.Trend.Direction = "up"
	case resp.Trend.Delta < -0.01:
		resp.Trend.Direction = "down"
	}
	writeJSON(w, http.StatusOK, resp)
}

func splitWindows(rows []benchmarkHistoryEntry) (last, prev []float64) {
	for i, r := range rows {
		if i < 5 {
			last = append(last, r.AvgScore)
			continue
		}
		if i < 10 {
			prev = append(prev, r.AvgScore)
		}
	}
	return
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func dereqTimePtr(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}

// ensure core import is alive; without this `patterned` Go would
// trip on a missing reference in CI.
var _ = core.BenchmarkRun{}
