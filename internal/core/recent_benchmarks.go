package core

import (
	"context"
	"errors"
	"time"
)

// RecentBenchmarkRun is the slice of BenchmarkRun the drift watcher
// and the history endpoint read. Same fields as BenchmarkRun minus
// the lifecycle and scheduling bookkeeping those callers do not
// consult.
//
// Keeping MinScore, MaxScore and TotalSamples on this struct means
// the leaderboard and the trend endpoints do not have to do a
// second store round-trip per row; that round-trip cost grows
// linearly with the operator's lineup.
type RecentBenchmarkRun struct {
	ID           string
	Model        string
	AvgScore     *float64
	MinScore     *float64
	MaxScore     *float64
	TotalSamples *int
	CompletedAt  *time.Time
}

// ListRecentSettledByModel returns the most-recent settled runs for
// a model in completed_at DESC order. limit values <= 0 fall back to
// 5; values > 200 are clamped to 200. The query is the same shape
// PGBenchProvider.BenchmarkSnapshot uses, but the watcher needs
// more than one row per model (one to compare, one to set the
// baseline).
func (s *Store) ListRecentSettledByModel(ctx context.Context, model string, limit int) ([]RecentBenchmarkRun, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("core: store not configured")
	}
	if model == "" {
		return nil, errors.New("core: model is required")
	}
	if limit <= 0 {
		limit = 5
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, model, avg_score, min_score, max_score, total_samples, completed_at
		FROM benchmark_runs
		WHERE model = $1 AND status = 'completed'
		  AND avg_score IS NOT NULL
		  AND completed_at IS NOT NULL
		ORDER BY completed_at DESC LIMIT $2`, model, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RecentBenchmarkRun{}
	for rows.Next() {
		var r RecentBenchmarkRun
		if err := rows.Scan(&r.ID, &r.Model, &r.AvgScore, &r.MinScore, &r.MaxScore, &r.TotalSamples, &r.CompletedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
