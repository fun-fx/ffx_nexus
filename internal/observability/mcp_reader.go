package observability

import (
	"context"
	"strings"
	"time"
)

// MCPLogSummary is a compact row for the MCP logs list.
type MCPLogSummary struct {
	ID           string    `json:"id"`
	Timestamp    time.Time `json:"timestamp"`
	ServerLabel  string    `json:"server_label"`
	ToolName     string    `json:"tool_name"`
	Status       string    `json:"status"`
	LatencyMs    int64     `json:"latency_ms"`
	CostUSD      float64   `json:"cost_usd"`
	VirtualKeyID string    `json:"virtual_key_id"`
	LLMTraceID   string    `json:"llm_trace_id,omitempty"`
	SessionID    string    `json:"session_id,omitempty"`
	TurnID       string    `json:"turn_id,omitempty"`
	RequestID    string    `json:"request_id,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
}

// MCPLogDetail includes full payloads.
type MCPLogDetail struct {
	MCPLogSummary
	ServerID  string `json:"server_id"`
	UserID    string `json:"user_id"`
	Arguments string `json:"arguments,omitempty"`
	Result    string `json:"result,omitempty"`
	Metadata  string `json:"metadata,omitempty"`
}

// MCPLogFilter narrows MCP log queries.
type MCPLogFilter struct {
	ToolName     string
	ServerLabel  string
	Status       string
	VirtualKeyID string
	LLMTraceID   string
	Q            string
}

// MCPLogCursor is pagination state for MCP logs.
type MCPLogCursor struct {
	BeforeISO string `json:"before"`
	SinceISO  string `json:"since"`
}

// MCPLogPage is one page of MCP logs.
type MCPLogPage struct {
	Items      []MCPLogSummary `json:"items"`
	NextCursor MCPLogCursor    `json:"next_cursor"`
}

// MCPLogStats aggregates MCP log metrics for a time window.
type MCPLogStats struct {
	Total       uint64  `json:"total"`
	Success     uint64  `json:"success"`
	Errors      uint64  `json:"errors"`
	SuccessRate float64 `json:"success_rate"`
	AvgLatency  float64 `json:"avg_latency_ms"`
	P50Latency  float64 `json:"p50_latency_ms"`
	P95Latency  float64 `json:"p95_latency_ms"`
}

// MCPLogFilterData holds distinct values for filter dropdowns.
type MCPLogFilterData struct {
	ToolNames    []string `json:"tool_names"`
	ServerLabels []string `json:"server_labels"`
	Statuses     []string `json:"statuses"`
}

// MCPLogPage returns cursor-paged MCP logs for an org.
func (r *Reader) MCPLogPage(ctx context.Context, orgID, userID string, limit int, before, since time.Time, f MCPLogFilter) (MCPLogPage, error) {
	if r == nil || r.conn == nil {
		return MCPLogPage{}, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if since.IsZero() {
		since = time.Now().Add(-90 * 24 * time.Hour)
	}
	if before.IsZero() {
		before = time.Now()
	}

	query := `
		SELECT id, timestamp, server_label, tool_name, status,
		       latency_ms, cost_usd, virtual_key_id,
		       llm_trace_id, session_id, turn_id, request_id, error_message
		FROM mcp_tool_logs`
	orgCond, args := orgScopeClause(orgID)
	conds := []string{orgCond, "timestamp >= ?", "timestamp < ?"}
	args = append(args, since, before)
	if userID != "" {
		conds = append(conds, "user_id = ?")
		args = append(args, userID)
	}
	if f.ToolName != "" {
		conds = append(conds, "tool_name = ?")
		args = append(args, f.ToolName)
	}
	if f.ServerLabel != "" {
		conds = append(conds, "server_label = ?")
		args = append(args, f.ServerLabel)
	}
	if f.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, f.Status)
	}
	if f.VirtualKeyID != "" {
		conds = append(conds, "virtual_key_id = ?")
		args = append(args, f.VirtualKeyID)
	}
	if f.LLMTraceID != "" {
		conds = append(conds, "llm_trace_id = ?")
		args = append(args, f.LLMTraceID)
	}
	if f.Q != "" {
		conds = append(conds, "(positionCaseInsensitive(arguments, ?) > 0 OR positionCaseInsensitive(result, ?) > 0 OR positionCaseInsensitive(tool_name, ?) > 0)")
		args = append(args, f.Q, f.Q, f.Q)
	}
	query += ` WHERE ` + strings.Join(conds, " AND ")
	query += ` ORDER BY timestamp DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := r.conn.Query(ctx, query, args...)
	if err != nil {
		return MCPLogPage{}, err
	}
	defer rows.Close()

	var items []MCPLogSummary
	for rows.Next() {
		var s MCPLogSummary
		if err := rows.Scan(
			&s.ID, &s.Timestamp, &s.ServerLabel, &s.ToolName, &s.Status,
			&s.LatencyMs, &s.CostUSD, &s.VirtualKeyID,
			&s.LLMTraceID, &s.SessionID, &s.TurnID, &s.RequestID, &s.ErrorMessage,
		); err != nil {
			return MCPLogPage{}, err
		}
		items = append(items, s)
	}
	if err := rows.Err(); err != nil {
		return MCPLogPage{}, err
	}

	page := MCPLogPage{Items: items}
	if len(items) == limit {
		last := items[len(items)-1]
		page.NextCursor = MCPLogCursor{
			BeforeISO: last.Timestamp.Format(time.RFC3339Nano),
			SinceISO:  since.Format(time.RFC3339Nano),
		}
	}
	return page, nil
}

// MCPLogByID returns one MCP log with payloads.
func (r *Reader) MCPLogByID(ctx context.Context, orgID, id string) (*MCPLogDetail, error) {
	if r == nil || r.conn == nil {
		return nil, nil
	}
	orgCond, args := orgScopeClause(orgID)
	query := `
		SELECT id, timestamp, server_id, server_label, tool_name, status,
		       latency_ms, cost_usd, virtual_key_id, user_id,
		       llm_trace_id, session_id, turn_id, request_id,
		       arguments, result, error_message, metadata
		FROM mcp_tool_logs WHERE ` + orgCond + ` AND id = ?`
	args = append(args, id)

	rows, err := r.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	var d MCPLogDetail
	if err := rows.Scan(
		&d.ID, &d.Timestamp, &d.ServerID, &d.ServerLabel, &d.ToolName, &d.Status,
		&d.LatencyMs, &d.CostUSD, &d.VirtualKeyID, &d.UserID,
		&d.LLMTraceID, &d.SessionID, &d.TurnID, &d.RequestID,
		&d.Arguments, &d.Result, &d.ErrorMessage, &d.Metadata,
	); err != nil {
		return nil, err
	}
	d.MCPLogSummary = MCPLogSummary{
		ID: d.ID, Timestamp: d.Timestamp, ServerLabel: d.ServerLabel,
		ToolName: d.ToolName, Status: d.Status, LatencyMs: d.LatencyMs,
		CostUSD: d.CostUSD, VirtualKeyID: d.VirtualKeyID, LLMTraceID: d.LLMTraceID,
		SessionID: d.SessionID, TurnID: d.TurnID, RequestID: d.RequestID,
		ErrorMessage: d.ErrorMessage,
	}
	return &d, rows.Err()
}

// MCPLogStatsWindow returns aggregate stats for a time range.
func (r *Reader) MCPLogStatsWindow(ctx context.Context, orgID, userID string, since, before time.Time) (MCPLogStats, error) {
	if r == nil || r.conn == nil {
		return MCPLogStats{}, nil
	}
	if since.IsZero() {
		since = time.Now().Add(-24 * time.Hour)
	}
	if before.IsZero() {
		before = time.Now()
	}
	orgCond, args := orgScopeClause(orgID)
	conds := []string{orgCond, "timestamp >= ?", "timestamp < ?"}
	args = append(args, since, before)
	if userID != "" {
		conds = append(conds, "user_id = ?")
		args = append(args, userID)
	}
	query := `
		SELECT
			count() AS total,
			countIf(status = 'success') AS success,
			countIf(status = 'error') AS errors,
			avg(latency_ms) AS avg_latency,
			quantile(0.5)(latency_ms) AS p50,
			quantile(0.95)(latency_ms) AS p95
		FROM mcp_tool_logs WHERE ` + strings.Join(conds, " AND ")

	rows, err := r.conn.Query(ctx, query, args...)
	if err != nil {
		return MCPLogStats{}, err
	}
	defer rows.Close()
	var stats MCPLogStats
	if rows.Next() {
		if err := rows.Scan(&stats.Total, &stats.Success, &stats.Errors, &stats.AvgLatency, &stats.P50Latency, &stats.P95Latency); err != nil {
			return MCPLogStats{}, err
		}
	}
	if stats.Total > 0 {
		stats.SuccessRate = float64(stats.Success) / float64(stats.Total)
	}
	return stats, rows.Err()
}

// MCPLogFilterData returns distinct filter values (last 7 days).
func (r *Reader) MCPLogFilterData(ctx context.Context, orgID string) (MCPLogFilterData, error) {
	if r == nil || r.conn == nil {
		return MCPLogFilterData{}, nil
	}
	since := time.Now().Add(-7 * 24 * time.Hour)
	orgCond, baseArgs := orgScopeClause(orgID)
	base := ` FROM mcp_tool_logs WHERE ` + orgCond + ` AND timestamp >= ?`
	queryArgs := append(append([]any{}, baseArgs...), since)

	out := MCPLogFilterData{Statuses: []string{"success", "error"}}
	for _, pair := range []struct {
		col  string
		dest *[]string
	}{
		{"tool_name", &out.ToolNames},
		{"server_label", &out.ServerLabels},
	} {
		q := `SELECT DISTINCT ` + pair.col + base + ` ORDER BY ` + pair.col + ` LIMIT 200`
		rows, err := r.conn.Query(ctx, q, queryArgs...)
		if err != nil {
			return MCPLogFilterData{}, err
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return MCPLogFilterData{}, err
			}
			if v != "" {
				*pair.dest = append(*pair.dest, v)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return MCPLogFilterData{}, err
		}
	}
	return out, nil
}
