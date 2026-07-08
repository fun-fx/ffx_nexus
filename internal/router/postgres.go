package router

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStatsProvider reads rolling per-model stats from Postgres eval_scores.
// Without gateway_traces in Postgres, latency/cost stay zero and routing leans
// on eval quality and heuristic safety pass rates.
type PGStatsProvider struct {
	pool *pgxpool.Pool
}

// NewPGStatsProvider builds a provider reusing the control-plane pool.
func NewPGStatsProvider(pool *pgxpool.Pool) *PGStatsProvider {
	return &PGStatsProvider{pool: pool}
}

// ModelStats implements StatsProvider.
func (p *PGStatsProvider) ModelStats(ctx context.Context, window time.Duration) (map[string]ModelStats, error) {
	since := time.Now().Add(-window)
	out := map[string]ModelStats{}

	rows, err := p.pool.Query(ctx, `
		SELECT request_model,
		       COUNT(DISTINCT trace_id)::bigint AS samples
		FROM eval_scores
		WHERE timestamp >= $1 AND request_model <> ''
		GROUP BY request_model`, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s ModelStats
		if err := rows.Scan(&s.Model, &s.Samples); err != nil {
			rows.Close()
			return nil, err
		}
		out[s.Model] = s
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	qrows, err := p.pool.Query(ctx, `
		SELECT request_model,
		       AVG(score) AS quality,
		       AVG(CASE WHEN passed THEN 1.0 ELSE 0.0 END) AS pass_rate,
		       COUNT(*)::bigint AS quality_samples
		FROM eval_scores
		WHERE timestamp >= $1 AND metric = 'quality' AND request_model <> ''
		GROUP BY request_model`, since)
	if err != nil {
		return nil, err
	}
	for qrows.Next() {
		var model string
		var quality, passRate float64
		var qualitySamples int64
		if err := qrows.Scan(&model, &quality, &passRate, &qualitySamples); err != nil {
			qrows.Close()
			return nil, err
		}
		s := out[model]
		s.Model = model
		s.Quality = quality
		s.PassRate = passRate
		s.QualitySamples = qualitySamples
		if s.Samples == 0 {
			s.Samples = qualitySamples
		}
		out[model] = s
	}
	qrows.Close()
	if err := qrows.Err(); err != nil {
		return nil, err
	}

	srows, err := p.pool.Query(ctx, `
		SELECT request_model,
		       AVG(CASE WHEN passed THEN 1.0 ELSE 0.0 END) AS safety_pass_rate,
		       COUNT(*)::bigint AS safety_samples
		FROM eval_scores
		WHERE timestamp >= $1 AND evaluator LIKE 'heuristic_%' AND request_model <> ''
		GROUP BY request_model`, since)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	for srows.Next() {
		var model string
		var safetyPassRate float64
		var safetySamples int64
		if err := srows.Scan(&model, &safetyPassRate, &safetySamples); err != nil {
			return nil, err
		}
		s := out[model]
		s.Model = model
		s.SafetyPassRate = safetyPassRate
		s.SafetySamples = safetySamples
		if s.Samples == 0 {
			s.Samples = safetySamples
		}
		out[model] = s
	}
	return out, srows.Err()
}
