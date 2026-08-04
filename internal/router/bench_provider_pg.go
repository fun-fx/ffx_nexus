package router

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PGBenchProvider reads the latest settled benchmark score per
// model from Postgres. Settled = status = 'completed' AND
// avg_score IS NOT NULL. Most-recent-settled wins: if a model has
// many rows, only the row with the greatest completed_at is
// returned. This is the smallest possible query — operators can
// always re-run a benchmark and supersede the prior result without
// having to delete history.
type PGBenchProvider struct {
	pool *pgxpool.Pool
}

func NewPGBenchProvider(pool *pgxpool.Pool) *PGBenchProvider {
	return &PGBenchProvider{pool: pool}
}

// BenchmarkSnapshot runs DISTINCT ON to pick the most recent
// completed_at row per model. AVG(avg_score) is not used: a row's
// avg_score is the platform's aggregate report for one run, and
// the row itself is the unit — averaging aggregates would double-
// count work the platform already did.
func (p *PGBenchProvider) BenchmarkSnapshot(ctx context.Context) (map[string]BenchmarkStats, error) {
	if p.pool == nil {
		return map[string]BenchmarkStats{}, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT DISTINCT ON (model)
		       model,
		       avg_score,
		       completed_at
		FROM benchmark_runs
		WHERE status = 'completed'
		  AND avg_score IS NOT NULL
		  AND completed_at IS NOT NULL
		ORDER BY model, completed_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]BenchmarkStats{}
	for rows.Next() {
		var (
			model string
			avg   float64
			done  time.Time
		)
		if err := rows.Scan(&model, &avg, &done); err != nil {
			return nil, err
		}
		out[model] = BenchmarkStats{
			AvgScore:    avg,
			CompletedAt: done,
		}
	}
	return out, rows.Err()
}
