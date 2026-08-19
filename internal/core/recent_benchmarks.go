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
//
// orgID scopes the result to one tenant. A benchmark score is not shared
// operational health the way provider latency is: the run used the tenant's own
// spec, dataset and provider key, so its average, its sample count and its run
// id are that tenant's data. Filtering on model alone returned every org's runs
// for any model name a caller could type, which also handed out run ids
// belonging to other tenants.
//
// Empty orgID means no tenant filter, and is for installation-wide consumers —
// the drift watcher and the router's quality snapshot, which make routing
// decisions rather than answering a request. Request-serving callers must pass a
// concrete org.
func (s *Store) ListRecentSettledByModel(ctx context.Context, orgID, model string, limit int) ([]RecentBenchmarkRun, error) {
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
	// A run recorded before org attribution existed has org_id ''. Treat it as
	// the default org's rather than as everyone's — the same rule
	// console.ownedBenchmark applies to a single run, so a row is not visible
	// through the history endpoint that would be refused by the detail one.
	const orgScope = ` AND ($3 = '' OR COALESCE(NULLIF(org_id, ''), 'default') = $3)`
	rows, err := s.pool.Query(ctx, `
		SELECT id, model, avg_score, min_score, max_score, total_samples, completed_at
		FROM benchmark_runs
		WHERE model = $1 AND status = 'completed'
		  AND avg_score IS NOT NULL
		  AND completed_at IS NOT NULL`+orgScope+`
		ORDER BY completed_at DESC LIMIT $2`, model, limit, orgID)
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
