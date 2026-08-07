package observability

import (
	"context"
	"strings"
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
	// ResponseModel is the model that actually served the response —
	// may differ from RequestModel when routing aliases / fallbacks
	// dispatched to a different vendor (e.g. claude-opus-latest
	// requested, anthropic/claude-opus-5 answered because The Grid
	// routed it). Surfaced on the Recent-sessions panel so multi-
	// vendor fan-out stays visible even when every shared row's
	// provider_name is "grid".
	ResponseModel string `json:"response_model,omitempty"`
	InputTokens   uint32 `json:"input_tokens"`
	OutputTokens  uint32 `json:"output_tokens"`
	// TotalTokens is input_tokens + output_tokens at read time. We
	// compute it in the SELECT rather than storing it in the table —
	// the source columns are append-only the row ingests, and we'd
	// rather avoid rewriting client totals every time the cost
	// composer changes.
	TotalTokens     int64   `json:"total_tokens"`
	LatencyMs       int64   `json:"latency_ms"`
	TTFTMs          int64   `json:"ttft_ms"`
	CostUSD         float64 `json:"cost_usd"`
	StatusCode      uint16  `json:"status_code"`
	Streamed        uint8   `json:"streamed"`
	FinishReason    string  `json:"finish_reason"`
	CacheHit        uint8   `json:"cache_hit"`
	GuardrailAction string  `json:"guardrail_action"`
	// SessionID is the per-conversation marker the gateway extracted
	// from metadata.session_id / sessionId / conversation_id on the
	// request, or "user:<id>" when only the OpenAI user field was
	// present. Empty when none of those were on the wire — the
	// frontend's sessionize fallback merges by time window in that
	// case. Added in 007_session_id.sql.
	SessionID        string `json:"session_id,omitempty"`
	UserID           string `json:"user_id"`
	UserEmail        string `json:"user_email,omitempty"`
	CredentialSource string `json:"credential_source"`
}

// RecentTraces returns the most recent traces, newest first. When userID is
// non-empty, the result is scoped to that caller's traffic (BYOK dashboard).
//
// Exists for backwards compatibility; new callers should prefer TracePage,
// which exposes a sliding time-window and a cursor for "Load older".
func (r *Reader) RecentTraces(ctx context.Context, limit int, userID string) ([]TraceSummary, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	query := `
		SELECT trace_id, timestamp, provider_name, request_model,
		       response_model, input_tokens, output_tokens,
		       toInt64(input_tokens + output_tokens) AS total_tokens,
		       latency_ms, ttft_ms, cost_usd,
		       status_code, streamed, finish_reason, cache_hit, guardrail_action,
		       session_id, user_id, credential_source
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
			&s.ResponseModel, &s.InputTokens, &s.OutputTokens, &s.TotalTokens,
			&s.LatencyMs, &s.TTFTMs, &s.CostUSD,
			&s.StatusCode, &s.Streamed, &s.FinishReason, &s.CacheHit, &s.GuardrailAction,
			&s.SessionID, &s.UserID, &s.CredentialSource,
		); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// TraceFilter narrows a TracePage's match set to a single status slice, a
// specific provider, or a fuzzy text query. Empty fields collapse to "any".
// The console sends these straight through to /api/traces as query params
// (?status=err&provider=openai&q=gpt-4o), so the field names mirror the URL
// surface exactly. `Q` is matched as a substring against request_model,
// provider_name, user_email (joined in via enrichTraceUserEmails), and
// guardrail_action — same set as the legacy client-side filter, just
// pushed into ClickHouse so pages stay consistent under time-windowing.
type TraceFilter struct {
	Status   string // "ok" | "err" | "" (any)
	Provider string // exact match against provider_name, empty = any
	Q        string // fuzzy match, empty = any
}

// TraceCursor is an opaque cursor the console holds between pages of trace
// listings. Encoding only the timestamp is sufficient because TracePage orders
// by `(timestamp, trace_id) DESC` and uses the timestamp as the cursor key
// with `WHERE timestamp < ?` filtered server-side; trace_id is not needed for
// correctness (only `<` not `<=`), but emitting it makes the cursor URL
// self-documenting and discourages hand-edits.
type TraceCursor struct {
	BeforeISO string `json:"before"` // RFC3339Nano; empty when at the newest edge
	SinceISO  string `json:"since"`  // RFC3339Nano; lower bound on the window
}

// TracePage is one windowed, cursor-paged, filter-narrowed view of
// gateway_traces.
//
// "Window" = [since, before) — half-open so two adjacent pages do not overlap
// at the boundary and so subsequent calls without a "since" still produce a
// bounded result. When `since` is the empty string, the server treats the
// floor as the gateway_traces TTL (90 days in the prod migration), and when
// `before` is the empty string the result is anchored on "now" at query time.
//
// Filters (status, provider, q) widen the funnel so the cursor walks only
// matches. The console preserves them across "Load older" pages by echoing
// them back into the next request — see TraceCursor for the wire shape.
type TracePage struct {
	Items      []TraceSummary `json:"items"`
	NextCursor TraceCursor    `json:"next_cursor"`
}

// TracePage returns one cursor-paged, filter-narrowed slice of traces bounded
// by the supplied time window. When `userID` is non-empty the result is
// scoped to that caller's traffic, identical to RecentTraces.
//
// `before` / `since` are RFC3339 timestamps (second or nano precision). A zero
// value on either side collapses the bound to the underlying table TTL on
// the low end and to "now" on the high end.
//
// `limit` caps the page size. The function requests `limit + 1` rows so it
// can detect whether a next page exists without a second `COUNT(*)`. The
// returned cursor's `before` is set to the timestamp of the last returned
// row so the next call's `before=...` predicate continues from there.
func (r *Reader) TracePage(ctx context.Context, before, since time.Time, limit int, userID string, filter TraceFilter) (TracePage, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	// We request limit+1 rows so we can detect "next page exists" without a
	// `count()`. The last row is dropped below.
	const probe = 1
	rows, err := r.conn.Query(ctx, buildTracePageQuery(userID, before, since, limit+probe, filter), buildTracePageArgs(userID, before, since, limit+probe, filter)...)
	if err != nil {
		return TracePage{}, err
	}
	defer rows.Close()

	out := make([]TraceSummary, 0, limit+probe)
	for rows.Next() {
		var s TraceSummary
		if err := rows.Scan(
			&s.TraceID, &s.Timestamp, &s.ProviderName, &s.RequestModel,
			&s.ResponseModel, &s.InputTokens, &s.OutputTokens, &s.TotalTokens,
			&s.LatencyMs, &s.TTFTMs, &s.CostUSD,
			&s.StatusCode, &s.Streamed, &s.FinishReason, &s.CacheHit, &s.GuardrailAction,
			&s.SessionID, &s.UserID, &s.CredentialSource,
		); err != nil {
			return TracePage{}, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return TracePage{}, err
	}

	page := TracePage{}
	if len(out) > limit {
		// Probe indicates there is at least one more page.
		page.NextCursor = TraceCursor{
			BeforeISO: out[limit-1].Timestamp.UTC().Format(time.RFC3339Nano),
			SinceISO:  cursorSince(since),
		}
		out = out[:limit]
	} else {
		// Emit a self-documenting cursor only when the caller had set
		// `since`; otherwise the page concept collapses to "no more pages
		// in this window", which the client already knows.
		if !since.IsZero() {
			page.NextCursor = TraceCursor{SinceISO: cursorSince(since)}
		}
	}
	page.Items = out
	return page, nil
}

func cursorSince(since time.Time) string {
	if since.IsZero() {
		return ""
	}
	return since.UTC().Format(time.RFC3339Nano)
}

// buildTracePageQuery produces the windowed + filter-narrowed SELECT for
// TracePage. Pulled out as a pure function so the unit test in reader_test.go
// can pin the SQL shape without needing a real ClickHouse connection.
//
// Placeholder ordering (mirrored by buildTracePageArgs) is:
//  1. user_id (if present)
//  2. timestamp < (before)
//  3. timestamp >= (since)
//  4. provider_name (=)
//  5. status predicate (no args; uses inline literal `>= 400` or `< 400`)
//  6. q ILIKE on four columns (the operator `%?%` repeats four times)
//  7. LIMIT
func buildTracePageQuery(userID string, before, since time.Time, limit int, filter TraceFilter) string {
	q := `
		SELECT trace_id, timestamp, provider_name, request_model,
		       response_model, input_tokens, output_tokens,
		       toInt64(input_tokens + output_tokens) AS total_tokens,
		       latency_ms, ttft_ms, cost_usd,
		       status_code, streamed, finish_reason, cache_hit, guardrail_action,
		       session_id, user_id, credential_source
		FROM gateway_traces`
	conds := []string{}
	if userID != "" {
		conds = append(conds, "user_id = ?")
	}
	if !before.IsZero() {
		conds = append(conds, "timestamp < ?")
	}
	if !since.IsZero() {
		conds = append(conds, "timestamp >= ?")
	}
	if filter.Provider != "" {
		conds = append(conds, "provider_name = ?")
	}
	switch filter.Status {
	case "ok":
		conds = append(conds, "status_code < 400")
	case "err":
		conds = append(conds, "status_code >= 400")
	}
	if filter.Q != "" {
		conds = append(conds,
			"(request_model LIKE ? OR provider_name LIKE ? OR user_email LIKE ? OR guardrail_action LIKE ?)")
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY timestamp DESC, trace_id DESC LIMIT ?"
	return q
}

// buildTracePageArgs mirrors buildTracePageQuery's placeholder ordering. The
// caller MUST pass both to the driver in this exact order; ClickHouse binds
// positionally. The `q` argument is wildcard-padded and percent/underscore
// escaped here (NOT in the SQL string) so the SQL stays a single placeholder.
func buildTracePageArgs(userID string, before, since time.Time, limit int, filter TraceFilter) []any {
	args := []any{}
	if userID != "" {
		args = append(args, userID)
	}
	if !before.IsZero() {
		args = append(args, before.UTC())
	}
	if !since.IsZero() {
		args = append(args, since.UTC())
	}
	if filter.Provider != "" {
		args = append(args, filter.Provider)
	}
	if filter.Q != "" {
		pattern := "%" + escapeLike(filter.Q) + "%"
		args = append(args, pattern, pattern, pattern, pattern)
	}
	args = append(args, limit)
	return args
}

// escapeLike escapes the two metacharacters the SQL `LIKE` operator treats
// specially when NOT paired with an explicit ESCAPE clause. Without it,
// `q = "gpt_"` would match `gptX` (underscore = any single char) and
// `q = "100%"` would expect trailing digits — both obvious surprises for
// a free-text search box.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
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

// ProviderStat is one row in a per-provider aggregate over a recent window.
// Sourced from gateway_traces; one row per distinct provider_name that
// issued at least one trace in the window.
type ProviderStat struct {
	Provider     string  `json:"provider"` // openai, anthropic, grid, gemini, etc.
	Requests     int64   `json:"requests"` // trace count in window
	CostUSD      float64 `json:"cost_usd"` // sum(trace.cost_usd)
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	CacheHits    int64   `json:"cache_hits"`
}

// ProviderStats returns per-provider aggregates over the trailing window.
// Ordered by raw cost descending so the dashboard's "spend by provider"
// widget can render the most expensive provider first. When userID is
// non-empty, only the caller's traffic is counted.
//
// Resource profile: one ClickHouse SELECT with a single GROUP BY. The
// query has the same max_memory_usage budget as WindowStats() so the
// response time lands in the same single-digit-ms range on the prod
// gateway_traces table; callers that hit this endpoint more than once
// per 30 s are expected to wrap it in in-memory cache at a higher layer.
func (r *Reader) ProviderStats(ctx context.Context, window time.Duration, userID string, limit int) ([]ProviderStat, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query := `
		SELECT
			provider_name,
			toInt64(count()) AS requests,
			ifNull(sum(cost_usd), 0) AS cost,
			toInt64(sum(input_tokens))  AS in_tokens,
			toInt64(sum(output_tokens)) AS out_tokens,
			if(count() = 0, 0, avg(latency_ms)) AS avg_latency,
			toInt64(countIf(cache_hit = 1)) AS cache_hits
		FROM gateway_traces
		WHERE timestamp >= now() - INTERVAL ? SECOND`
	args := []any{int64(window.Seconds())}
	if userID != "" {
		query += ` AND user_id = ?`
		args = append(args, userID)
	}
	query += `
		GROUP BY provider_name
		ORDER BY cost DESC
		LIMIT ?
		SETTINGS max_memory_usage = 400000000`
	args = append(args, limit)

	rows, err := r.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProviderStat
	for rows.Next() {
		var s ProviderStat
		if err := rows.Scan(&s.Provider, &s.Requests, &s.CostUSD,
			&s.InputTokens, &s.OutputTokens, &s.AvgLatencyMs, &s.CacheHits); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
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
	// CTEs, so the bind count depends on whether userID is set. SQL placeholders
	// appear in textual order:
	//   eval_scores:        INTERVAL ?  [user_id]
	//   gateway_traces:     INTERVAL ?  [user_id]
	// i.e. [secs, userID, secs, userID] when scoped, [secs, secs] otherwise.
	var userFilter string
	args := []any{secs, secs}
	if userID != "" {
		userFilter = ` AND user_id = ?`
		args = []any{secs, userID, secs, userID}
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
