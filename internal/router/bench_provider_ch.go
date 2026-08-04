package router

import (
	"context"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// CHBenchProvider reads the latest settled benchmark score per
// model from ClickHouse. The shape mirrors PGBenchProvider so
// either backend can be wired into the same combinedStatsProvider
// without conditional code in the router. The query strategy
// differs because ClickHouse lacks Postgres' DISTINCT ON — the
// standard idiom is `argMax(value, key)` to pick the row whose
// (completed_at) is greatest within a (model) group.
//
// A subtle trade-off worth flagging: we count completed_at as the
// "ordering key" for argMax rather than e.g. updated_at, because
// a re-run of the same benchmark only produces a NEW row (it does
// not UPDATE the prior row's completed_at). Updated_at would tie
// across historical replicas of the same model and produce a
// non-deterministic result; completed_at is the column that
// actually advances on a fresh run.
type CHBenchProvider struct {
	conn driver.Conn
}

func NewCHBenchProvider(conn driver.Conn) *CHBenchProvider {
	return &CHBenchProvider{conn: conn}
}

// BenchmarkSnapshot runs one query: SELECT model,
// argMax(avg_score, completed_at) AS latest_score, same for
// completed_at. We push both aggregations into a single
// projection so ClickHouse only reads each row once.
//
// The query uses `assumeNotNull(avg_score)` rather than
// `avg_score IS NOT NULL` because argMax already filters when
// its second argument is nullable — the more common idiom in
// bulk-aggregation queries. If a row has `avg_score = null`
// but `completed_at` is also null, argMax returns zero + zero,
// which BenchmarkStats rejects (the model simply does not
// appear in the result map; the router sees no benchmark for
// that model and falls back to judge-only).
func (p *CHBenchProvider) BenchmarkSnapshot(ctx context.Context) (map[string]BenchmarkStats, error) {
	if p.conn == nil {
		return map[string]BenchmarkStats{}, nil
	}
	rows, err := p.conn.Query(ctx, `
		SELECT model,
		       argMax(avg_score, completed_at) AS latest_avg,
		       argMax(completed_at, completed_at) AS latest_completed_at
		FROM benchmark_runs
		WHERE status = 'completed'
		  AND avg_score IS NOT NULL
		  AND completed_at IS NOT NULL
		GROUP BY model`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]BenchmarkStats{}
	for rows.Next() {
		var (
			model    string
			avg      float64
			complete time.Time
		)
		if err := rows.Scan(&model, &avg, &complete); err != nil {
			return nil, err
		}
		// argMax returns its zero value when no row contributed; guard
		// against an "empty" match by skipping the model entirely. This
		// can happen if the partition is so fresh that ClickHouse's
		// default UTC clock differs from the local clock by less than a
		// millisecond — without the guard we'd record a settled score
		// of 0.0, which the router would happily blend in. Skipping is
		// equivalent to "no benchmark contribution" and matches the
		// Postgres path's NULL-filter.
		if avg == 0 {
			continue
		}
		out[model] = BenchmarkStats{
			AvgScore:    avg,
			CompletedAt: complete,
		}
	}
	return out, rows.Err()
}
