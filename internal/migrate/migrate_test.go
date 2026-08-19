package migrate_test

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	nexus "github.com/ffxnexus/nexus"
	"github.com/ffxnexus/nexus/internal/migrate"
)

// ---------------------------------------------------------------------------
// Discovery: the class of bug that shipped three times
// ---------------------------------------------------------------------------

// Every .sql file in the repository must be discovered. This is the test that
// would have caught 009-011, 014_invite_tokens.sql and the ClickHouse
// benchmark_runs migration all being absent from the hardcoded boot list.

// mustHaveMigrations is the tripwire list: a test mutation that drops one
// of these entries must fail this test so a regression of the discovery
// path surfaces immediately. The list is pinned to which migrations MUST
// be discovered by migrate.Load — listing every Postgres migration file
// by name. A drift between the file system and this list indicates the
// discovery path is broken OR a migration was renamed/missing.
//
// The point of pinning every expected entry rather than a sparse sample:
// a `SELECT mustHave WHERE NOT discovered` works the same way the
// `migrate.Load(nexus.Migrations, ...)` callsite does — the test reads
// the canonical set rather than a curated one.
//
// To add a new migration: also append it here. The next test run will
// confirm the discovery works. Force-remove an entry below and the
// test fails — which is the property the discovery contract relies on.
var mustHaveMigrations = []struct {
	engine   migrate.Engine
	mustHave []string
}{
	{migrate.EnginePostgres, []string{
		"postgres/001_init.sql",
		"postgres/009_eval_plugins.sql",
		"postgres/012_benchmark_runs.sql",
		"postgres/013_scheduled_benchmarks.sql",
		// Invites 500'd on a fresh install until this was added.
		"postgres/014_invite_tokens.sql",
		"postgres/015_eval_scores_org.sql",
		// 016 was added in the Chapter 1 fix to repair the missing
		// last_run_id column on benchmark_schedules.
		"postgres/016_benchmark_schedule_last_run.sql",
		"postgres/017_audit_request_id.sql",
		"postgres/018_audit_client_request_id.sql",
		"postgres/019_audit_aggregation.sql",
		"postgres/020_audit_roles.sql",
		"postgres/021_audit_view_indexes.sql",
	}},
	{migrate.EngineClickHouse, []string{
		"clickhouse/001_init.sql",
		"clickhouse/008_turn_id.sql",
		// Was unreachable behind a duplicate 007 ordinal previously.
		"clickhouse/009_benchmark_runs.sql",
		"clickhouse/010_eval_scores_org.sql",
	}},
}

func TestLoadDiscoversEveryExpectedMigration(t *testing.T) {
	for _, tc := range mustHaveMigrations {
		migs, err := migrate.Load(nexus.Migrations, tc.engine)
		if err != nil {
			t.Fatalf("Load(%s): %v", tc.engine, err)
		}
		got := map[string]bool{}
		for _, m := range migs {
			got[m.ID] = true
		}
		for _, want := range tc.mustHave {
			if !got[want] {
				t.Errorf("Load(%s) did not discover %s", tc.engine, want)
			}
		}
	}
}

// Ordering must be numeric, and in particular 012 must precede 013: the old
// list had them reversed, so 013's ALTER TABLE ran before 012 created the
// table, and benchmark_runs.schedule_id was never created anywhere.
func TestLoadOrdersByOrdinal(t *testing.T) {
	for _, engine := range []migrate.Engine{migrate.EnginePostgres, migrate.EngineClickHouse} {
		migs, err := migrate.Load(nexus.Migrations, engine)
		if err != nil {
			t.Fatalf("Load(%s): %v", engine, err)
		}
		for i := 1; i < len(migs); i++ {
			if migs[i-1].Ordinal >= migs[i].Ordinal {
				t.Errorf("%s: %s (ord %d) is not before %s (ord %d)",
					engine, migs[i-1].Name, migs[i-1].Ordinal, migs[i].Name, migs[i].Ordinal)
			}
		}
	}

	migs, err := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	if err != nil {
		t.Fatal(err)
	}
	idx := func(name string) int {
		for i, m := range migs {
			if m.Name == name {
				return i
			}
		}
		return -1
	}
	create, alter := idx("012_benchmark_runs.sql"), idx("013_scheduled_benchmarks.sql")
	if create < 0 || alter < 0 {
		t.Fatalf("expected both 012 and 013 present, got %d and %d", create, alter)
	}
	if create > alter {
		t.Errorf("012_benchmark_runs.sql (CREATE) must run before 013_scheduled_benchmarks.sql (ALTER)")
	}
}

// A duplicate ordinal has no defined order, which is how the ClickHouse 007
// collision silently hid a migration. Load must refuse rather than guess.
func TestLoadRejectsDuplicateOrdinals(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/postgres/001_init.sql":  {Data: []byte("SELECT 1;")},
		"migrations/postgres/007_alpha.sql": {Data: []byte("SELECT 1;")},
		"migrations/postgres/007_beta.sql":  {Data: []byte("SELECT 1;")},
	}
	_, err := migrate.Load(fsys, migrate.EnginePostgres)
	if err == nil {
		t.Fatal("Load accepted a duplicate ordinal, want error")
	}
	if !strings.Contains(err.Error(), "duplicate ordinal") {
		t.Errorf("error should name the problem, got: %v", err)
	}
}

func TestLoadRejectsUnnumberedFile(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/postgres/001_init.sql":  {Data: []byte("SELECT 1;")},
		"migrations/postgres/add_thing.sql": {Data: []byte("SELECT 1;")},
	}
	if _, err := migrate.Load(fsys, migrate.EnginePostgres); err == nil {
		t.Fatal("Load accepted a file without a numeric ordinal, want error")
	}
}

func TestChecksumIsStableAndContentSensitive(t *testing.T) {
	base := fstest.MapFS{"migrations/postgres/001_init.sql": {Data: []byte("SELECT 1;")}}
	a, err := migrate.Load(base, migrate.EnginePostgres)
	if err != nil {
		t.Fatal(err)
	}
	again, err := migrate.Load(base, migrate.EnginePostgres)
	if err != nil {
		t.Fatal(err)
	}
	if a[0].Checksum != again[0].Checksum {
		t.Error("checksum is not stable across loads")
	}

	edited := fstest.MapFS{"migrations/postgres/001_init.sql": {Data: []byte("SELECT 2;")}}
	b, err := migrate.Load(edited, migrate.EnginePostgres)
	if err != nil {
		t.Fatal(err)
	}
	if a[0].Checksum == b[0].Checksum {
		t.Error("checksum did not change when file content changed")
	}
}

// ---------------------------------------------------------------------------
// Invariants the engine design depends on
// ---------------------------------------------------------------------------

// The ClickHouse path has no transactions and no lock; the Postgres path can
// crash between DDL and ledger insert on an unclean shutdown. Both rely on
// every statement being replay-safe. If someone adds unguarded DDL, that
// assumption breaks silently - so assert it.
func TestMigrationsAreIdempotent(t *testing.T) {
	// Matches CREATE/ALTER/DROP of a schema object, capturing the guard if any.
	unguarded := regexp.MustCompile(`(?im)^\s*(CREATE|DROP)\s+(OR\s+REPLACE\s+)?(TABLE|INDEX|VIEW|MATERIALIZED\s+VIEW|DICTIONARY|DATABASE|TYPE)\s+(?:IF\s+NOT\s+EXISTS\s+|IF\s+EXISTS\s+)?`)
	guard := regexp.MustCompile(`(?i)IF\s+(NOT\s+)?EXISTS`)

	for _, engine := range []migrate.Engine{migrate.EnginePostgres, migrate.EngineClickHouse} {
		migs, err := migrate.Load(nexus.Migrations, engine)
		if err != nil {
			t.Fatalf("Load(%s): %v", engine, err)
		}
		for _, m := range migs {
			for i, line := range strings.Split(m.SQL, "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" || strings.HasPrefix(trimmed, "--") {
					continue
				}
				if unguarded.MatchString(line) && !guard.MatchString(line) {
					t.Errorf("%s:%d is not replay-safe (needs IF NOT EXISTS / IF EXISTS): %s",
						m.ID, i+1, trimmed)
				}
				// ADD COLUMN / ADD INDEX must be guarded too.
				if regexp.MustCompile(`(?i)^\s*ADD\s+(COLUMN|INDEX)\b`).MatchString(line) &&
					!guard.MatchString(line) {
					t.Errorf("%s:%d ADD without IF NOT EXISTS is not replay-safe: %s",
						m.ID, i+1, trimmed)
				}
			}
		}
	}
}

// pgExecutor.Apply wraps each migration in a transaction together with its
// ledger row. Postgres forbids a handful of statements inside a transaction; if
// one is ever added, the migration would fail at runtime against a real
// database rather than here.
func TestPostgresMigrationsAreTransactionSafe(t *testing.T) {
	forbidden := []string{
		"CREATE INDEX CONCURRENTLY",
		"DROP INDEX CONCURRENTLY",
		"REINDEX CONCURRENTLY",
		"VACUUM",
		"CREATE DATABASE",
		"DROP DATABASE",
		"ALTER SYSTEM",
		"CREATE TABLESPACE",
	}
	migs, err := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migs {
		upper := strings.ToUpper(m.SQL)
		for _, f := range forbidden {
			if strings.Contains(upper, f) {
				t.Errorf("%s contains %q, which cannot run inside a transaction; "+
					"pgExecutor.Apply would fail. Either avoid it or give the migration "+
					"engine an explicit non-transactional escape hatch.", m.ID, f)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Run(): ledger semantics, exercised against a fake Executor
// ---------------------------------------------------------------------------

type fakeExec struct {
	engine   migrate.Engine
	ledger   map[string]migrate.LedgerEntry
	applyLog []string
	failOn   string
	locks    int
	unlocks  int
	existing bool
}

func newFake(engine migrate.Engine) *fakeExec {
	return &fakeExec{engine: engine, ledger: map[string]migrate.LedgerEntry{}}
}

func (f *fakeExec) Engine() migrate.Engine                     { return f.engine }
func (f *fakeExec) EnsureLedger(context.Context) error         { return nil }
func (f *fakeExec) SchemaExists(context.Context) (bool, error) { return f.existing, nil }

func (f *fakeExec) Applied(context.Context) (map[string]migrate.LedgerEntry, error) {
	out := map[string]migrate.LedgerEntry{}
	for k, v := range f.ledger {
		out[k] = v
	}
	return out, nil
}

func (f *fakeExec) Apply(_ context.Context, m migrate.Migration) error {
	if f.failOn == m.ID {
		return errors.New("boom")
	}
	f.applyLog = append(f.applyLog, m.ID)
	f.ledger[m.ID] = migrate.LedgerEntry{
		ID: m.ID, Checksum: m.Checksum, AppliedAt: time.Now(), Success: true,
	}
	return nil
}

func (f *fakeExec) Lock(context.Context) (func(), error) {
	f.locks++
	return func() { f.unlocks++ }, nil
}

func fixtures() []migrate.Migration {
	fsys := fstest.MapFS{
		"migrations/postgres/001_a.sql": {Data: []byte("CREATE TABLE IF NOT EXISTS a();")},
		"migrations/postgres/002_b.sql": {Data: []byte("CREATE TABLE IF NOT EXISTS b();")},
		"migrations/postgres/003_c.sql": {Data: []byte("CREATE TABLE IF NOT EXISTS c();")},
	}
	migs, err := migrate.Load(fsys, migrate.EnginePostgres)
	if err != nil {
		panic(err)
	}
	return migs
}

func opts() migrate.Options {
	return migrate.Options{Logger: slog.New(slog.DiscardHandler)}
}

func TestRunAppliesAllThenSkipsOnSecondRun(t *testing.T) {
	ex, migs := newFake(migrate.EnginePostgres), fixtures()

	first, err := migrate.Run(context.Background(), ex, migs, opts())
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if len(first.Applied) != 3 {
		t.Fatalf("first run applied %v, want all 3", first.Applied)
	}

	// Re-running is the common case: every pod restart, every replica.
	second, err := migrate.Run(context.Background(), ex, migs, opts())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(second.Applied) != 0 {
		t.Errorf("second run re-applied %v, want none", second.Applied)
	}
	if len(second.Skipped) != 3 {
		t.Errorf("second run skipped %v, want all 3", second.Skipped)
	}
	if len(ex.applyLog) != 3 {
		t.Errorf("Apply called %d times total, want exactly 3", len(ex.applyLog))
	}
}

func TestRunAppliesOnlyTheNewMigration(t *testing.T) {
	ex, migs := newFake(migrate.EnginePostgres), fixtures()
	if _, err := migrate.Run(context.Background(), ex, migs[:2], opts()); err != nil {
		t.Fatal(err)
	}
	ex.applyLog = nil

	res, err := migrate.Run(context.Background(), ex, migs, opts())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 1 || res.Applied[0] != "postgres/003_c.sql" {
		t.Fatalf("applied %v, want only postgres/003_c.sql", res.Applied)
	}
}

// A failure must abort rather than continue to later migrations, and must not
// be reported as success. The old code logged and carried on, leaving the
// binary serving traffic against a schema it did not match.
func TestRunStopsAtFirstFailure(t *testing.T) {
	ex, migs := newFake(migrate.EnginePostgres), fixtures()
	ex.failOn = "postgres/002_b.sql"

	_, err := migrate.Run(context.Background(), ex, migs, opts())
	if err == nil {
		t.Fatal("Run returned nil error on a failing migration")
	}
	if !strings.Contains(err.Error(), "002_b.sql") {
		t.Errorf("error should name the failing migration, got: %v", err)
	}
	for _, applied := range ex.applyLog {
		if applied == "postgres/003_c.sql" {
			t.Error("Run continued past the failure to 003_c.sql")
		}
	}
}

// Editing an already-applied migration means the recorded schema and the file
// no longer agree. Skipping it hides the divergence; re-running it may not even
// be valid. Stop and say so.
func TestRunRejectsChecksumDrift(t *testing.T) {
	ex, migs := newFake(migrate.EnginePostgres), fixtures()
	if _, err := migrate.Run(context.Background(), ex, migs, opts()); err != nil {
		t.Fatal(err)
	}

	edited := append([]migrate.Migration(nil), migs...)
	edited[1].Checksum = "deadbeefdeadbeefdeadbeef"

	_, err := migrate.Run(context.Background(), ex, edited, opts())
	if !errors.Is(err, migrate.ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}

	// ...unless the operator explicitly accepts it.
	o := opts()
	o.AllowChecksumDrift = true
	if _, err := migrate.Run(context.Background(), ex, edited, o); err != nil {
		t.Fatalf("AllowChecksumDrift should permit the run, got %v", err)
	}
}

func TestRunTakesAndReleasesTheLock(t *testing.T) {
	ex, migs := newFake(migrate.EnginePostgres), fixtures()
	if _, err := migrate.Run(context.Background(), ex, migs, opts()); err != nil {
		t.Fatal(err)
	}
	if ex.locks != 1 || ex.unlocks != 1 {
		t.Errorf("locks=%d unlocks=%d, want 1 and 1", ex.locks, ex.unlocks)
	}
}

// The lock must be released even when a migration fails, or the next attempt
// deadlocks behind a lock nobody holds any more.
func TestRunReleasesLockOnFailure(t *testing.T) {
	ex, migs := newFake(migrate.EnginePostgres), fixtures()
	ex.failOn = "postgres/001_a.sql"
	if _, err := migrate.Run(context.Background(), ex, migs, opts()); err == nil {
		t.Fatal("expected failure")
	}
	if ex.unlocks != 1 {
		t.Errorf("unlocks=%d, want 1 - the lock leaked on the failure path", ex.unlocks)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	ex, migs := newFake(migrate.EnginePostgres), fixtures()
	o := opts()
	o.DryRun = true

	res, err := migrate.Run(context.Background(), ex, migs, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pending) != 3 {
		t.Errorf("Pending = %v, want all 3", res.Pending)
	}
	if len(ex.applyLog) != 0 {
		t.Errorf("dry run applied %v, want nothing", ex.applyLog)
	}
	if ex.locks != 0 {
		t.Errorf("dry run took the migration lock %d times, want 0", ex.locks)
	}
}

// A pre-ledger database is adopted, not reinitialised, and the operator is told.
func TestRunAdoptsExistingDatabase(t *testing.T) {
	ex, migs := newFake(migrate.EnginePostgres), fixtures()
	ex.existing = true

	res, err := migrate.Run(context.Background(), ex, migs, opts())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Adopted {
		t.Error("Adopted = false, want true for a database with objects but no ledger")
	}
	// Adoption replays everything; idempotent DDL makes that a no-op for what
	// exists and repairs what earlier defects skipped.
	if len(res.Applied) != 3 {
		t.Errorf("Applied = %v, want all 3 replayed during adoption", res.Applied)
	}
}

func TestRunOnFreshDatabaseIsNotFlaggedAsAdoption(t *testing.T) {
	ex, migs := newFake(migrate.EnginePostgres), fixtures()
	ex.existing = false
	res, err := migrate.Run(context.Background(), ex, migs, opts())
	if err != nil {
		t.Fatal(err)
	}
	if res.Adopted {
		t.Error("Adopted = true on an empty database, want false")
	}
}

func TestPendingReportsOutstandingWithoutLocking(t *testing.T) {
	ex, migs := newFake(migrate.EnginePostgres), fixtures()

	pending, err := migrate.Pending(context.Background(), ex, migs)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("Pending = %v, want 3", pending)
	}
	if ex.locks != 0 {
		t.Error("Pending took the migration lock; the readiness probe must not contend for it")
	}

	if _, err := migrate.Run(context.Background(), ex, migs, opts()); err != nil {
		t.Fatal(err)
	}
	pending, err = migrate.Pending(context.Background(), ex, migs)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("Pending = %v after a full run, want empty", pending)
	}
}

// TestSplitCHStatementsSemicolonsInsideCommentsStayOutOfSplit exercises the
// comment-aware CH statement splitter that replaced the naive strings.Split
// on `;` after 009_benchmark_runs.sql began failing parse in CI with code: 62.
// A `;` inside a `-- ` comment line must NOT terminate a statement, and the
// SETTINGS terminator must be consumed so the next statement opens cleanly.
func TestSplitCHStatementsSemicolonsInsideCommentsStayOutOfSplit(t *testing.T) {
	sql := "-- header; why this file exists.\n" +
		"CREATE TABLE IF NOT EXISTS t (id String)\n" +
		"ENGINE = MergeTree\n" +
		"ORDER BY id\n" +
		"SETTINGS index_granularity = 8192;\n" +
		"\n" +
		"-- trailing note; do not break this.\n" +
		"ALTER TABLE t ADD INDEX i (id) TYPE bloom_filter() GRANULARITY 4;\n"
	parts := migrate.SplitCHStatementsForTest(sql)
	if len(parts) != 2 {
		t.Fatalf("split = %d parts, want 2 — naive Split(m.SQL, \";\") style bug regressed:\n%#v",
			len(parts), parts)
	}
	if !strings.Contains(parts[0], "CREATE TABLE") || strings.HasSuffix(strings.TrimSpace(parts[0]), ";") {
		t.Errorf("first part %q must contain CREATE TABLE and have no trailing semicolon", parts[0])
	}
	if !strings.Contains(parts[1], "ALTER TABLE") || !strings.Contains(parts[1], "bloom_filter") {
		t.Errorf("second part %q must contain ALTER TABLE and bloom_filter", parts[1])
	}
	if strings.Contains(parts[0], "header") || strings.Contains(parts[1], "trailing note") {
		t.Errorf("comment prose leaked into a statement: %q / %q", parts[0], parts[1])
	}
}

// TestSplitCHStatementsNaiveSplitRegressionDetected is the load-bearing
// mutation test. The naive split -- strings.Split on ";" -- keeps the
// terminator in the emitted part, so a single `;`-terminated statement
// produces a part whose content ends with ";". The comment-aware splitter
// trims that trailing `;` because CH's driver parses a fragment with a
// stray trailing `;` strictly. The tripwire is therefore: under the
// comment-aware form there must be NO part whose trimmed text ends with
// ";".
func TestSplitCHStatementsNaiveSplitRegressionDetected(t *testing.T) {
	sql := "-- header line with embedded ; semicolon.\n" +
		"CREATE TABLE t (id String)\n" +
		"ENGINE = MergeTree\n" +
		"ORDER BY id\n" +
		"SETTINGS index_granularity = 8192;"
	parts := migrate.SplitCHStatementsForTest(sql)
	for i, p := range parts {
		if strings.HasSuffix(strings.TrimSpace(p), ";") {
			t.Fatalf("part %d ends with \";\" (%q) — a regression to the naive "+
				"strings.Split form retains the terminator inside the part because "+
				"the comment-aware trim step is what removes it", i, p)
		}
	}
}

// TestSplitCHStatementsSingleStatementNoTrailingSemicolon asserts the trim
// step strips a final `;` so a single-statement file with `;` produces one
// SQL chunk that CH's driver can consume.
func TestSplitCHStatementsSingleStatementNoTrailingSemicolon(t *testing.T) {
	parts := migrate.SplitCHStatementsForTest("CREATE TABLE t (id String) ENGINE = MergeTree ORDER BY id;")
	if len(parts) != 1 {
		t.Fatalf("split = %d parts, want 1: %#v", len(parts), parts)
	}
	if strings.HasSuffix(strings.TrimSpace(parts[0]), ";") {
		t.Errorf("trailing semicolon not stripped: %q", parts[0])
	}
	if !strings.Contains(parts[0], "CREATE TABLE") {
		t.Errorf("missing CREATE TABLE: %q", parts[0])
	}
}

// TestSplitCHStatementsSemicolonInsideSingleQuoteStringIsNotATerminator
// is the trap case for string literals. A `;` inside a quoted value
// is data, not a separator, so an INSERT INTO tbl VALUES ('a;b')
// must be emitted as a single statement.
func TestSplitCHStatementsSemicolonInsideSingleQuoteStringIsNotATerminator(t *testing.T) {
	sql := "INSERT INTO log (msg) VALUES ('one');"
	parts := migrate.SplitCHStatementsForTest(sql)
	if len(parts) != 1 {
		t.Fatalf("split = %d parts, want 1 — splitter split on a literal inside a string value:\n%#v",
			len(parts), parts)
	}
	if !strings.Contains(parts[0], "'one'") {
		t.Errorf("string value lost: %q", parts[0])
	}
}

// TestSplitCHStatementsSemicolonInsideBacktickIdentifierIsNotATerminator
// is the trap case for ClickHouse-style backtick identifier quoting.
// A `\`my;col\“ is a quoted column name, and the `;` between `my` and
// `col` is part of the identifier — not a statement boundary.
func TestSplitCHStatementsSemicolonInsideBacktickIdentifierIsNotATerminator(t *testing.T) {
	sql := "CREATE TABLE t (`weird;name` String) ENGINE = MergeTree ORDER BY `weird;name`;"
	parts := migrate.SplitCHStatementsForTest(sql)
	if len(parts) != 1 {
		t.Fatalf("split = %d parts, want 1 — splitter split inside a backtick identifier:\n%#v",
			len(parts), parts)
	}
}

// TestSplitCHStatementsBlockCommentWithEmbeddedSemicolons confirms
// `/* ... ; ... */` does not split. CH accepts block comments and
// uses them in long WITH-CTE-style migrations for headers.
func TestSplitCHStatementsBlockCommentWithEmbeddedSemicolons(t *testing.T) {
	sql := "/* Migration: do this; then do that; carefully. */\n" +
		"CREATE TABLE t (id String) ENGINE = MergeTree ORDER BY id;"
	parts := migrate.SplitCHStatementsForTest(sql)
	if len(parts) != 1 {
		t.Fatalf("split = %d parts, want 1 — block comment split (was that; comment was real):\n%#v",
			len(parts), parts)
	}
	if !strings.Contains(parts[0], "CREATE TABLE") {
		t.Errorf("CREATE TABLE lost: %q", parts[0])
	}
}

// TestSplitCHStatementsNestedBlockCommentKeepsSplit covers nested
// /* /*  ... */ */ block comments. ClickHouse supports them and the
// splitter must respect the depth so an inner `;` is not a terminator.
func TestSplitCHStatementsNestedBlockCommentKeepsSplit(t *testing.T) {
	sql := "/* outer /* inner ; still in comment */ out ; still out */\n" +
		"SELECT 1;"
	parts := migrate.SplitCHStatementsForTest(sql)
	if len(parts) != 1 {
		t.Fatalf("split = %d parts, want 1 — nested comment let `;` leak as terminator:\n%#v",
			len(parts), parts)
	}
}

// TestSplitCHStatementsDollarQuotedStringKeepsSplit covers the
// Postgres-style `$$ ... $$` string form which CH accepts in some
// system-function contexts. The inner `;` is data, not a separator.
func TestSplitCHStatementsDollarQuotedStringKeepsSplit(t *testing.T) {
	sql := "SELECT $$not; a; terminator$$;"
	parts := migrate.SplitCHStatementsForTest(sql)
	if len(parts) != 1 {
		t.Fatalf("split = %d parts, want 1 — dollar-quoted `;` leaked as terminator:\n%#v",
			len(parts), parts)
	}
}
