package schemacontract_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	nexus "github.com/ffxnexus/nexus"
	"github.com/ffxnexus/nexus/internal/migrate"
	"github.com/ffxnexus/nexus/internal/schemacontract"
)

// The schema contract, checked by the database rather than by a parser we wrote.
//
// Every SQL statement in the repository's Go source is extracted and handed to
// Postgres as `PREPARE stmt AS <statement>`. Postgres validates every table,
// column and type without executing anything. A column the migration set does not
// create fails here with SQLSTATE 42703 and a message naming it.
//
// Run it against a throwaway database:
//
//	NEXUS_TEST_POSTGRES_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' \
//	  go test ./internal/schemacontract/ -v

func migratedSchema(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("NEXUS_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set NEXUS_TEST_POSTGRES_URL to run the schema contract against a real database")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	schema := "contract_" + strings.ToLower(strings.NewReplacer("/", "_", "-", "_").Replace(t.Name()))
	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`,
		`CREATE SCHEMA ` + schema,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			admin.Close()
			t.Fatalf("prepare schema: %v", err)
		}
	}
	admin.Close()

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	// The migrations create unqualified objects, so a per-test search_path gives
	// each run its own namespace without editing any SQL.
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("scoped pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		if cleanup, err := pgxpool.New(context.Background(), url); err == nil {
			_, _ = cleanup.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
			cleanup.Close()
		}
	})

	migs, err := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if _, err := migrate.Run(ctx, migrate.NewPostgres(pool, "schema-contract"), migs, migrate.Options{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(wd))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("cannot locate the repository root from %s", wd)
	}
	return root
}

// Statements targeting ClickHouse live in the same tree, and Postgres cannot
// parse their engine-specific syntax (argMax, toInt64, INTERVAL ? SECOND), so
// preparing them against Postgres reports failures that mean nothing.
//
// Classification is by file path, not by sniffing the SQL: sniffing would
// misclassify a portable-looking SELECT and silently drop it from the Postgres
// contract, which is the failure mode this whole package exists to prevent.
//
// The first attempt at this was a hand-written list, and it leaked immediately —
// it missed internal/router/clickhouse.go and internal/router/bench_provider_ch.go,
// which surfaced as two spurious failures. So the primary rule is the repository's
// naming convention, and the explicit list holds only the files that do not follow
// it. TestClickHouseClassificationIsExhaustive keeps both honest.
var clickhouseNamingConvention = regexp.MustCompile(`clickhouse[^/]*\.go$|_ch\.go$`)

// clickhouseExceptions are ClickHouse-querying files whose names do not say so.
var clickhouseExceptions = []string{
	"internal/observability/reader.go",    // console read paths, all ClickHouse
	"internal/observability/trace_org.go", // trace org resolution
}

func isClickHouse(path string) bool {
	slash := filepath.ToSlash(path)
	if clickhouseNamingConvention.MatchString(slash) {
		return true
	}
	for _, f := range clickhouseExceptions {
		if strings.HasSuffix(slash, f) {
			return true
		}
	}
	return false
}

// An exception for a file that no longer exists, or that no longer holds SQL, is
// stale — and a stale exception is a Postgres statement excluded from the contract
// for no reason.
func TestClickHouseClassificationIsExhaustive(t *testing.T) {
	root := repoRoot(t)
	var all []schemacontract.Statement
	for _, dir := range []string{"internal", "cmd"} {
		stmts, err := schemacontract.Extract(filepath.Join(root, dir))
		if err != nil {
			t.Fatalf("extract: %v", err)
		}
		all = append(all, stmts...)
	}

	withSQL := map[string]bool{}
	for _, s := range all {
		rel, _ := filepath.Rel(root, s.File)
		withSQL[filepath.ToSlash(rel)] = true
	}

	for _, exc := range clickhouseExceptions {
		if !withSQL[exc] {
			t.Errorf("clickhouseExceptions lists %s, which contains no extracted SQL. "+
				"Remove it: an unnecessary exception excludes real statements from the "+
				"Postgres contract.", exc)
		}
	}

	// Any file matching the naming convention must actually hold SQL, or the
	// convention has drifted from what the repository does.
	matched := 0
	for f := range withSQL {
		if clickhouseNamingConvention.MatchString(f) {
			matched++
		}
	}
	if matched == 0 {
		t.Error("no SQL-bearing file matches the ClickHouse naming convention. Either " +
			"the convention changed or the extractor broke; ClickHouse statements are " +
			"now being prepared against Postgres and will fail confusingly.")
	}
}

func TestIntegrationEveryPostgresStatementValidatesAgainstTheMigratedSchema(t *testing.T) {
	pool := migratedSchema(t)
	ctx := context.Background()
	root := repoRoot(t)

	var all []schemacontract.Statement
	for _, dir := range []string{"internal", "cmd"} {
		stmts, err := schemacontract.Extract(filepath.Join(root, dir))
		if err != nil {
			t.Fatalf("extract from %s: %v", dir, err)
		}
		all = append(all, stmts...)
	}
	if len(all) == 0 {
		t.Fatal("extracted no SQL at all; the extractor has stopped working and this " +
			"test is now vacuous")
	}

	var pg []schemacontract.Statement
	for _, s := range all {
		if !isClickHouse(s.File) {
			pg = append(pg, s)
		}
	}

	cov := schemacontract.Summarise(pg)
	t.Logf("postgres SQL: %d statements, %d complete enough to validate, %d assembled at runtime",
		cov.Total, cov.Complete, len(cov.Incomplete))

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	type failure struct {
		stmt schemacontract.Statement
		err  error
	}
	var failures []failure
	prepared := 0

	for i, s := range pg {
		if !s.Complete {
			continue
		}
		name := fmt.Sprintf("contract_stmt_%d", i)
		// PREPARE parses and plans without executing. A missing table is 42P01, a
		// missing column 42703 — both from the same parser production uses.
		_, err := conn.Exec(ctx, `PREPARE `+name+` AS `+s.SQL)
		if err != nil {
			if ignorableParseError(err) {
				continue
			}
			failures = append(failures, failure{s, err})
			continue
		}
		prepared++
		_, _ = conn.Exec(ctx, `DEALLOCATE `+name)
	}

	if prepared == 0 {
		t.Fatal("prepared no statements successfully; either the schema is empty or " +
			"the extractor is producing text Postgres cannot parse. Either way this " +
			"test proves nothing — investigate before trusting a pass.")
	}
	t.Logf("validated %d statements against the migrated schema", prepared)

	for _, f := range failures {
		rel, _ := filepath.Rel(root, f.stmt.File)
		t.Errorf("%s:%d does not validate against the schema the migrations produce:\n"+
			"  %v\n"+
			"  statement: %s\n\n"+
			"  This query fails at runtime on every database built from the migration "+
			"set, including every customer's. Add a migration; do not weaken the "+
			"query to match the schema unless the column was genuinely never wanted.",
			rel, f.stmt.Line, f.err, collapse(f.stmt.SQL))
	}
}

// ignorableParseError filters failures that are not schema problems.
//
// Kept deliberately narrow. Every entry here is a hole in the contract, so each
// one names the exact SQLSTATE and why it cannot indicate a missing identifier.
// Anything broader — swallowing all syntax errors, for instance — would let a real
// 42703 through whenever it happened to arrive alongside something else.
func ignorableParseError(err error) bool {
	code := sqlState(err)
	switch code {
	case "42P18":
		// indeterminate_datatype: a bare $1 whose type Postgres cannot infer, e.g.
		// `WHERE col = ANY($1)` without a cast. The tables and columns were still
		// resolved to reach this error, so identifier checking succeeded.
		return true
	case "42601":
		// syntax_error: the extracted text is not the final statement. classify()
		// catches most of these; the remainder are literals assembled in a way the
		// heuristics miss, and they are covered by the smoke test.
		return true
	case "0A000":
		// feature_not_supported: PREPARE refuses utility statements (CREATE, ALTER).
		// Those are migration DDL, which the migration tests already execute.
		return true
	}
	return false
}

func sqlState(err error) string {
	type coder interface{ SQLState() string }
	for e := err; e != nil; {
		if c, ok := e.(coder); ok {
			return c.SQLState()
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return ""
		}
		e = u.Unwrap()
	}
	return ""
}

func collapse(sql string) string {
	s := strings.Join(strings.Fields(sql), " ")
	if len(s) > 220 {
		return s[:220] + " …"
	}
	return s
}

// The extractor's coverage must be visible, and it must not silently collapse.
// A regression that made classify() reject everything would turn the test above
// into a no-op that still reports success.
func TestExtractorCoverageIsReported(t *testing.T) {
	root := repoRoot(t)
	var all []schemacontract.Statement
	for _, dir := range []string{"internal", "cmd"} {
		stmts, err := schemacontract.Extract(filepath.Join(root, dir))
		if err != nil {
			t.Fatalf("extract: %v", err)
		}
		all = append(all, stmts...)
	}

	cov := schemacontract.Summarise(all)
	t.Logf("all SQL: %d statements, %d complete, %d assembled at runtime",
		cov.Total, cov.Complete, len(cov.Incomplete))

	if cov.Total < 40 {
		t.Errorf("only %d SQL statements found across the repository. The extractor "+
			"has probably stopped matching; verify before trusting the contract test.",
			cov.Total)
	}
	if cov.Complete < cov.Total/3 {
		t.Errorf("only %d of %d statements are complete enough to validate. Below a "+
			"third, the contract test covers too little to rely on.", cov.Complete, cov.Total)
	}

	// Name the runtime-assembled statements so the gap is a list somebody can read
	// rather than a number.
	byFile := map[string][]int{}
	for _, s := range cov.Incomplete {
		rel, _ := filepath.Rel(root, s.File)
		byFile[rel] = append(byFile[rel], s.Line)
	}
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		t.Logf("  runtime-assembled, covered by the smoke test instead: %s:%v", f, byFile[f])
	}
}
