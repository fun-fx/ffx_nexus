package router

import (
	"context"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// CHStatsProvider reads rolling per-model stats from ClickHouse, combining
// trace aggregates (latency/cost/volume) with quality eval scores.
type CHStatsProvider struct {
	conn driver.Conn
}

// NewCHStatsProvider builds a provider reusing an existing ClickHouse pool.
func NewCHStatsProvider(conn driver.Conn) *CHStatsProvider {
	return &CHStatsProvider{conn: conn}
}

// ModelStats implements StatsProvider.
func (p *CHStatsProvider) ModelStats(ctx context.Context, window time.Duration) (map[string]ModelStats, error) {
	secs := int64(window.Seconds())
	out := map[string]ModelStats{}

	// Trace aggregates: latency, cost, sample count per model (successes only).
	rows, err := p.conn.Query(ctx, `
		SELECT request_model,
		       avg(latency_ms) AS avg_latency,
		       avg(cost_usd)   AS avg_cost,
		       toInt64(count()) AS samples
		FROM gateway_traces
		WHERE timestamp >= now() - INTERVAL ? SECOND AND status_code = 200
		GROUP BY request_model`, secs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s ModelStats
		if err := rows.Scan(&s.Model, &s.AvgLatencyMs, &s.AvgCostUSD, &s.Samples); err != nil {
			rows.Close()
			return nil, err
		}
		out[s.Model] = s
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Judge quality scores: join eval_scores (metric='quality') to traces.
	qrows, err := p.conn.Query(ctx, `
		SELECT t.request_model AS model,
		       avg(e.score)     AS quality,
		       avg(e.passed)    AS pass_rate,
		       toInt64(count()) AS quality_samples
		FROM eval_scores AS e
		INNER JOIN gateway_traces AS t ON e.trace_id = t.trace_id
		WHERE e.metric = 'quality' AND e.timestamp >= now() - INTERVAL ? SECOND
		GROUP BY t.request_model`, secs)
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
		out[model] = s
	}
	qrows.Close()
	if err := qrows.Err(); err != nil {
		return nil, err
	}

	// Heuristic safety scores: average pass across heuristic evaluators
	// (e.g. PII, completeness), so routing reacts to them even without a judge.
	srows, err := p.conn.Query(ctx, `
		SELECT t.request_model AS model,
		       avg(e.passed)    AS safety_pass_rate,
		       toInt64(count()) AS safety_samples
		FROM eval_scores AS e
		INNER JOIN gateway_traces AS t ON e.trace_id = t.trace_id
		WHERE e.evaluator LIKE 'heuristic_%' AND e.timestamp >= now() - INTERVAL ? SECOND
		GROUP BY t.request_model`, secs)
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
		out[model] = s
	}
	return out, srows.Err()
}
