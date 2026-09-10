package observability

import (
	"context"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

const dashboardFacetLimit = 200

// TokenBucket is prompt vs completion tokens in one histogram bin.
type TokenBucket struct {
	Timestamp time.Time `json:"timestamp"`
	Input     int64     `json:"input"`
	Output    int64     `json:"output"`
}

// CostBucket is summed USD spend in one histogram bin.
type CostBucket struct {
	Timestamp time.Time `json:"timestamp"`
	USD       float64   `json:"usd"`
}

// LatencyBucket is average and p95 latency in one histogram bin.
type LatencyBucket struct {
	Timestamp time.Time `json:"timestamp"`
	AvgMs     float64   `json:"avg_ms"`
	P95Ms     float64   `json:"p95_ms"`
}

// CacheBucket is cache-hit counts versus total requests in one bin.
type CacheBucket struct {
	Timestamp time.Time `json:"timestamp"`
	Hits      int64     `json:"hits"`
	Requests  int64     `json:"requests"`
}

// DashboardFacets is the distinct provider/model set for filter dropdowns.
type DashboardFacets struct {
	Providers []string `json:"providers"`
	Models    []string `json:"models"`
}

// Dashboard is the wire shape for GET /api/traces/dashboard: KPI plus
// the five Overview histograms, all over the same [since, before) and
// filter. Available is false when ClickHouse is not wired.
type Dashboard struct {
	Available       bool            `json:"available"`
	Since           time.Time       `json:"since"`
	Before          time.Time       `json:"before"`
	IntervalSeconds int64           `json:"interval_seconds"`
	Stats           Stats           `json:"stats"`
	Volume          []VolumeBucket  `json:"volume"`
	Tokens          []TokenBucket   `json:"tokens"`
	Cost            []CostBucket    `json:"cost"`
	Latency         []LatencyBucket `json:"latency"`
	Cache           []CacheBucket   `json:"cache"`
	Facets          DashboardFacets `json:"facets"`
}

// EmptyDashboard is the ClickHouse-down / zero-traffic envelope. Slices
// are empty rather than null so the SPA does not have to special-case JSON.
func EmptyDashboard(since, before time.Time, available bool) Dashboard {
	if before.IsZero() {
		before = time.Now().UTC()
	}
	if since.IsZero() {
		since = before.Add(-time.Hour)
	}
	interval := BucketIntervalForWindow(before.Sub(since))
	return Dashboard{
		Available:       available,
		Since:           since.UTC(),
		Before:          before.UTC(),
		IntervalSeconds: int64(interval.Seconds()),
		Volume:          []VolumeBucket{},
		Tokens:          []TokenBucket{},
		Cost:            []CostBucket{},
		Latency:         []LatencyBucket{},
		Cache:           []CacheBucket{},
		Facets: DashboardFacets{
			Providers: []string{},
			Models:    []string{},
		},
	}
}

// Dashboard loads KPI + histograms + facets for the Overview page.
func (r *Reader) Dashboard(ctx context.Context, before, since time.Time, orgID, userID string, filter TraceFilter) (Dashboard, error) {
	if before.IsZero() {
		before = time.Now().UTC()
	}
	if since.IsZero() {
		since = before.Add(-time.Hour)
	}
	before = before.UTC()
	since = since.UTC()
	interval := BucketIntervalForWindow(before.Sub(since))
	intervalSec := int64(interval.Seconds())
	out := EmptyDashboard(since, before, true)
	out.IntervalSeconds = intervalSec

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		st, err := r.WindowStatsRange(ctx, before, since, orgID, userID, filter)
		if err != nil {
			return err
		}
		out.Stats = st
		return nil
	})
	g.Go(func() error {
		vol, err := r.RequestVolumeSeries(ctx, before, since, orgID, userID, filter)
		if err != nil {
			return err
		}
		out.Volume = vol.Buckets
		return nil
	})
	g.Go(func() error {
		rows, err := r.queryTokenSeries(ctx, before, since, orgID, userID, filter, intervalSec)
		if err != nil {
			return err
		}
		out.Tokens = fillDenseTokenBuckets(rows, since, before, interval)
		return nil
	})
	g.Go(func() error {
		rows, err := r.queryCostSeries(ctx, before, since, orgID, userID, filter, intervalSec)
		if err != nil {
			return err
		}
		out.Cost = fillDenseCostBuckets(rows, since, before, interval)
		return nil
	})
	g.Go(func() error {
		rows, err := r.queryLatencySeries(ctx, before, since, orgID, userID, filter, intervalSec)
		if err != nil {
			return err
		}
		out.Latency = fillDenseLatencyBuckets(rows, since, before, interval)
		return nil
	})
	g.Go(func() error {
		rows, err := r.queryCacheSeries(ctx, before, since, orgID, userID, filter, intervalSec)
		if err != nil {
			return err
		}
		out.Cache = fillDenseCacheBuckets(rows, since, before, interval)
		return nil
	})
	g.Go(func() error {
		names, err := r.queryFacetValues(ctx, before, since, orgID, userID, filter, "provider_name", dimensionSkip{providers: true})
		if err != nil {
			return err
		}
		out.Facets.Providers = names
		return nil
	})
	g.Go(func() error {
		names, err := r.queryFacetValues(ctx, before, since, orgID, userID, filter, "request_model", dimensionSkip{models: true})
		if err != nil {
			return err
		}
		out.Facets.Models = names
		return nil
	})
	if err := g.Wait(); err != nil {
		return Dashboard{}, err
	}
	return out, nil
}

func (r *Reader) queryTokenSeries(ctx context.Context, before, since time.Time, orgID, userID string, filter TraceFilter, intervalSec int64) ([]TokenBucket, error) {
	q := buildBucketedSeriesQuery(
		`toInt64(sum(input_tokens)) AS input,
			toInt64(sum(output_tokens)) AS output`,
		orgID, userID, filter, dimensionSkip{},
	)
	args := buildBucketedSeriesArgs(orgID, userID, before, since, intervalSec, filter, dimensionSkip{})
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TokenBucket, 0)
	for rows.Next() {
		var b TokenBucket
		if err := rows.Scan(&b.Timestamp, &b.Input, &b.Output); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (r *Reader) queryCostSeries(ctx context.Context, before, since time.Time, orgID, userID string, filter TraceFilter, intervalSec int64) ([]CostBucket, error) {
	q := buildBucketedSeriesQuery(
		`ifNull(sum(cost_usd), 0) AS usd`,
		orgID, userID, filter, dimensionSkip{},
	)
	args := buildBucketedSeriesArgs(orgID, userID, before, since, intervalSec, filter, dimensionSkip{})
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CostBucket, 0)
	for rows.Next() {
		var b CostBucket
		if err := rows.Scan(&b.Timestamp, &b.USD); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (r *Reader) queryLatencySeries(ctx context.Context, before, since time.Time, orgID, userID string, filter TraceFilter, intervalSec int64) ([]LatencyBucket, error) {
	q := buildBucketedSeriesQuery(
		`if(count() = 0, 0, avg(latency_ms)) AS avg_ms,
			if(count() = 0, 0, toFloat64(quantileTDigest(0.95)(latency_ms))) AS p95_ms`,
		orgID, userID, filter, dimensionSkip{},
	)
	args := buildBucketedSeriesArgs(orgID, userID, before, since, intervalSec, filter, dimensionSkip{})
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]LatencyBucket, 0)
	for rows.Next() {
		var b LatencyBucket
		if err := rows.Scan(&b.Timestamp, &b.AvgMs, &b.P95Ms); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (r *Reader) queryCacheSeries(ctx context.Context, before, since time.Time, orgID, userID string, filter TraceFilter, intervalSec int64) ([]CacheBucket, error) {
	q := buildBucketedSeriesQuery(
		`toInt64(countIf(cache_hit = 1)) AS hits,
			toInt64(count()) AS requests`,
		orgID, userID, filter, dimensionSkip{},
	)
	args := buildBucketedSeriesArgs(orgID, userID, before, since, intervalSec, filter, dimensionSkip{})
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CacheBucket, 0)
	for rows.Next() {
		var b CacheBucket
		if err := rows.Scan(&b.Timestamp, &b.Hits, &b.Requests); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (r *Reader) queryFacetValues(ctx context.Context, before, since time.Time, orgID, userID string, filter TraceFilter, column string, skip dimensionSkip) ([]string, error) {
	orgCond, orgArgs := orgScopeClause(orgID)
	conds := []string{orgCond, column + ` != ''`}
	args := append([]any{}, orgArgs...)
	if userID != "" {
		conds = append(conds, "user_id = ?")
		args = append(args, userID)
	}
	conds = append(conds, "timestamp >= ?", "timestamp < ?")
	args = append(args, since.UTC(), before.UTC())
	conds = appendStatusConds(conds, filter)
	dimConds, dimArgs := appendDimensionConds(nil, filter, skip)
	conds = append(conds, dimConds...)
	args = append(args, dimArgs...)
	q := `
		SELECT ` + column + `
		FROM gateway_traces
		WHERE ` + strings.Join(conds, " AND ") + `
		GROUP BY ` + column + `
		ORDER BY ` + column + ` ASC
		LIMIT ?
		SETTINGS max_memory_usage = 400000000`
	args = append(args, dashboardFacetLimit)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	if out == nil {
		out = []string{}
	}
	return out, rows.Err()
}

func fillDenseTokenBuckets(sparse []TokenBucket, since, before time.Time, interval time.Duration) []TokenBucket {
	grid := denseGrid(since, before, interval)
	if grid == nil {
		if sparse == nil {
			return []TokenBucket{}
		}
		return sparse
	}
	byUnix := make(map[int64]TokenBucket, len(sparse))
	for _, b := range sparse {
		ts := b.Timestamp.UTC().Truncate(interval)
		byUnix[ts.Unix()] = TokenBucket{Timestamp: ts, Input: b.Input, Output: b.Output}
	}
	out := make([]TokenBucket, 0, len(grid))
	for _, t := range grid {
		if hit, ok := byUnix[t.Unix()]; ok {
			out = append(out, hit)
			continue
		}
		out = append(out, TokenBucket{Timestamp: t})
	}
	return out
}

func fillDenseCostBuckets(sparse []CostBucket, since, before time.Time, interval time.Duration) []CostBucket {
	grid := denseGrid(since, before, interval)
	if grid == nil {
		if sparse == nil {
			return []CostBucket{}
		}
		return sparse
	}
	byUnix := make(map[int64]CostBucket, len(sparse))
	for _, b := range sparse {
		ts := b.Timestamp.UTC().Truncate(interval)
		byUnix[ts.Unix()] = CostBucket{Timestamp: ts, USD: b.USD}
	}
	out := make([]CostBucket, 0, len(grid))
	for _, t := range grid {
		if hit, ok := byUnix[t.Unix()]; ok {
			out = append(out, hit)
			continue
		}
		out = append(out, CostBucket{Timestamp: t})
	}
	return out
}

func fillDenseLatencyBuckets(sparse []LatencyBucket, since, before time.Time, interval time.Duration) []LatencyBucket {
	grid := denseGrid(since, before, interval)
	if grid == nil {
		if sparse == nil {
			return []LatencyBucket{}
		}
		return sparse
	}
	byUnix := make(map[int64]LatencyBucket, len(sparse))
	for _, b := range sparse {
		ts := b.Timestamp.UTC().Truncate(interval)
		byUnix[ts.Unix()] = LatencyBucket{Timestamp: ts, AvgMs: b.AvgMs, P95Ms: b.P95Ms}
	}
	out := make([]LatencyBucket, 0, len(grid))
	for _, t := range grid {
		if hit, ok := byUnix[t.Unix()]; ok {
			out = append(out, hit)
			continue
		}
		out = append(out, LatencyBucket{Timestamp: t})
	}
	return out
}

func fillDenseCacheBuckets(sparse []CacheBucket, since, before time.Time, interval time.Duration) []CacheBucket {
	grid := denseGrid(since, before, interval)
	if grid == nil {
		if sparse == nil {
			return []CacheBucket{}
		}
		return sparse
	}
	byUnix := make(map[int64]CacheBucket, len(sparse))
	for _, b := range sparse {
		ts := b.Timestamp.UTC().Truncate(interval)
		byUnix[ts.Unix()] = CacheBucket{Timestamp: ts, Hits: b.Hits, Requests: b.Requests}
	}
	out := make([]CacheBucket, 0, len(grid))
	for _, t := range grid {
		if hit, ok := byUnix[t.Unix()]; ok {
			out = append(out, hit)
			continue
		}
		out = append(out, CacheBucket{Timestamp: t})
	}
	return out
}

func denseGrid(since, before time.Time, interval time.Duration) []time.Time {
	if interval <= 0 || !before.After(since) {
		return nil
	}
	start := since.Truncate(interval)
	if start.Before(since) {
		start = start.Add(interval)
	}
	out := make([]time.Time, 0, int(before.Sub(start)/interval)+1)
	for t := start; t.Before(before); t = t.Add(interval) {
		out = append(out, t)
	}
	return out
}
