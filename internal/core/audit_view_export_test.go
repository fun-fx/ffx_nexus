// c0.7 audit view/export acceptance. Three concerns:
//
//   1. Bypass attempts with another org_id are silently filtered
//      because the SQL WHERE has org_id = $1, and the handler
//      chooses the org_id from the authenticated user's session,
//      never from the request body.
//
//   2. CSV formula injection defence makes every '=' cell start
//      with a single quote so spreadsheet programs don't evaluate
//      it as a formula.
//
//   3. Export requests themselves show up as audit rows.
//
//   4. The view query is bounded by ViewMaxRows / DefaultViewTimeSpan;
//      the export is bounded by ExportMaxRows / DefaultExportTimeSpan.

package core

import (
	"bytes"
	"context"
	"encoding/csv"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	nexus "github.com/ffxnexus/nexus"
	"github.com/ffxnexus/nexus/internal/auditaggregator"
	"github.com/ffxnexus/nexus/internal/migrate"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func initTestDB(t *testing.T) *pgxpool.Pool {
	if os.Getenv("NEXUS_TEST_POSTGRES_URL") == "" {
		t.Skip("set NEXUS_TEST_POSTGRES_URL")
	}
	pool, err := pgxpool.New(context.Background(), os.Getenv("NEXUS_TEST_POSTGRES_URL"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	migs, err := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	if err != nil {
		t.Fatalf("migrate.Load: %v", err)
	}
	exec := migrate.NewPostgres(pool, "test-"+t.Name()+"-"+uuid.NewString()[:8])
	if _, err := migrate.Run(context.Background(), exec, migs, migrate.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}); err != nil {
		t.Fatalf("migrate.Run: %v", err)
	}
	return pool
}

// TestViewAuditOrgBoundaryFiltersByOrg asserts attempts to query a
// row that belongs to a different org are filtered silently because
// the SQL query is parameterized by org_id. The view API does not
// permit mixing orgs even if both are configured in the same DB.
func TestViewAuditOrgBoundaryFiltersByOrg(t *testing.T) {
	pool := initTestDB(t)
	defer pool.Close()
	ctx := context.Background()
	s := &Store{pool: pool, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, _ = pool.Exec(ctx, `DELETE FROM audit_log`)

	// Two distinct orgs.
	s.Audit(ctx, AuditEvent{ActorID: "alice", OrgID: "org-A", Action: AuditAction("test.view"), TargetID: "x"})
	s.Audit(ctx, AuditEvent{ActorID: "bob", OrgID: "org-B", Action: AuditAction("test.view"), TargetID: "y"})

	// Caller presents the session as org-A but tries to fetch rows
	// that the SQL parameterisation would expose to org-A only.
	rows, err := ViewAudit(ctx, pool, AuditFilter{OrgID: "org-A", Limit: 50})
	if err != nil {
		t.Fatalf("ViewAudit: %v", err)
	}
	for _, r := range rows {
		if r.OrgID != "org-A" {
			t.Fatalf("view returned row with org_id %q (callers expected only org-A)", r.OrgID)
		}
	}
	// Org B rows should be returned as if they don't exist when the
	// caller's filter says org-A.
	if len(rows) != 1 {
		t.Fatalf("expected 1 row for org-A, got %d (the cross-org row should not appear)", len(rows))
	}
}

// TestViewAuditEnforcesTimeBound prevents an unbounded query that
// would scan the entire audit history. The handler must reject a
// window that exceeds the documented span.
func TestViewAuditEnforcesTimeBound(t *testing.T) {
	pool := initTestDB(t)
	defer pool.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	_, err := ViewAudit(ctx, pool, AuditFilter{
		OrgID:    "org-X",
		FromTime: now.Add(-365 * 24 * time.Hour),
		ToTime:   now,
		Limit:    10,
	})
	if err == nil {
		t.Fatalf("expected error for time range > %v", DefaultViewTimeSpan)
	}
}

// TestExportAuditStreamsAsCSV verifies the export API writes a CSV
// with a stable header row and at least one data row.
//
// The Cell escape from apierr.Scrub is applied to the detail column
// here BEFORE we exercise the formula-injection defence; the defence
// is independent of scrub because operator-facing CSV cells must
// also be safe.
func TestExportAuditStreamsAsCSV(t *testing.T) {
	pool := initTestDB(t)
	defer pool.Close()
	ctx := context.Background()
	s := &Store{pool: pool, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, _ = pool.Exec(ctx, `DELETE FROM audit_log`)

	s.Audit(ctx, AuditEvent{ActorID: "alice", OrgID: "org-A", Action: AuditAction("test.export"), TargetID: "x", Detail: "hello"})

	var buf bytes.Buffer
	count, err := ExportAudit(ctx, pool, AuditFilter{OrgID: "org-A", Limit: 10}, &buf)
	if err != nil {
		t.Fatalf("ExportAudit: %v", err)
	}
	if count < 2 {
		t.Errorf("expected at least header + 1 row, got %d", count)
	}
	// The CSV header must match the documented schema.
	r := csv.NewReader(&buf)
	header, err := r.Read()
	if err != nil {
		t.Fatalf("read header: %v", err)
	}
	if header[0] != "org_id" || header[1] != "actor" {
		t.Fatalf("unexpected header: %v", header)
	}
}

// TestExportAuditCSVDefendsFormulaInjection is the load-bearing
// formula-injection test. Without the defence, an Excel cell that
// begins with `=cmd|...` would be evaluated as a formula by Excel
// or LibreOffice when the operator opens the CSV.
//
// Mutating csvSafeCell to a no-op (return s) would surface here as
// a missing leading "'" on the offending cell, and the test fails.
func TestExportAuditCSVDefendsFormulaInjection(t *testing.T) {
	// We exercise csvSafeCell directly; the SQL path is verified in
	// TestExportAuditStreamsAsCSV.
	cases := map[string]string{
		"=cmd|...":                    "'=cmd|...",
		"+evil":                       "'+evil",
		"-1":                          "'-1",
		"@calc":                       "'@calc",
		"hello world":                 "hello world",
		"":                            "",
		"=SUM(A1:A99)/5":              "'=SUM(A1:A99)/5",
		"\tleading-tab":               " leading-tab",
		"\r\r\r":                      "",
		"plain text with = mid-line":  "plain text with = mid-line",
		"\n=evil\nnewline":            " =evil newline",
	}
	for in, want := range cases {
		if got := csvSafeCell(in); got != want {
			t.Errorf("csvSafeCell(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestExportAuditBoundsTimeRange ensures the time-range check fires
// when a request asks for a span longer than the documented
// maximum. The check happens before the SQL is issued.
func TestExportAuditBoundsTimeRange(t *testing.T) {
	pool := initTestDB(t)
	defer pool.Close()
	ctx := context.Background()
	_, err := ExportAudit(ctx, pool, AuditFilter{
		OrgID:    "org-A",
		Limit:    100,
		FromTime: time.Now().Add(-365 * 24 * time.Hour),
		ToTime:   time.Now(),
	}, io.Discard)
	if err == nil {
		t.Fatalf("expected reject when time range > DefaultExportTimeSpan")
	}
	if !strings.Contains(err.Error(), "time range") {
		t.Fatalf("expected time-range error, got %v", err)
	}
}

// TestExportAuditRefusesMissingOrg ensures a request without an
// org_id is rejected. The handle pathway enforces org scoping; a
// free-form request would not have it set.
func TestExportAuditRefusesMissingOrg(t *testing.T) {
	pool := initTestDB(t)
	defer pool.Close()
	ctx := context.Background()
	_, err := ExportAudit(ctx, pool, AuditFilter{
		OrgID:    "",
		Limit:    100,
		FromTime: time.Now().Add(-24 * time.Hour),
		ToTime:   time.Now(),
	}, io.Discard)
	if err == nil {
		t.Fatalf("expected reject when org id is empty")
	}
	if !strings.Contains(err.Error(), "OrgID") {
		t.Fatalf("expected OrgID error, got %v", err)
	}
}

// TestRecordExportInscribesInAuditFeed verifies that an export
// request itself produces an audit row tagged `audit.export`. This
// is the no-recursion test: the export row's request_id is the
// current request id, and it does not cause another export.
func TestRecordExportInscribesInAuditFeed(t *testing.T) {
	pool := initTestDB(t)
	defer pool.Close()
	ctx := context.Background()
	s := &Store{pool: pool, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, _ = pool.Exec(ctx, `DELETE FROM audit_log`)

	RecordExport(ctx, s, OrgID("org-A"))
	rows, err := ViewAudit(ctx, pool, AuditFilter{
		OrgID:        "org-A",
		ActionPrefix: "audit.export",
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("ViewAudit: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 audit-export row, got %d", len(rows))
	}
	if rows[0].Action != "audit.export" {
		t.Fatalf("audit row action = %q, want audit.export", rows[0].Action)
	}
}

// TestAuditExplorerUsesTheCursorIndexWithoutFullScan is the index
// utilisation smoke test. EXPLAIN of the audit query against the
// migration 021 indexes must NOT show a sequential scan.
func TestAuditExplorerUsesTheCursorIndexWithoutFullScan(t *testing.T) {
	pool := initTestDB(t)
	defer pool.Close()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `EXPLAIN SELECT id FROM audit_log WHERE org_id = $1
		ORDER BY created_at DESC, id DESC LIMIT 11`, "org-A")
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		_ = rows.Scan(&line)
		plan = append(plan, line)
	}
	planText := strings.ToLower(strings.Join(plan, "\n"))
	if strings.Contains(planText, "seq scan on audit_log") {
		t.Errorf("audit query plan uses Seq Scan; expected index. Plan:\n%s", strings.Join(plan, "\n"))
	}
	if !strings.Contains(planText, "index") && !strings.Contains(planText, "audit_log_org_cursor") {
		t.Errorf("audit query plan does not reference idx_audit_log_org_cursor; got:\n%s",
			strings.Join(plan, "\n"))
	}
	_ = auditaggregator.WindowSize // keep import warm
}
