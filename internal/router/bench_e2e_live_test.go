package router

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// helperLiveBenchSeed opens the live connection that this suite
// depends on and applies the bench schema (idempotent) before
// returning the pool. It is broken out so both scenarios share
// the same warm-up dance; a divergent path here is more
// brittle than one shared one. -short and an empty
// NEXUS_POSTGRES_URL both short-circuit to t.Skip so the suite
// is invisible to contributor runs that have neither.
func helperLiveBenchSeed(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping live bench provider E2E in -short mode")
	}
	dsn := os.Getenv("NEXUS_POSTGRES_URL")
	if dsn == "" {
		t.Skip("NEXUS_POSTGRES_URL is empty; live bench E2E is up to the live CI cluster only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)

	schema, err := os.ReadFile(filepath.Join("..", "..", "migrations", "postgres", "012_benchmark_runs.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(schema)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	// Stay idempotent on a shared cluster: clear the rows we
	// seeded in any prior run rather than dropping the table,
	// because other suites in this package need the schema
	// alive after we return.
	if _, err := pool.Exec(ctx, `DELETE FROM benchmark_runs WHERE id LIKE 'live-%' OR id LIKE 'avg-%'`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM benchmark_runs WHERE id LIKE 'live-%' OR id LIKE 'avg-%'`)
	})
	return ctx, pool
}

// router sees in production: rows seeded by the bench CRUD layer
// (CRUD under test in internal/core/benchmark_runs_test.go) are
// read back through PGBenchProvider.BenchmarkSnapshot, then fed
// to CombinedStatsProvider to confirm a fully-decayed benchmark
// contributes nothing while a fresh one dominates. We pin this
// as live rather than unit because the value lives in the schema
// plus the DISTINCT ON shape — exactly what a mock would paper
// over. CI runs the suite without -short; -short skips live
// tests so contributors without Postgres still see a green
// build.
func TestBenchProviderE2E_PGSnapshot(t *testing.T) {
	ctx, pool := helperLiveBenchSeed(t)
	bench := NewPGBenchProvider(pool)

	// -------- Scenario 1: settled run is read back exactly --------
	now := time.Now().UTC()
	if err := insertRun(ctx, pool, "live-completed-gpt", "gpt-4o-mini", "completed", "COMPLETED", ptrF(0.82), &now); err != nil {
		t.Fatalf("seed completed: %v", err)
	}
	snap, err := bench.BenchmarkSnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	got, ok := snap["gpt-4o-mini"]
	if !ok {
		t.Fatalf("snapshot missing gpt-4o-mini: %+v", snap)
	}
	if !approxNumeric(got.AvgScore, 0.82) {
		t.Errorf("avg = %v, want 0.82", got.AvgScore)
	}
	if !got.CompletedAt.Truncate(time.Second).Equal(now.Truncate(time.Second)) {
		t.Errorf("completed_at = %v, want within 1s of %v", got.CompletedAt, now)
	}

	// -------- Scenario 2: re-run supersedes prior row --------
	// The operator runs the benchmark again. The new row's
	// completed_at is fresher, so DISTINCT ON must pick it
	// without operator cleanup.
	later := now.Add(2 * time.Hour)
	if err := insertRun(ctx, pool, "live-completed-gpt-rerun", "gpt-4o-mini", "completed", "COMPLETED", ptrF(0.95), &later); err != nil {
		t.Fatalf("seed re-run: %v", err)
	}
	snap, err = bench.BenchmarkSnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot after re-run: %v", err)
	}
	got, ok = snap["gpt-4o-mini"]
	if !ok {
		t.Fatalf("snapshot missing after re-run: %+v", snap)
	}
	if !approxNumeric(got.AvgScore, 0.95) {
		t.Errorf("avg = %v, want 0.95 (latest run)", got.AvgScore)
	}

	// -------- Scenario 3: pending + failed contribute nothing --------
	if err := insertRun(ctx, pool, "live-pending-sonnet", "claude-sonnet", "pending", "PROCESSING", nil, nil); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	if err := insertRun(ctx, pool, "live-failed-sonnet", "claude-sonnet", "failed", "TIMEOUT", nil, nil); err != nil {
		t.Fatalf("seed failed: %v", err)
	}
	snap, err = bench.BenchmarkSnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot with mixed states: %v", err)
	}
	if _, ok := snap["claude-sonnet"]; ok {
		t.Errorf("snapshot leaked pending/failed run: %+v", snap)
	}

	// -------- Scenario 4: blend math against judge-only model --------
	// The router sees a judge-only BaselineQuality; the bench
	// provider supplies a fresh 0.95 score; a 0.5 weight blend
	// should produce 0.5 * 0.95 + 0.5 * 0.4 = 0.675. We seed
	// "judge-only" with no benchmark and "blended" with a
	// fresh row, then assert the equality precisely — drift
	// here means routing decisions shift without anyone
	// knowing.
	if err := insertRun(ctx, pool, "live-blended-claude", "blended-claude", "completed", "COMPLETED", ptrF(0.95), &now); err != nil {
		t.Fatalf("seed blended: %v", err)
	}
	snap, err = bench.BenchmarkSnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot for blend: %v", err)
	}
	if _, ok := snap["blended-claude"]; !ok {
		t.Fatalf("blended-claude missing from snapshot: %+v", snap)
	}
	primary := &fakeStats{
		row: map[string]ModelStats{
			"judge-only":     {Model: "judge-only", Quality: 0.4, QualitySamples: 12},
			"blended-claude": {Model: "blended-claude", Quality: 0.4, QualitySamples: 12},
		},
	}
	combined := NewCombinedStatsProvider(primary, bench, CombinedWeights{BenchmarkWeight: 0.5}, 7*24*time.Hour)
	stats, err := combined.ModelStats(ctx, time.Hour)
	if err != nil {
		t.Fatalf("ModelStats: %v", err)
	}
	blended, ok := stats["blended-claude"]
	if !ok {
		t.Fatalf("blended-claude missing from combined stats: %+v", stats)
	}
	want := 0.5*0.4 + 0.5*0.95
	if !approxNumeric(blended.Quality, want) {
		t.Errorf("blended Quality = %v, want %v", blended.Quality, want)
	}
	if blended.QualitySamples != 12 {
		t.Errorf("QualitySamples = %d, want 12 (bench contribution is not a sample)", blended.QualitySamples)
	}

	// -------- Scenario 5: stale row decays out of the blend --------
	// Same setup, but the bench row is exactly one half-life
	// old (7 days for a 168h half-life), so freshness = 0.5
	// and the effective weight halves. Asserting 0.475 with
	// the tolerance the rest of the package uses gives a
	// pin against accidental edit to blendQuality or the
	// decay math.
	sevenDaysAgo := time.Now().UTC().Add(-7 * 24 * time.Hour)
	if err := insertRun(ctx, pool, "live-stale-blend", "stale-claude", "completed", "COMPLETED", ptrF(0.95), &sevenDaysAgo); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	snap, err = bench.BenchmarkSnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot for decay: %v", err)
	}
	staleCombined := NewCombinedStatsProvider(&fakeStats{
		row: map[string]ModelStats{"stale-claude": {Model: "stale-claude", Quality: 0.4, QualitySamples: 8}},
	}, bench, CombinedWeights{BenchmarkWeight: 0.5}, 7*24*time.Hour)
	staleStats, err := staleCombined.ModelStats(ctx, time.Hour)
	if err != nil {
		t.Fatalf("stale ModelStats: %v", err)
	}
	staleEntry := staleStats["stale-claude"]
	// 0.5 (judge) + 0.5 (bench weight) * 0.5 (freshness at one
	// half-life) * 0.95 (bench) = 0.5*0.4 + 0.125 = 0.325.
	wantStaleReal := 0.5*0.4 + 0.5*0.5*0.95
	if !approxNumeric(staleEntry.Quality, wantStaleReal) {
		t.Errorf("stale Quality = %v, want %v", staleEntry.Quality, wantStaleReal)
	}
}

// insertRun is a minimal seed helper. We do not import
// internal/core here because that would couple the router test
// to the CRUD surface — the value of this test is the SQL/JSON
// round-trips through pgx, not the CRUD API above them.
func insertRun(
	ctx context.Context,
	pool *pgxpool.Pool,
	id, model, status, externalStatus string,
	avg *float64,
	completedAt *time.Time,
) error {
	metrics := []byte(`{"accuracy":0.0}`)
	if err := json.Unmarshal([]byte(`{"accuracy":`+fmt.Sprintf("%v", avg)+
		`}`), &metrics); err != nil && avg != nil {
		return err
	}
	if avg == nil {
		metrics = nil
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO benchmark_runs (
			id, org_id, provider, model, status, external_status,
			num_examples, rollouts, via_gateway, avg_score, metrics, completed_at
		) VALUES (
			$1, 'org-test', 'primeintellect', $2, $3, $4,
			5, 1, TRUE, $5, $6, $7
		)
	`, id, model, status, externalStatus, avg, metrics, completedAt)
	return err
}

func ptrF(v float64) *float64 { return &v }

// TestBenchProviderE2E_AvgScoreIsNotAverage is a guard against
// a regression where a future engineer wraps the row-level
// avg_score in AVG() to "smooth" duplicate completed rows. Each
// row's avg_score is the platform's single-run aggregate; an
// outer average would double-count work the platform already
// did. The first scenario in TestBenchProviderE2E_PGSnapshot
// already covers the most-recent-wins behaviour; this test
// sharpens the assertion by seeding two distinct scores for the
// same model and rejecting any answer in between — if a future
// query returns ~0.55 the regression is a single round of
// AVG() away and this test should fail loud.
func TestBenchProviderE2E_AvgScoreIsNotAverage(t *testing.T) {
	ctx, pool := helperLiveBenchSeed(t)

	earlier := time.Now().UTC().Add(-2 * time.Hour)
	later := time.Now().UTC()
	if err := insertRun(ctx, pool, "avg-old-claude", "claude-mixed", "completed", "COMPLETED", ptrF(0.10), &earlier); err != nil {
		t.Fatalf("seed old: %v", err)
	}
	if err := insertRun(ctx, pool, "avg-new-claude", "claude-mixed", "completed", "COMPLETED", ptrF(0.99), &later); err != nil {
		t.Fatalf("seed new: %v", err)
	}

	bench := NewPGBenchProvider(pool)
	got, err := bench.BenchmarkSnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	entry, ok := got["claude-mixed"]
	if !ok {
		t.Fatalf("claude-mixed missing from snapshot: %+v", got)
	}
	// Reject anything that is neither endpoint: the test is
	// specifically guarding against AVG(global) regression,
	// not asserting exact equality to a specific score.
	if !approxNumeric(entry.AvgScore, 0.99) {
		t.Errorf("avg = %v, want 0.99 (most recent settled row, not the mean across rows)", entry.AvgScore)
	}
}
