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
	TraceID      string    `json:"trace_id"`
	Timestamp    time.Time `json:"timestamp"`
	ProviderName string    `json:"provider_name"`
	RequestModel string    `json:"request_model"`
	InputTokens  uint32    `json:"input_tokens"`
	OutputTokens uint32    `json:"output_tokens"`
	LatencyMs    int64     `json:"latency_ms"`
	TTFTMs       int64     `json:"ttft_ms"`
	CostUSD      float64   `json:"cost_usd"`
	StatusCode   uint16    `json:"status_code"`
	Streamed     uint8     `json:"streamed"`
	FinishReason string    `json:"finish_reason"`
}

// RecentTraces returns the most recent traces, newest first.
func (r *Reader) RecentTraces(ctx context.Context, limit int) ([]TraceSummary, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := r.conn.Query(ctx, `
		SELECT trace_id, timestamp, provider_name, request_model,
		       input_tokens, output_tokens, latency_ms, ttft_ms, cost_usd,
		       status_code, streamed, finish_reason
		FROM gateway_traces
		ORDER BY timestamp DESC
		LIMIT ?`, limit)
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
			&s.StatusCode, &s.Streamed, &s.FinishReason,
		); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Stats holds dashboard aggregates over a recent time window.
type Stats struct {
	TotalRequests int64   `json:"total_requests"`
	ErrorRate     float64 `json:"error_rate"`
	AvgLatencyMs  float64 `json:"avg_latency_ms"`
	P95LatencyMs  float64 `json:"p95_latency_ms"`
	TotalTokens   int64   `json:"total_tokens"`
	TotalCostUSD  float64 `json:"total_cost_usd"`
}

// WindowStats returns aggregate metrics over the trailing window.
func (r *Reader) WindowStats(ctx context.Context, window time.Duration) (Stats, error) {
	var s Stats
	row := r.conn.QueryRow(ctx, `
		SELECT
			count() AS total,
			if(total = 0, 0, countIf(status_code >= 400) / total) AS error_rate,
			avg(latency_ms) AS avg_latency,
			quantile(0.95)(latency_ms) AS p95_latency,
			toInt64(sum(input_tokens + output_tokens)) AS total_tokens,
			sum(cost_usd) AS total_cost
		FROM gateway_traces
		WHERE timestamp >= now() - INTERVAL ? SECOND`,
		int64(window.Seconds()))
	if err := row.Scan(&s.TotalRequests, &s.ErrorRate, &s.AvgLatencyMs, &s.P95LatencyMs, &s.TotalTokens, &s.TotalCostUSD); err != nil {
		return s, err
	}
	return s, nil
}
