package migrate

import (
	"context"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// chExecutor applies migrations to ClickHouse.
//
// # What ClickHouse cannot give us
//
// ClickHouse has no multi-statement transactions and no advisory lock, so the
// two guarantees the Postgres path relies on are unavailable:
//
//   - Atomicity of "run the DDL and record it". A crash between the two leaves
//     the DDL applied but unrecorded.
//   - Mutual exclusion between concurrent migrators.
//
// # What we rely on instead
//
// Idempotency. Every statement in migrations/clickhouse/ is written with
// IF NOT EXISTS (asserted by TestMigrationsAreIdempotent), so:
//
//   - Replaying a migration that already ran is a no-op. This makes the
//     unrecorded-DDL crash window harmless: the retry simply re-runs it.
//   - Two replicas running the same DDL concurrently converge on the same
//     result instead of one failing.
//
// The ledger is therefore an optimisation and an audit record here, not a
// correctness mechanism as it is on Postgres. The ledger table is a
// ReplacingMergeTree ordered by id so concurrent inserts for the same
// migration collapse to the newest row rather than accumulating duplicates.
//
// Consequence for operators: a failed ClickHouse migration is safe to retry,
// and the correct recovery is always "run nexus migrate again", never a manual
// DROP. See docs/customer-self-hosted-upgrade-rollback.md.
type chExecutor struct {
	conn    driver.Conn
	version string
}

// NewClickHouse returns an Executor for a ClickHouse connection.
func NewClickHouse(conn driver.Conn, version string) Executor {
	return &chExecutor{conn: conn, version: version}
}

func (e *chExecutor) Engine() Engine { return EngineClickHouse }

func (e *chExecutor) EnsureLedger(ctx context.Context) error {
	return e.conn.Exec(ctx, `
CREATE TABLE IF NOT EXISTS `+LedgerTable+` (
    id            String,
    checksum      String,
    applied_at    DateTime64(3) DEFAULT now64(3),
    duration_ms   UInt64,
    success       UInt8,
    error         String,
    nexus_version String
) ENGINE = ReplacingMergeTree(applied_at)
ORDER BY id`)
}

func (e *chExecutor) Applied(ctx context.Context) (map[string]LedgerEntry, error) {
	// FINAL collapses the ReplacingMergeTree so a retried migration shows only
	// its latest outcome. The table has one row per migration, so the cost of
	// FINAL here is irrelevant.
	rows, err := e.conn.Query(ctx,
		`SELECT id, checksum, applied_at, success FROM `+LedgerTable+` FINAL WHERE success = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]LedgerEntry{}
	for rows.Next() {
		var (
			id, checksum string
			appliedAt    time.Time
			success      uint8
		)
		if err := rows.Scan(&id, &checksum, &appliedAt, &success); err != nil {
			return nil, err
		}
		out[id] = LedgerEntry{ID: id, Checksum: checksum, AppliedAt: appliedAt, Success: success == 1}
	}
	return out, rows.Err()
}

// Apply runs each statement in the file, then records the outcome. Statements
// are split on ';' that fall OUTSIDE a `--` line comment, because the Go
// driver's Exec takes one statement at a time and CH is strict about partial
// fragments from a misplaced semicolon.
//
// The comment-aware split is the load-bearing fix: a CH migration's first
// statement often ends inside a comment block (semicolons appearing in prose
// like `-- status: pending | running | completed | failed;`), and a naive
// strings.Split(m.SQL, ";") turns those comment fragments into garbage
// statements that CH rejects with code: 62. The naive split was previously
// OK by accident because no CH migration had a `;` inside a comment line;
// 009_benchmark_runs.sql now does, and the previous behaviour manifested as
// `failed at position N ('(')` on the append-only audit test we wrote on the
// way through Chapter 2.
func (e *chExecutor) Apply(ctx context.Context, m Migration) error {
	start := time.Now()
	for _, stmt := range splitCHStatements(m.SQL) {
		if !hasSQL(stmt) {
			continue
		}
		if err := e.conn.Exec(ctx, strings.TrimSpace(stmt)); err != nil {
			e.record(ctx, m, time.Since(start), false, err.Error())
			return err
		}
	}
	e.record(ctx, m, time.Since(start), true, "")
	return nil
}

// SplitCHStatementsForTest exposes the comment-aware splitter for the test
// suite so a regression to naive strings.Split can be caught without spinning
// up a ClickHouse container. The "ForTest" suffix is the codebase convention
// for test-only exports — production callers must use Apply().
func SplitCHStatementsForTest(sql string) []string {
	return splitCHStatements(sql)
}

// splitCHStatements splits a CH migration into executable statements,
// respecting `--` line comments. A `;` inside a `--` comment is part of the
// comment, not a statement terminator; CH is strict about partial fragments
// (code: 62) and a naive strings.Split m.SQL on ";" turns comment fragments
// into garbage statements. The naive form was previously OK by accident
// because no CH migration had a `;` inside a comment line; 009_benchmark_runs
// does, and the previous behaviour broke the E2E playwright run that calls
// `nexus migrate` on the persistent CH volume.
//
// Strings quoted with single quote, double quote, or backtick are not common
// in CH migration prose today; if they become common, the splitter will need
// to learn them. Inline `--` comments are stripped conservatively (only the
// `-- ` form, not the run-on `--` form) so a statement like
// `id String, -- inline note` does not bleed markers into the next line.
func splitCHStatements(sql string) []string {
	var out []string
	var stmtLines []string
	for _, line := range strings.Split(sql, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		if i := strings.Index(line, " -- "); i >= 0 {
			line = line[:i]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		stmtLines = append(stmtLines, line)
		// A statement ends on the first line that closes with `;`.
		if !strings.HasSuffix(strings.TrimSpace(line), ";") {
			continue
		}
		joined := strings.TrimSpace(strings.Join(stmtLines, "\n"))
		joined = strings.TrimSuffix(joined, ";")
		if joined != "" {
			out = append(out, joined)
		}
		stmtLines = nil
	}
	if len(stmtLines) > 0 {
		joined := strings.TrimSpace(strings.Join(stmtLines, "\n"))
		joined = strings.TrimSuffix(joined, ";")
		if joined != "" {
			out = append(out, joined)
		}
	}
	return out
}

func (e *chExecutor) record(ctx context.Context, m Migration, took time.Duration, ok bool, errMsg string) {
	var success uint8
	if ok {
		success = 1
	}
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	// Best-effort: losing a ledger row costs a redundant no-op replay next
	// time, which is exactly what idempotency is for. Never mask the real
	// migration error with a bookkeeping one.
	_ = e.conn.Exec(c, `
INSERT INTO `+LedgerTable+` (id, checksum, duration_ms, success, error, nexus_version)
VALUES (?, ?, ?, ?, ?, ?)`,
		m.ID, m.Checksum, uint64(took.Milliseconds()), success, errMsg, e.version)
}

// Lock is a no-op. See the type comment: ClickHouse offers no advisory lock, so
// concurrent safety comes from every statement being IF NOT EXISTS rather than
// from mutual exclusion. Returning a no-op release keeps Run's shape identical
// across engines instead of hiding an engine check inside it.
func (e *chExecutor) Lock(_ context.Context) (func(), error) {
	return func() {}, nil
}

// SchemaExists probes for the traces table created by 001_init.sql.
func (e *chExecutor) SchemaExists(ctx context.Context) (bool, error) {
	rows, err := e.conn.Query(ctx,
		`SELECT count() FROM system.tables WHERE database = currentDatabase() AND name = 'traces'`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var n uint64
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return false, err
		}
	}
	return n > 0, rows.Err()
}

// hasSQL reports whether a split fragment contains anything but comments and
// whitespace.
func hasSQL(stmt string) bool {
	for _, line := range strings.Split(stmt, "\n") {
		l := strings.TrimSpace(line)
		if l != "" && !strings.HasPrefix(l, "--") {
			return true
		}
	}
	return false
}
