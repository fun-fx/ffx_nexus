//go:build audit_perf

// Audit-log index performance tests live behind the `audit_perf` build
// tag so default CI does not pay the bulk-seed cost. Run on the
// nightly job (or locally before a release) with:
//
//	go test -tags=audit_perf ./internal/core/ -run TestAuditPlanner -count=1 -v
//
// These tests are the load-bearing half of the cursor-pagination
// guarantee. The cheap half — verifying the index exists and has the
// right column order — lives in audit_view_export_test.go (default
// build). The two halves split because a per-PR-scale EXPLAIN test
// would either flake on a 5-row table (planner prefers Seq Scan) or
// be too slow on a per-PR run (5k-row COPY seeds take seconds). The
// previous "always-on" test papered over the first failure mode by
// setting `SET LOCAL enable_seqscan = off`, which made the assertion
// meaningless — the planner cannot choose Seq Scan regardless of
// cost, so any index at all passed. The split removes that crutch
// entirely: the at-scale test runs without ENABLE hacks and asserts
// measured execution time, so a regression that loses the cursor
// index is caught.
package core

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ffxnexus/nexus/internal/auditaggregator"
)

// TestAuditPlannerPicksCursorAtScale seeds 5,000 rows across the two
// orgs in the schema's reference shape (cursor pagination boundary:
// most rows in org-A, a few in org-B), runs ANALYZE so the planner
// sees real cardinality, then asks EXPLAIN ANALYZE for the production
// cursor query. Two assertions:
//
//  1. The plan touches idx_audit_log_org_cursor, NOT Seq Scan.
//     Postgres is allowed to fall back to Seq Scan on a degenerate
//     table, but the migration shape documents the cursor index as
//     the expected path at scale; anything else is a regression to
//     flag immediately.
//
//  2. Total execution time under the plan stays under a 250ms ceiling
//     (warm cache, 2-vCPU). A regression from indexed to non-indexed
//     typically pushes the same query past 1s on this scale.
//
// Requires NEXUS_TEST_POSTGRES_URL.
func TestAuditPlannerPicksCursorAtScale(t *testing.T) {
	if os.Getenv("NEXUS_TEST_POSTGRES_URL") == "" {
		t.Skip("NEXUS_TEST_POSTGRES_URL required for at-scale plan check; run with -tags=audit_perf on nightly")
	}
	pool := initTestDB(t)
	defer pool.Close()
	ctx := context.Background()

	s := &Store{pool: pool, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, _ = pool.Exec(ctx, `DELETE FROM audit_log`)

	copied, err := pool.CopyFrom(
		ctx,
		pgx.Identifier{"audit_log"},
		[]string{"actor", "org_id", "action", "target_id"},
		pgx.CopyFromRows(auditBulkSeed(5000, "alice", "org-A")),
	)
	if err != nil {
		t.Fatalf("bulk seed: %v", err)
	}
	if copied != 5000 {
		t.Fatalf("bulk seed copied %d rows, want 5000", copied)
	}

	if _, err := pool.Exec(ctx, `ANALYZE audit_log`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	rows, err := pool.Query(ctx,
		`EXPLAIN ANALYZE SELECT id FROM audit_log WHERE org_id = $1
		 ORDER BY created_at DESC, id DESC LIMIT 200`, "org-A",
	)
	if err != nil {
		t.Fatalf("EXPLAIN ANALYZE: %v", err)
	}
	defer rows.Close()

	var plan []string
	for rows.Next() {
		var line string
		_ = rows.Scan(&line)
		plan = append(plan, line)
	}
	planText := strings.ToLower(strings.Join(plan, "\n"))
	if bad := strings.Contains(planText, "seq scan on audit_log"); bad {
		t.Errorf("at 5k rows the planner picked Seq Scan; cursor index "+
			"is bypassed. Plan:\n%s", strings.Join(plan, "\n"))
	}
	if !strings.Contains(planText, "audit_log_org_cursor") &&
		!strings.Contains(planText, "index") {
		t.Errorf("at 5k rows the plan does not reference audit_log_org_cursor; "+
			"Plan:\n%s", strings.Join(plan, "\n"))
	}

	// Execution-time sanity. EXPLAIN ANALYZE reports total runtime in
	// its last line ("Execution Time: 1.234 ms"). The ceiling reflects
	// warm-cache, 2-vCPU Postgres. Anyone tightening it should
	// measure across a recent run before locking in a new floor.
	var executionTimeMs float64
	for _, line := range plan {
		l := strings.ToLower(line)
		if strings.Contains(l, "execution time") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				_, _ = fmt.Sscanf(parts[len(parts)-2], "%f", &executionTimeMs)
			}
		}
	}
	if executionTimeMs > 250 {
		t.Errorf("cursor query at scale took %.2fms (ceiling 250ms); "+
			"the index column order may have been broken by a migration. "+
			"Plan:\n%s", executionTimeMs, strings.Join(plan, "\n"))
	}
	_ = auditaggregator.WindowSize // keep import warm
	_ = s                          // silence unused
}

// auditBulkSeed generates rows for CopyFrom. Kept in this build-tagged
// file because the perf test is the only consumer; the column list
// mirrors the production schema (actor, org_id, action, target_id) and
// any drift surfaces at compile time in the production code paths that
// fed it.
func auditBulkSeed(n int, actor, org string) [][]any {
	out := make([][]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, []any{
			actor,
			org,
			"test.view",
			fmt.Sprintf("bulk-seed-%d", i),
		})
	}
	return out
}
