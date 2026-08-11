package console

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/router"
)

// benchmarkLeaderboard pairs the router-lineup with the most-recent
// settled benchmark per model. The response is intentionally a flat
// list rather than two maps because the operator reads it as a
// table: every row tells the same story, and a table makes
// "champion / contender / gap" visible at a glance.
//
// Models are sorted by the decay-applied effective weight
// descending; ties broken by completed_at desc. That order answers
// "which model is the router leaning on hardest right now" with
// the same numbers the router uses, so an operator's question about
// the leaderboard never drifts from the answer.
type benchmarkLeaderboardResponse struct {
	GeneratedAt time.Time                `json:"generated_at"`
	Weights     router.CombinedWeights    `json:"weights"`
	HalfLife    string                    `json:"half_life"`
	Rows        []benchmarkLeaderboardRow `json:"rows"`
}

// benchmarkLeaderboardRow is one entry on the table. AvgScore is
// the provider-reported aggregate; Freshness is the decay weight the
// router will multiply it by; Effective is the product, kept on
// the row to avoid a second multiplication in the front-end.
type benchmarkLeaderboardRow struct {
	Model         string    `json:"model"`
	LatestRunID   string    `json:"latest_run_id"`
	AvgScore      float64   `json:"avg_score"`
	MinScore      float64   `json:"min_score"`
	MaxScore      float64   `json:"max_score"`
	CompletedAt   time.Time `json:"completed_at"`
	TotalSamples  int       `json:"total_samples"`
	Freshness     float64   `json:"freshness"`
	Effective     float64   `json:"effective"`
	BlendedIn     bool      `json:"blended_in"`
	StaleAgeDays  float64   `json:"stale_age_days"`
}

func (s *Server) benchmarkLeaderboard(w http.ResponseWriter, r *http.Request, _ core.User) {
	if s.benchmarks == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "benchmarks not configured",
		})
		return
	}
	if s.qualityRouter == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "quality router not wired",
		})
		return
	}

	weights, halfLife, src := s.qualityRouter.BlendConfig()
	lineup := s.qualityRouter.KnownModels(r.Context())
	if len(lineup) == 0 {
		writeJSON(w, http.StatusOK, benchmarkLeaderboardResponse{
			GeneratedAt: time.Now().UTC(),
			Weights:     weights,
			HalfLife:    halfLife.String(),
			Rows:        []benchmarkLeaderboardRow{},
		})
		return
	}

	// Pull per-model snapshots so we can carry min / max / sample
	// counts alongside the average. The store's read is a single
	// query per pull but the size is bounded by the router's
	// lineup; a deployment with a hundred models pays one
	// round-trip per pull, which is acceptable for an admin endpoint.
	rows, err := src.BenchmarkSnapshot(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "benchmark snapshot failed: " + err.Error(),
		})
		return
	}

	// Joined view: each lineup member gets a row regardless of
	// whether the provider has produced a settled value. Absent
	// rows render with score=0 and effective=0 so the operator sees
	// the gap, not a phantom. Adding — let alone running — a
	// benchmark is the action item.
	now := time.Now().UTC()
	out := benchmarkLeaderboardResponse{
		GeneratedAt: now,
		Weights:     weights,
		HalfLife:    halfLife.String(),
		Rows:        make([]benchmarkLeaderboardRow, 0, len(lineup)),
	}
	for _, m := range lineup {
		stat, ok := rows[m]
		if !ok {
			out.Rows = append(out.Rows, benchmarkLeaderboardRow{Model: m})
			continue
		}
		fresh := router.Freshness(stat, halfLife, now)
		row := benchmarkLeaderboardRow{
			Model:       m,
			LatestRunID: latestRunFor(s, m),
			AvgScore:    stat.AvgScore,
			CompletedAt: stat.CompletedAt,
			Freshness:   fresh,
			Effective:   stat.AvgScore * fresh,
			BlendedIn:   weights.BenchmarkWeight > 0 && fresh > 0,
			StaleAgeDays: now.Sub(stat.CompletedAt).Hours() / 24,
		}
		if s.benchmarks != nil {
			run, rerr := s.benchmarks.GetLatestSettledByModel(r.Context(), m)
			if rerr == nil {
				row.MinScore = dereqFloatPtr(run.MinScore)
				row.MaxScore = dereqFloatPtr(run.MaxScore)
				row.TotalSamples = dereqIntPtr(run.TotalSamples)
				row.LatestRunID = run.ID
			}
		}
		out.Rows = append(out.Rows, row)
	}
	sort.Slice(out.Rows, func(i, j int) bool {
		if out.Rows[i].Effective != out.Rows[j].Effective {
			return out.Rows[i].Effective > out.Rows[j].Effective
		}
		return out.Rows[i].CompletedAt.After(out.Rows[j].CompletedAt)
	})

	writeJSON(w, http.StatusOK, out)
}

// latestRunFor returns the most recent settled run id for one
// model. Pulled through the StatisticsStore so the leaderboard
// view and the row detail share the same data path. Errors are
// swallowed: a missing run renders as "" in the response, which is
// the operator's expected state when the row just settled.
func latestRunFor(s *Server, model string) string {
	if s == nil || s.benchmarks == nil {
		return ""
	}
	run, err := s.benchmarks.GetLatestSettledByModel(context.Background(), model)
	if err != nil {
		return ""
	}
	return run.ID
}

func dereqFloatPtr(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func dereqIntPtr(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
