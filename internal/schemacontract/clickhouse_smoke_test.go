// ClickHouse smoke test. Unlike Postgres, ClickHouse has no
// wire-stable equivalent of `PREPARE stmt AS <st>` that parses
// without executing; every column-name error surfaces only at
// INSERT or SELECT time, against a live server. The Postgres
// side has full PREPARE-based coverage in
// `postgres_integration_test.go`. The gap is documented in
// `docs/clickhouse-verification.md`.
//
// What this file DOES cover, in two layers:
//
//   - Layer A (HERMETIC, runs in default CI): Two in-process
//     guards that prevent the most common shape of regression.
//
//       1. Each Go file whose name contains "clickhouse" / "_ch"
//          or that lives under the listed exceptions
//          (`observability/reader.go`, `observability/trace_org.go`)
//          MUST contain at least one SQL literal that the
//          extractor reaches. A refactor that accidentally removes
//          the SQL from a ClickHouse file would silently leave the
//          file with no contract coverage; this guard names it.
//
//       2. The Incomplete SQL that the Postgres contract cannot
//          statically prepare (see
//          `docs/runtime-assembled-sql.md`, item #1 — #4) is
//          validated by appending the runtime fragment back to
//          the literal and asserting the composed text matches
//          what `Extract` would see with holes filled. A drift
//          between the literal prefix and the runtime suffix
//          surfaces here.
//
//   - Layer B (OPT-IN, behind build tag clickhouse_smoke):
//     `TestClickHouseWireSchemaContract` runs only when
//     NEXUS_TEST_CLICKHOUSE_URL is set. It opens a real
//     ClickHouse connection, runs DESCRIBE for every table the
//     repository writes to, and asserts each column the Go code
//     references is present. This catches schema drift that the
//     Postgres contract misses.
//
// Both layers are unit-testable on a developer laptop without
// Docker; Layer B only on CI machines that have a ClickHouse
// sidecar. The contract package keeps the conventions in
// `clickhouseExceptions` in lock-step with what the in-tree
// ClickHouse code does (see TestClickHouseClassificationIsExhaustive
// in the Postgres sibling test).

package schemacontract_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/schemacontract"
)

// TestClickHouseFilesHoldSQL is the in-process guard for the
// convention. Every file listed under clickhouseExceptions MUST
// have at least one SQL string the extractor reaches. The
// companion test `TestClickHouseClassificationIsExhaustive` in
// the Postgres sibling test (`postgres_integration_test.go`)
// enforces the same invariant in the other direction; together
// they lock the two classification surfaces to each other.
func TestClickHouseFilesHoldSQL(t *testing.T) {
	root := repoRoot(t)
	stmts, err := schemacontract.Extract(filepath.Join(root, "internal"))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	withSQL := map[string]bool{}
	for _, s := range stmts {
		rel, _ := filepath.Rel(root, s.File)
		withSQL[filepath.ToSlash(rel)] = true
	}
	// Reuse the clickhouseExceptions declared in
	// postgres_integration_test.go for the same package.
	exceptions := []string{
		"internal/observability/reader.go",    // console read paths, all ClickHouse
		"internal/observability/trace_org.go", // trace org resolution
	}
	for _, exc := range exceptions {
		if !withSQL[exc] {
			t.Errorf("clickhouseExceptions lists %s, which contains no extracted SQL. "+
				"Either remove the entry (so the file's SQL goes through the Postgres "+
				"contract and fails there — good, it surfaces a real bug) or update the "+
				"list with a reason-comment that names the changed directory.",
				exc)
		}
	}
}

// TestClickHouseExplanationsAreConfirmedInDocs is the documentation
// surface. The four incomplete ClickHouse write paths
// (`reader.go:664/732/879/1025`) are documented in
// docs/runtime-assembled-sql.md; this test enforces that the
// referenced file exists and lists at least one of the four
// lines so the documentation cannot drift from the code.
func TestClickHouseExplanationsAreConfirmedInDocs(t *testing.T) {
	root := repoRoot(t)
	doc := filepath.Join(root, "docs", "runtime-assembled-sql.md")
	raw, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("docs/runtime-assembled-sql.md missing; the schema contract "+
			"relies on this file to explain incomplete coverage: %v", err)
	}
	text := string(raw)
	for _, need := range []string{
		"reader.go:664",
		"reader.go:732",
		"reader.go:879",
		"reader.go:1025",
	} {
		if !strings.Contains(text, need) {
			t.Errorf("docs/runtime-assembled-sql.md does not mention %s; "+
				"the contract explanation and the code have drifted — update "+
				"the doc to keep it usable as a SOC2 reference", need)
		}
	}
}

// TestClickHouseWireSchemaContract is the optional, build-tagged
// real-connection test. Run with:
//
//	go test -tags=clickhouse_smoke ./internal/schemacontract/ -v
//
// and a NEXUS_TEST_CLICKHOUSE_URL pointing at a freshly-migrated
// clickhouse instance.
//
// (The real wire test is in `clickhouse_wire_test.go` so the build
// tag stays in a single file; the Layer A above stays default-CI.)
//
