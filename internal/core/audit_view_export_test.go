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

// TestExportAuditFilterParameterBoundaries guards the c0.x #8
// input-validation surface. An attacker who can reach the export
// handler via a misuse of the admin console — or a future regression
// that lets a tenant pick their export filename — must not be able
// to:
//
//   1. Smuggle a 1-MB OrgID into the audit_log query (would break the
//      column-width assumption).
//   2. Send a CR/LF in an actor name to ride a CSV cell into a
//      header-injection attack on the operator viewing the CSV.
//   3. Send a CR/LF in a Content-Disposition filename* to mint a
//      second response header via Set-Cookie or X-.
//
// The validation rejects each case with a typed error. The opposite
// direction is also asserted: a normal OrgID/actor pair (the kind
// the audit table accepts in steady state) MUST still pass through
// view & export so legitimate operators are not collateral-damaged
// by the validation.
func TestExportAuditFilterParameterBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*AuditFilter)
		wantErr string
	}{
		{"empty-org", func(f *AuditFilter) { f.OrgID = "" }, "OrgID is required"},
		{"org-with-control", func(f *AuditFilter) { f.OrgID = "org\x07bad" }, "non-identifier"},
		{"org-with-crlf", func(f *AuditFilter) { f.OrgID = "org\r\nSet-Cookie: x" }, "non-identifier"},
		{"org-too-long", func(f *AuditFilter) {
			f.OrgID = strings.Repeat("a", 200)
		}, "exceeds 128 chars"},
		{"actor-too-long", func(f *AuditFilter) {
			f.Actor = strings.Repeat("a", 200)
		}, "Actor exceeds"},
		{"actor-with-crlf", func(f *AuditFilter) {
			f.Actor = "alice\r\nx"
		}, "disallowed characters"},
		{"action-too-long", func(f *AuditFilter) {
			f.ActionPrefix = strings.Repeat("x", 200)
		}, "ActionPrefix exceeds"},
		{"target-too-long", func(f *AuditFilter) {
			f.TargetPrefix = strings.Repeat("x", 1000)
		}, "TargetPrefix exceeds"},
		{"reqid-too-long", func(f *AuditFilter) {
			f.RequestID = strings.Repeat("x", 1000)
		}, "RequestID exceeds"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := AuditFilter{
				OrgID: "org-good",
				Limit: 10,
			}
			c.mutate(&f)
			err := validateAuditFilter(f, DefaultExportTimeSpan)
			if err == nil {
				t.Fatalf("validateAuditFilter(%+v) = nil, want error containing %q",
					f, c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %q, want substring %q", err.Error(), c.wantErr)
			}
		})
	}

	// Opposite direction: a legitimate OrgID+Actor with a sealed time
	// window MUST pass without error. Without this, the validation
	// would reject every legitimate admin query.
	good := AuditFilter{OrgID: "org-good", Actor: "alice@example.com",
		Limit: 10, FromTime: time.Now().Add(-1 * time.Hour), ToTime: time.Now()}
	if err := validateAuditFilter(good, DefaultExportTimeSpan); err != nil {
		t.Errorf("legit filter %+v was rejected: %v", good, err)
	}
}

// TestContentDispositionSanitiser covers the filename sanitisation
// path used by the export handler. An attacker-controlled filename
// could otherwise be used to mint a second response header.
func TestContentDispositionSanitiser(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"audit-export.csv", "audit-export.csv"},
		{"org-1234-2026.csv", "org-1234-2026.csv"},
		// CR/LF injection attempt → falls back to default.
		{"audit.csv\r\nSet-Cookie: y", "audit-export.csv"},
		// Embedded NUL control character.
		{"org\x00evil", "org_evil"},
		// Long filename → truncated.
		{strings.Repeat("x", 200), strings.Repeat("x", 128)},
		// Non-printable Unicode outside ASCII → falls back.
		{"audit-\xff.csv", "audit-export.csv"},
	}
	for _, c := range cases {
		if got := sanitizeContentDisposition(c.in); got != c.want {
			t.Errorf("sanitizeContentDisposition(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

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
		"=cmd|...":                   "'=cmd|...",
		"+evil":                      "'+evil",
		"-1":                         "'-1",
		"@calc":                      "'@calc",
		"hello world":                "hello world",
		"":                           "",
		"=SUM(A1:A99)/5":             "'=SUM(A1:A99)/5",
		"\tleading-tab":              " leading-tab",
		"\r\r\r":                     "",
		"plain text with = mid-line": "plain text with = mid-line",
		"\n=evil\nnewline":           " =evil newline",
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

// TestAuditExplorerCursorIndexIsDefined proves the migration created the
// index that the audit view/export query relies on for cursor pagination.
// It does NOT use EXPLAIN on a populated table: proving the planner picks
// the index at every scale would require thousands of seeded rows and
// would still fluke between Seq Scan and Index Scan based on the cost
// estimator. The honest, default-CI-safe assertions are:
//
//  1. The index exists.
//  2. Its leading column matches the WHERE clause (org_id).
//  3. Its sort order matches the ORDER BY (created_at DESC, id DESC).
//
// If those hold, the planner has the option to use the cursor. A separate
// guard at-scale test (TestAuditPlannerPicksCursorAtScale, build tag
// `audit_perf`) exercises the EXPLAIN side; default CI does not run it
// because seeding 5k rows is too slow for a per-PR gate.
//
// Requires NEXUS_TEST_POSTGRES_URL: the test_helpers fake path returns
// a stub reply (no real EXPLAIN), so without a live DB the assertion
// would fail spuriously.
func TestAuditExplorerCursorIndexIsDefined(t *testing.T) {
	if os.Getenv("NEXUS_TEST_POSTGRES_URL") == "" {
		t.Skip("NEXUS_TEST_POSTGRES_URL required for live index-shape check")
	}
	pool := initTestDB(t)
	defer pool.Close()
	ctx := context.Background()

	const want = "audit_log_org_cursor"
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes
		                WHERE schemaname = 'public'
		                  AND tablename  = 'audit_log'
		                  AND indexname  = $1)`,
		want,
	).Scan(&exists); err != nil {
		t.Fatalf("query pg_indexes: %v", err)
	}
	if !exists {
		t.Fatalf("index %q not found on audit_log; cursor pagination cannot be honored", want)
	}

	// Read the indexed columns in their stored order. column_name is
	// textual; ordering is captured via the position in indexdef.
	var def string
	if err := pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes
		 WHERE schemaname = 'public'
		   AND tablename  = 'audit_log'
		   AND indexname  = $1`,
		want,
	).Scan(&def); err != nil {
		t.Fatalf("query indexdef: %v", err)
	}
	defLower := strings.ToLower(def)
	for _, want := range []string{"org_id", "created_at desc", "id desc"} {
		if !strings.Contains(defLower, want) {
			t.Errorf("cursor index def does not contain %q; def=%s", want, def)
		}
	}
	_ = auditaggregator.WindowSize // keep import warm
}

// TestAuditExplorerIndexIsUsable is the default-CI smoke check on the
// cursor pagination query: with at least one row in the table, the
// query runs without error and returns the expected row. It does NOT
// assert planner behaviour — index usage at scale is asserted in
// TestAuditPlannerPicksCursorAtScale (build tag audit_perf), because
// Postgres cost estimates flip on table size and a per-PR-scale test
// would either flake (small) or be slow (large).
//
// Requires NEXUS_TEST_POSTGRES_URL: the test_helpers fake path returns
// a stub reply (no real EXPLAIN), so without a live DB the assertion
// would fail spuriously.
func TestAuditExplorerIndexIsUsable(t *testing.T) {
	if os.Getenv("NEXUS_TEST_POSTGRES_URL") == "" {
		t.Skip("NEXUS_TEST_POSTGRES_URL required for query-shape check")
	}
	pool := initTestDB(t)
	defer pool.Close()
	ctx := context.Background()
	s := &Store{pool: pool, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, _ = pool.Exec(ctx, `DELETE FROM audit_log`)
	s.Audit(ctx, AuditEvent{
		ActorID: "alice",
		OrgID:   "org-A",
		Action:  AuditAction("test.view"),
	})
	rows, err := ViewAudit(ctx, pool, AuditFilter{
		OrgID: "org-A",
		Limit: 1,
	})
	if err != nil {
		t.Fatalf("ViewAudit: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("ViewAudit: want 1 row, got %d", len(rows))
	}
	_ = auditaggregator.WindowSize // keep import warm
}
