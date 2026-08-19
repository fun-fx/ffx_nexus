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
// respecting line comments (`--`), block comments (`/* ... */`), string
// literals (single and double quoted), and ClickHouse-style identifier
// backticks. A `;` inside any of those contexts is part of the
// surrounding content, not a statement terminator. CH is strict about
// partial fragments (code: 27 / 62) and a naive strings.Split m.SQL on
// ";" turns comment or quoted fragments into garbage statements.
//
// The split tracks a small set of mutually-exclusive contexts:
//   - line  : default
//   - lineComment : inside `-- ... \n`
//   - blockComment: inside `/* ... */` (supports nesting — ClickHouse
//     allows `/* /* nested */ */`)
//   - singleQuoteString, doubleQuoteString: '...', "..." with backslash
//     escape for embedded quote of the same kind
//   - backtickId: `column_name` — same nesting escape rule as the strings
//   - dollarQuote: `$$ ... $$` — Postgres-style dollar-quoted strings
//     (CH supports them in some system functions; treat
//     them consistently with the engine)
//
// We additionally split INSIDE block-comment depth, so a `*/` inside a
// string literal does not exit a comment. Inline `-- ` (with the space)
// is stripped conservatively; a run-on `--` (no space) is left alone so
// legacy migrations that use the form survive. The naive splitter that
// this replaced only handled `--` line comments and broke 009_benchmark_runs
// when a comment contained `;`.
func splitCHStatements(sql string) []string {
	var (
		out              []string
		current          strings.Builder
		stmtBuf          strings.Builder
		hasStatementText bool
		state            = stateLine
		blockDepth       int
		lineHasContent   bool
	)
	pushStmt := func() {
		text := strings.TrimSpace(stmtBuf.String())
		stmtBuf.Reset()
		hasStatementText = false
		lineHasContent = false
		if text == "" {
			return
		}
		if endsWithLineComment(text) {
			// Trim -- line comments that trail the statement. Block
			// comments are stripped at parse time above. This catches
			// the case where the splitter saw code, then a `-- ...;`
			// line, then a `;` from the code line — the line comment
			// would otherwise be carried forward as a phantom prefix.
			if idx := strings.Index(text, "\n--"); idx >= 0 {
				text = strings.TrimSpace(text[:idx])
				if text == "" {
					return
				}
			}
		}
		out = append(out, text)
	}
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch state {
		case stateLine:
			switch c {
			case '\'':
				state = stateSingle
				stmtBuf.WriteByte(c)
				lineHasContent = true
				hasStatementText = true
			case '"':
				state = stateDouble
				stmtBuf.WriteByte(c)
				lineHasContent = true
				hasStatementText = true
			case '`':
				state = stateBacktick
				stmtBuf.WriteByte(c)
				lineHasContent = true
				hasStatementText = true
			case '$':
				if i+1 < len(sql) && sql[i+1] == '$' {
					state = stateDollarDouble
					stmtBuf.WriteString("$$")
					i++ // consume the second '$'
					lineHasContent = true
					hasStatementText = true
					continue
				}
				stmtBuf.WriteByte(c)
				hasStatementText = true
			case '-':
				if i+1 < len(sql) && sql[i+1] == '-' {
					state = stateLineComment
					// Roll back any whitespace-only content accumulated
					// on this line; the line comment is not part of the
					// statement. If the line already has code, the line
					// ends with the comment and we must NOT erase it
					// (statements like `id String, -- inline note` are
					// valid CH).
					if !hasStatementText {
						stmtBuf.Reset()
					} else {
						stmtBuf.WriteString(" -- ")
						// Drain the rest of the line; the LineComment
						// state will switch back at the newline and
						// drop the body.
						for j := i + 2; j < len(sql) && sql[j] != '\n'; j++ {
							stmtBuf.WriteByte(sql[j])
							i = j
						}
						i++ // step onto the newline so the next loop picks it up
					}
					i++ // consume the second '-'
					continue
				}
				stmtBuf.WriteByte(c)
				hasStatementText = true
			case '/':
				if i+1 < len(sql) && sql[i+1] == '*' {
					state = stateBlockComment
					blockDepth = 1
					stmtBuf.WriteString("/*")
					i++
					continue
				}
				stmtBuf.WriteByte(c)
				hasStatementText = true
			case ';':
				if hasStatementText {
					pushStmt()
				} else {
					stmtBuf.Reset()
					lineHasContent = false
				}
			case '\n':
				stmtBuf.WriteByte(c)
				if !lineHasContent {
					// Collapse leading blank lines but preserve intra-statement
					// newlines (a multi-line statement still needs its lines).
				}
				lineHasContent = false
			default:
				stmtBuf.WriteByte(c)
				hasStatementText = true
				lineHasContent = true
			}
		case stateLineComment:
			// The line-comment state is entered with the comment content
			// already drained into stmtBuf (or reset) above. The only job
			// here is to eat characters until the newline, then switch
			// back to line state. We do not write the comment characters
			// into stmtBuf — comments are not part of the statement.
			if c == '\n' {
				state = stateLine
				lineHasContent = false
			}
		case stateBlockComment:
			stmtBuf.WriteByte(c)
			switch c {
			case '/':
				if i+1 < len(sql) && sql[i+1] == '*' {
					blockDepth++
					stmtBuf.WriteByte('*')
					i++
				}
			case '*':
				if i+1 < len(sql) && sql[i+1] == '/' {
					if blockDepth == 1 {
						state = stateLine
						stmtBuf.WriteByte('/')
						i++
						blockDepth = 0
					} else {
						blockDepth--
						stmtBuf.WriteByte('/')
						i++
					}
				}
			}
		case stateSingle:
			stmtBuf.WriteByte(c)
			switch c {
			case '\\':
				if i+1 < len(sql) {
					stmtBuf.WriteByte(sql[i+1])
					i++
				}
			case '\'':
				state = stateLine
			}
		case stateDouble:
			stmtBuf.WriteByte(c)
			switch c {
			case '\\':
				if i+1 < len(sql) {
					stmtBuf.WriteByte(sql[i+1])
					i++
				}
			case '"':
				state = stateLine
			}
		case stateBacktick:
			stmtBuf.WriteByte(c)
			switch c {
			case '`':
				if i+1 < len(sql) && sql[i+1] == '`' {
					stmtBuf.WriteByte('`')
					i++
				} else {
					state = stateLine
				}
			}
		case stateDollarDouble:
			stmtBuf.WriteByte(c)
			if c == '$' && i+1 < len(sql) && sql[i+1] == '$' {
				stmtBuf.WriteByte('$')
				i++
				state = stateLine
			}
		}
		_ = current
	}
	if hasStatementText {
		out = append(out, stmtBuf.String())
	}
	return out
}

const (
	stateLine = iota
	stateLineComment
	stateBlockComment
	stateSingle
	stateDouble
	stateBacktick
	stateDollarDouble
)

// endsWithLineComment reports whether the trimmed tail of a statement
// body is a `--` comment. Used after the splitter strikes a `;` to
// decide whether the trailing line(s) need to be peeled off so they
// do not get sent to CH as garbage code.
func endsWithLineComment(s string) bool {
	for {
		s = strings.TrimRight(s, "\n\t ")
		if idx := strings.LastIndex(s, "\n--"); idx >= 0 {
			s = s[idx+1:]
		}
		if strings.HasPrefix(s, "--") {
			return true
		}
		return false
	}
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
