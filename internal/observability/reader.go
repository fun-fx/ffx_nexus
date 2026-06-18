package observability

import (
	"context"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Reader queries persisted traces for the console/dashboard.
type Reader struct {
	conn driver.Conn
}

// NewReader returns a Reader backed by the recorder's ClickHouse connection.
func (r *CHRecorder) NewReader() *Reader { return &Reader{conn: r.conn} }

// TraceSummary is a compact row for the trace list view.
type TraceSummary struct {
	TraceID          string    `json:"trace_id"`
	Timestamp        time.Time `json:"timestamp"`
	ProviderName     string    `json:"provider_name"`
	RequestModel     string    `json:"request_model"`
	InputTokens      uint32    `json:"input_tokens"`
	OutputTokens     uint32    `json:"output_tokens"`
	LatencyMs        int64     `json:"latency_ms"`
	TTFTMs           int64     `json:"ttft_ms"`
	CostUSD          float64   `json:"cost_usd"`
	StatusCode       uint16    `json:"status_code"`
	Streamed         uint8     `json:"streamed"`
	FinishReason     string    `json:"finish_reason"`
	CacheHit         uint8     `json:"cache_hit"`
	GuardrailAction  string    `json:"guardrail_action"`
	UserID           string    `json:"user_id"`
	CredentialSource string    `json:"credential_source"`
}

// RecentTraces returns the most recent traces, newest first. When userID is
// non-empty, the result is scoped to that caller's traffic (BYOK dashboard).
func (r *Reader) RecentTraces(ctx context.Context, limit int, userID string) ([]TraceSummary, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	query := `
		SELECT trace_id, timestamp, provider_name, request_model,
		       input_tokens, output_tokens, latency_ms, ttft_ms, cost_usd,
		       status_code, streamed, finish_reason, cache_hit, guardrail_action,
		       user_id, credential_source
		FROM gateway_traces`
	args := []any{}
	if userID != "" {
		query += ` WHERE user_id = ?`
		args = append(args, userID)
	}
	query += ` ORDER BY timestamp DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TraceSummary
	for rows.Next() {
		var s TraceSummary
		if err := rows.Scan(
			&s.TraceID, &s.Timestamp, &s.ProviderName, &s.RequestModel,
			&s.InputTokens, &s.OutputTokens, &s.LatencyMs, &s.TTFTMs, &s.CostUSD,
			&s.StatusCode, &s.Streamed, &s.FinishReason, &s.CacheHit, &s.GuardrailAction,
			&s.UserID, &s.CredentialSource,
		); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Stats holds dashboard aggregates over a recent time window.
type Stats struct {
	TotalRequests   int64   `json:"total_requests"`
	ErrorRate       float64 `json:"error_rate"`
	AvgLatencyMs    float64 `json:"avg_latency_ms"`
	P95LatencyMs    float64 `json:"p95_latency_ms"`
	TotalTokens     int64   `json:"total_tokens"`
	TotalCostUSD    float64 `json:"total_cost_usd"`
	CacheHits       int64   `json:"cache_hits"`
	CacheHitRate    float64 `json:"cache_hit_rate"`
	GuardrailEvents int64   `json:"guardrail_events"`
}

// WindowStats returns aggregate metrics over the trailing window. When userID
// is non-empty, aggregates are scoped to that caller's traffic.
func (r *Reader) WindowStats(ctx context.Context, window time.Duration, userID string) (Stats, error) {
	var s Stats
	query := `
		SELECT
			toInt64(count()) AS total,
			if(count() = 0, 0, countIf(status_code >= 400) / count()) AS error_rate,
			if(count() = 0, 0, avg(latency_ms)) AS avg_latency,
			if(count() = 0, 0, toFloat64(quantileTDigest(0.95)(latency_ms))) AS p95_latency,
			toInt64(sum(input_tokens + output_tokens)) AS total_tokens,
			ifNull(sum(cost_usd), 0) AS total_cost,
			toInt64(countIf(cache_hit = 1)) AS cache_hits,
			if(count() = 0, 0, countIf(cache_hit = 1) / count()) AS cache_hit_rate,
			toInt64(countIf(guardrail_action != '')) AS guardrail_events
		FROM gateway_traces
		WHERE timestamp >= now() - INTERVAL ? SECOND`
	args := []any{int64(window.Seconds())}
	if userID != "" {
		query += ` AND user_id = ?`
		args = append(args, userID)
	}
	query += ` SETTINGS max_memory_usage = 400000000`
	row := r.conn.QueryRow(ctx, query, args...)
	if err := row.Scan(
		&s.TotalRequests, &s.ErrorRate, &s.AvgLatencyMs, &s.P95LatencyMs,
		&s.TotalTokens, &s.TotalCostUSD, &s.CacheHits, &s.CacheHitRate, &s.GuardrailEvents,
	); err != nil {
		return s, err
	}
	return s, nil
}

// EvalMetric aggregates async eval scores for one metric over a time window.
type EvalMetric struct {
	Evaluator string  `json:"evaluator"`
	Metric    string  `json:"metric"`
	AvgScore  float64 `json:"avg_score"`
	PassRate  float64 `json:"pass_rate"`
	Samples   int64   `json:"samples"`
}

// EvalSummary returns per-(evaluator, metric) aggregates from eval_scores over
// the trailing window, ordered by sample count so the busiest metrics surface
// first. Returns an empty slice when no scores exist.
func (r *Reader) EvalSummary(ctx context.Context, window time.Duration) ([]EvalMetric, error) {
	rows, err := r.conn.Query(ctx, `
		SELECT evaluator, metric,
		       avg(score) AS avg_score,
		       avg(passed) AS pass_rate,
		       toInt64(count()) AS samples
		FROM eval_scores
		WHERE timestamp >= now() - INTERVAL ? SECOND
		GROUP BY evaluator, metric
		ORDER BY samples DESC`,
		int64(window.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]EvalMetric, 0)
	for rows.Next() {
		var m EvalMetric
		if err := rows.Scan(&m.Evaluator, &m.Metric, &m.AvgScore, &m.PassRate, &m.Samples); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// UserQuality is a per-user rolling quality/safety aggregate over a window. This
// is Nexus's eval differentiator over spend-only gateways: alongside per-user
// cost we surface "what is this user's rolling quality score" (see
// docs/byok-multitenancy-design.md §9).
type UserQuality struct {
	UserID     string  `json:"user_id"`
	AvgQuality float64 `json:"avg_quality"` // mean of judge "quality" scores, 0..1
	PassRate   float64 `json:"pass_rate"`   // mean pass across all evaluators/metrics
	Samples    int64   `json:"samples"`     // total eval scores in the window
	CostUSD    float64 `json:"cost_usd"`    // total spend in the window (from traces)
	Requests   int64   `json:"requests"`    // total requests in the window (from traces)
}

// UserQualitySummary returns per-user quality/safety aggregates joined with
// per-user spend over the trailing window, ordered by sample count. Users with
// no recorded user_id (legacy/org-level traffic) are excluded. When userID is
// non-empty, the result is restricted to that single user (for the /me/quality
// endpoint).
func (r *Reader) UserQualitySummary(ctx context.Context, window time.Duration, userID string) ([]UserQuality, error) {
	secs := int64(window.Seconds())
	// The user filter is appended to BOTH the eval_scores and gateway_traces
	// CTEs, so the bind count is 2 (one per `?`) plus 2 for the window
	// seconds on each side (4 placeholders total). With userID set we have
	// 6 placeholders (2 user ids + 2 secs + 2 secs) — order matters and is
	// [userID, secs, userID, secs] in the rendered SQL.
	var userFilter string
	args := []any{secs, secs}
	if userID != "" {
		userFilter = ` AND user_id = ?`
		args = []any{userID, secs, userID, secs}
	} else {
		userFilter = ` AND user_id != ''`
	}
	rows, err := r.conn.Query(ctx, `
		WITH
		  q AS (
		    SELECT user_id,
		           avgIf(score, metric = 'quality') AS avg_quality,
		           avg(passed) AS pass_rate,
		           toInt64(count()) AS samples
		    FROM eval_scores
		    WHERE timestamp >= now() - INTERVAL ? SECOND`+userFilter+`
		    GROUP BY user_id
		  ),
		  t AS (
		    SELECT user_id,
		           sum(cost_usd) AS cost_usd,
		           toInt64(count()) AS requests
		    FROM gateway_traces
		    WHERE timestamp >= now() - INTERVAL ? SECOND`+userFilter+`
		    GROUP BY user_id
		  )
		SELECT q.user_id,
		       q.avg_quality,
		       q.pass_rate,
		       q.samples,
		       t.cost_usd,
		       t.requests
		FROM q LEFT JOIN t ON q.user_id = t.user_id
		ORDER BY q.samples DESC`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]UserQuality, 0)
	for rows.Next() {
		var u UserQuality
		if err := rows.Scan(&u.UserID, &u.AvgQuality, &u.PassRate, &u.Samples, &u.CostUSD, &u.Requests); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
