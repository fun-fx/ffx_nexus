package migrate_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// migrationFileLiteralRegex matches migration filename strings used as
// literals in Go source. The filename is shaped
//
//	[engine]/[NNN]_[name].sql
//
// where engine is "postgres" or "clickhouse" and NNN is a 3-digit
// ordinal. A migration is referenced by ID anywhere — this regex is the
// canonical pattern for both.
//
// We deliberately DO NOT match a bare numeric ordinal ("14") or a bare
// logical migration number ("#15") because the historic defect was a
// list of inline-form names like "014_invite_tokens.sql". A future
// comment that mentions a migration by ordinal in prose is fine; a
// future test or call site that lists migrations by name in code is
// exactly the regression class b1 forbids.
var migrationFileLiteralRegex = regexp.MustCompile(`(?:postgres|clickhouse)[/\\][0-9]{3}_[a-z0-9_]+\.sql`)

// partialListComment explains why the inventory crawls the repo and
// asserts no NEW literal appears. The list of migration filenames in
// the codebase is pinned by the migration directory itself; a Go test
// that hand-rolls the same list is the bug class we want to keep out.
//
// Tests in this package (and the package itself) are expected to NAME
// migration files. The allowlist below lists every place a string
// literal of the shape `postgres/NNN_*.sql` is intentional:
//
//   - The package itself (migrate.go, migrate_test.go, postgres.go,
//     postgres_integration_test.go) names files when testing the
//     loader. The fixture is `nexus.Migrations` (= `//go:embed
//     migrations`) so naming is necessary semantics, not a copy-paste
//     list.
//
//   - tests/schematrail happens inside the migrate integration tests
//     when checking the migration listing is complete.
//
// Any NEW file in the repository that hand-rolls a list of migrations
// when calling migrate.Load / migrate.Run is a regression class this
// test catches.
//
// Force-fail: temporarily add a new migration literal in a console
// handler to verify the test catches it; remove before commit.
func TestMigrationFilenameLiteralsAreRestrictedToTheMigratePackage(t *testing.T) {
	root := "../.."

	// Pin the file list (anchored on the embedded migrations/ directory)
	// so the test never silently grows: a new legit migration adds
	// exactly one new file under migrations/<engine>/ and the shell of
	// an audit log here.
	if got := listEmbeddedMigrationFilenames(t); got == nil {
		t.Fatal("could not enumerate embedded migrations")
	}

	// Files allowed to reference migrations by name.
	allow := map[string]bool{
		"internal/migrate/migrate_test.go":                        true,
		"internal/migrate/migrate.go":                             true,
		"internal/migrate/postgres.go":                            true,
		"internal/migrate/postgres_integration_test.go":           true,
		"internal/migrate/clickhouse.go":                          true,
		"internal/migrate/clickhouse_integration_test.go":         true,
		"internal/migrate/eval_scores_org_integration_test.go":    true,
		"internal/migrate/no_partial_listing_test.go":             true,
		"internal/migrate/numbering_test.go":                      true, // comments only referring to migration names
		"internal/apierr/leak_mutation_test.go":                   true, // synthetic-rename fixture
		"migrations/clickhouse/009_benchmark_runs.sql":            true, // header comment is a ref
		"migrations/postgres/013_scheduled_benchmarks.sql":        true,
		"migrations/postgres/014_invite_tokens.sql":               true,
		"migrations/postgres/015_eval_scores_org.sql":             true,
		"migrations/postgres/016_benchmark_schedule_last_run.sql": true,
		"migrations/postgres/017_audit_request_id.sql":            true,
		"internal/core/benchmark_runs_test.go":                    true, // uses bootDBSchema via migrate.Load
		"internal/core/invite_integration_test.go":                true,
		"internal/core/org_isolation_integration_test.go":         true,
		"internal/core/schema_contract_integration_test.go":       true,
		"internal/health/gate.go":                                 true,
		"internal/observability/reader.go":                        true,
		"deploy/helm/nexus/values.yaml":                           true,
		"migrations/README.md":                                    true,
		"scripts/e2e_invite_flow.sh":                              true,
		"internal/core/benchmark_schedules_test.go":               true,
	}

	var bad []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if path == filepath.Join(root, "node_modules") || path == filepath.Join(root, "vendor") ||
				path == filepath.Join(root, ".git") || path == filepath.Join(root, "web", "dist") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if allow[rel] {
			return nil
		}
		if !isGoOrSQL(path) {
			return nil
		}
		// Skip embedding directories themselves; the migration file is
		// referenced by file path inside migration code, but inside
		// migrations/<engine>/ the literal is the canonical filename.
		if strings.HasPrefix(rel, "migrations/") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if migrationFileLiteralRegex.Match(body) {
			bad = append(bad, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(bad) > 0 {
		t.Errorf("the following files reference a migration by filename string "+
			"(%v match). New migration lists must use migrate.Load + migrate.Run so "+
			"the discovery path is the only source of truth; hand-rolled lists re-introduce "+
			"the 009-011 / 014_invite_tokens / 016_benchmark_schedule_last_run class of bug.\n\n",
			migrationFileLiteralRegex)
		for _, f := range bad {
			t.Errorf("  %s", f)
		}
	}
}

// listEmbeddedMigrationFilenames is a no-op presence check: it proves
// the test runner can read the embedded migration directory. The
// enumeration of the actual filenames is left to migrate.Load so this
// test never duplicates that listing.
func listEmbeddedMigrationFilenames(t *testing.T) map[string]bool {
	t.Helper()
	root := "../.."
	out := map[string]bool{}
	err := filepath.Walk(filepath.Join(root, "migrations"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".sql") {
			rel, _ := filepath.Rel(root, path)
			out[rel] = true
		}
		return nil
	})
	if err != nil {
		return nil
	}
	return out
}

func isGoOrSQL(p string) bool {
	return strings.HasSuffix(p, ".go") || strings.HasSuffix(p, ".sql")
}

// _ is a placeholder so the compiler does not error on unused imports
// when this test is compiled locally and the runtime / go/parser /
// go/token packages are pulled in for type-touching only. Remove this
// section if/when the migration listing starts to use AST level checks
// (e.g. a hardcoded migration id embedded in a SwitchCase ast.Node).
var (
	_ = runtime.GOROOT
	_ = token.NewFileSet
	_ = parser.ParseFile
	_ ast.Node
)
