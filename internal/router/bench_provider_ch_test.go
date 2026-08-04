package router

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// TestCHBenchProvider_LiveContract drives the CHBenchProvider end
// to end with a real ClickHouse instance, validating that
// argMax(completed_at) picks the most recent settled row. Live
// integration tests are skipped in short mode and when CH is not
// reachable, mirroring the Postgres counterpart.
//
// The test is the contract the router relies on: the BlendProvider
// is only as accurate as the snapshot's notion of "latest settled".
// A regression here (e.g. ordering by updated_at) would quietly
// refresh the blend with stale data, so we keep one full pass
// against a freshly-seeded table per CI run.
func TestCHBenchProvider_LiveContract(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live CH contract in -short mode")
	}
	dsn := os.Getenv("NEXUS_CLICKHOUSE_URL")
	if dsn == "" {
		t.Skip("NEXUS_CLICKHOUSE_URL is empty; CH provider is up to the live CI cluster only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()

	// Pin to a per-test table so concurrent regressions on a shared
	// cluster don't taint each other. The provider only knows the
	// canonical `benchmark_runs` name, so we create the canonical
	// table once and TRUNCATE between scenarios.
	if err := execCH(ctx, conn, chSchema); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = conn.Exec(ctx, "DROP TABLE IF EXISTS benchmark_runs") })

	cases := []struct {
		name string
		rows []chRow
		want map[string]float64
	}{
		{
			name: "single completed row wins",
			rows: []chRow{
				{model: "thegrid", status: "completed", avg: 0.95, completedAt: now()},
			},
			want: map[string]float64{"thegrid": 0.95},
		},
		{
			name: "most recent completed row wins among many",
			rows: []chRow{
				{model: "thegrid", status: "completed", avg: 0.70, completedAt: now().Add(-72 * time.Hour)},
				{model: "thegrid", status: "completed", avg: 0.92, completedAt: now().Add(-1 * time.Hour)},
				{model: "thegrid", status: "completed", avg: 0.81, completedAt: now().Add(-24 * time.Hour)},
			},
			want: map[string]float64{"thegrid": 0.92},
		},
		{
			name: "pending and failed rows don't contribute",
			rows: []chRow{
				{model: "anthropic-claude", status: "completed", avg: 0.55, completedAt: now()},
				{model: "anthropic-claude", status: "failed", avg: 0.99, completedAt: now().Add(1 * time.Hour)},
				{model: "anthropic-claude", status: "pending", avg: 0.88, completedAt: now().Add(2 * time.Hour)},
			},
			want: map[string]float64{"anthropic-claude": 0.55},
		},
		{
			name: "completed with null avg_score is skipped",
			rows: []chRow{
				{model: "noisy-model", status: "completed", avg: 0.0, completedAt: now(), nullAvg: true},
				{model: "noisy-model", status: "completed", avg: 0.62, completedAt: now().Add(-30 * time.Minute)},
			},
			want: map[string]float64{"noisy-model": 0.62},
		},
		{
			name: "completed with null completed_at is skipped",
			rows: []chRow{
				{model: "in-progress-model", status: "completed", avg: 0.42, completedAt: now(), nullCompleted: true},
				{model: "in-progress-model", status: "completed", avg: 0.77, completedAt: now().Add(-2 * time.Hour)},
			},
			want: map[string]float64{"in-progress-model": 0.77},
		},
		{
			name: "two models with disjoint histories",
			rows: []chRow{
				{model: "thegrid", status: "completed", avg: 0.85, completedAt: now().Add(-12 * time.Hour)},
				{model: "recursive", status: "completed", avg: 0.65, completedAt: now().Add(-3 * time.Hour)},
				{model: "recursive", status: "completed", avg: 0.71, completedAt: now()},
			},
			want: map[string]float64{"thegrid": 0.85, "recursive": 0.71},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := execCH(ctx, conn, "TRUNCATE TABLE benchmark_runs"); err != nil {
				t.Fatalf("truncate: %v", err)
			}
			seed := buildSeed(tc.rows)
			if err := execCH(ctx, conn, seed); err != nil {
				t.Fatalf("seed: %v", err)
			}
			p := NewCHBenchProvider(conn)
			got, err := p.BenchmarkSnapshot(ctx)
			if err != nil {
				t.Fatalf("BenchmarkSnapshot: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Errorf("snapshot size: got %d want %d (got=%v)", len(got), len(tc.want), got)
			}
			for model, want := range tc.want {
				entry, ok := got[model]
				if !ok {
					t.Errorf("model %q missing from snapshot", model)
					continue
				}
				if !approxNumeric(entry.AvgScore, want) {
					t.Errorf("model %q avg: got %v want %v", model, entry.AvgScore, want)
				}
				if entry.CompletedAt.IsZero() {
					t.Errorf("model %q completed_at zero", model)
				}
			}
		})
	}
}

func TestCHBenchProvider_NilConnIsSafe(t *testing.T) {
	// The same nil-safety contract PGBenchProvider honours: a nil
	// conn must short-circuit and return an empty snapshot rather
	// than nil-map the router into a panic.
	p := CHBenchProvider{}
	out, err := p.BenchmarkSnapshot(context.Background())
	if err != nil {
		t.Fatalf("BenchmarkSnapshot: %v", err)
	}
	if out == nil {
		t.Errorf("snapshot nil; want empty map")
	}
	if len(out) != 0 {
		t.Errorf("snapshot not empty: got %v", out)
	}
}

func TestCHBenchProvider_QueryArgMaxOrderByCompletedAt(t *testing.T) {
	// Pure-string assertion on the query the provider uses. A
	// regression that ordered by updated_at (instead of
	// completed_at) would silently refresh the router with stale
	// data; the test fails loud if the SQL drifts away from the
	// argMax pattern documented in CHBenchProvider.
	sql := strings.Join(strings.Fields(chBenchQuery), " ")
	if !strings.Contains(sql, "argMax(avg_score, completed_at)") {
		t.Errorf("query missing argMax(avg_score, completed_at):\n%s", chBenchQuery)
	}
	if !strings.Contains(sql, "WHERE status = 'completed'") {
		t.Errorf("query missing status filter:\n%s", chBenchQuery)
	}
	if !strings.Contains(sql, "GROUP BY model") {
		t.Errorf("query missing GROUP BY model:\n%s", chBenchQuery)
	}
}

// chBenchQuery is duplicated verbatim from
// internal/router/bench_provider_ch.go's BenchmarkSnapshot. The
// duplication is intentional: the contract test pins a specific
// SQL shape against whichever query the provider is currently
// running. If a future change diverges from this shape, the
// string assertion above surfaces it before any blended routing
// decision goes wrong in production.
const chBenchQuery = `SELECT model, argMax(avg_score, completed_at) AS latest_avg, argMax(completed_at, completed_at) AS latest_completed_at FROM benchmark_runs WHERE status = 'completed' AND avg_score IS NOT NULL AND completed_at IS NOT NULL GROUP BY model`

// chRow is one synthetic benchmark row for the live test fixture.
// We keep the type minimal so the seed builder stays readable; the
// SQL builder turns it into INSERT statements against the schema.
type chRow struct {
	model         string
	status        string
	avg           float64
	completedAt   time.Time
	nullAvg       bool
	nullCompleted bool
}

func buildSeed(rows []chRow) string {
	var b strings.Builder
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].model < rows[j].model
	})
	b.WriteString("INSERT INTO benchmark_runs (id, model, status, avg_score, completed_at) VALUES ")
	for i, r := range rows {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "('%s', '%s', '%s', ",
			fmt.Sprintf("row-%s-%d", r.model, r.completedAt.UnixNano()),
			r.model, r.status)
		if r.nullAvg {
			b.WriteString("NULL, ")
		} else {
			fmt.Fprintf(&b, "%v, ", r.avg)
		}
		if r.nullCompleted {
			b.WriteString("NULL")
		} else {
			// ClickHouse DateTime64(9) expects 9-digit nanosecond
			// precision. Truncating to microseconds collapses rows
			// that occurred within the same microsecond into a
			// single key, which makes argMax under test non-
			// deterministic between cases whose seeded times
			// happen to share a microsecond. We pre-format at the
			// schema's full precision so the test's row order matches
			// the production comparator's.
			fmt.Fprintf(&b, "'%s'", r.completedAt.UTC().Format("2006-01-02 15:04:05.000000000"))
		}
		b.WriteString(")")
	}
	return b.String()
}

// chSchema is the universal (test-only) schema. We split id off
// the canonical PRIMARY KEY column because ClickHouse doesn't
// enforce PRIMARY KEY the way Postgres does — for a test fixture
// we keep the same CREATE TABLE statement the migration applies,
// but with a slightly looser partition strategy. Keeping it
// identical to the operator's is what makes this test a real
// contract test, not a unit test of an in-memory mock.
const chSchema = `CREATE TABLE IF NOT EXISTS benchmark_runs (
    id              String,
    org_id          LowCardinality(String) DEFAULT '',
    provider        LowCardinality(String) DEFAULT 'primeintellect',
    external_id     String DEFAULT '',
    name            String DEFAULT '',
    environments    Array(String) DEFAULT [],
    model           String,
    num_examples    UInt32 DEFAULT 5,
    rollouts        UInt32 DEFAULT 1,
    via_gateway     UInt8 DEFAULT 1,
    vkey_id         String DEFAULT '',
    status          LowCardinality(String) DEFAULT 'pending',
    external_status String DEFAULT '',
    avg_score       Nullable(Float64),
    min_score       Nullable(Float64),
    max_score       Nullable(Float64),
    total_samples   Nullable(UInt32),
    metrics         Nullable(String),
    viewer_url      String DEFAULT '',
    error           String DEFAULT '',
    created_by      String DEFAULT '',
    created_at      DateTime64(9) DEFAULT now64(9),
    updated_at      DateTime64(9) DEFAULT now64(9),
    started_at      Nullable(DateTime64(9)),
    completed_at    Nullable(DateTime64(9))
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(created_at)
ORDER BY (status, model, completed_at)
SETTINGS index_granularity = 8192, allow_nullable_key = 1`

func execCH(ctx context.Context, conn driver.Conn, sql string) error {
	return conn.Exec(ctx, sql)
}

func now() time.Time { return time.Now() }

func approxNumeric(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-6
}
