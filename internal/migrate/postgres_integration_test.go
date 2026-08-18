package migrate_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	nexus "github.com/ffxnexus/nexus"
	"github.com/ffxnexus/nexus/internal/migrate"
)

// Integration tests against a real Postgres. Skipped unless
// NEXUS_TEST_POSTGRES_URL points at a database the test may freely create and
// drop schemas in.
//
//	# local
//	NEXUS_TEST_POSTGRES_URL='postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable' \
//	  go test ./internal/migrate/ -run Integration -v
//
// These cover the properties that unit tests with a fake executor cannot: that
// the SQL actually applies in order against a real engine, that the advisory
// lock genuinely serialises concurrent migrators, and that adopting a
// pre-ledger database repairs rather than damages it.

func pgURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("NEXUS_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set NEXUS_TEST_POSTGRES_URL to run Postgres integration tests")
	}
	return url
}

// freshSchema gives each test an isolated Postgres schema, so tests neither see
// each other's tables nor need a separate database per case.
func freshSchema(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	name := fmt.Sprintf("nexus_test_%d_%s", time.Now().UnixNano(), sanitize(t.Name()))
	if len(name) > 60 {
		name = name[:60]
	}

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+name); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	admin.Close()

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	// search_path scopes every unqualified object in the migrations to this
	// schema, including the ledger.
	cfg.ConnConfig.RuntimeParams["search_path"] = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect with search_path: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		a, err := pgxpool.New(context.Background(), url)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+name+` CASCADE`)
	})
	return pool
}

func sanitize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func tableExists(t *testing.T, pool *pgxpool.Pool, table string) bool {
	t.Helper()
	var ok bool
	err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
                        WHERE table_schema = current_schema() AND table_name = $1)`, table).Scan(&ok)
	if err != nil {
		t.Fatalf("tableExists(%s): %v", table, err)
	}
	return ok
}

func columnExists(t *testing.T, pool *pgxpool.Pool, table, column string) bool {
	t.Helper()
	var ok bool
	err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
                        WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2)`,
		table, column).Scan(&ok)
	if err != nil {
		t.Fatalf("columnExists(%s.%s): %v", table, column, err)
	}
	return ok
}

func quietOpts() migrate.Options {
	return migrate.Options{Logger: slog.New(slog.DiscardHandler)}
}

// ---------------------------------------------------------------------------

// The headline case: an empty customer database must end up with every table
// the binary needs, including the two that defects in the old hardcoded list
// left permanently absent.
func TestIntegrationFreshDatabaseGetsCompleteSchema(t *testing.T) {
	pool := freshSchema(t, pgURL(t))
	migs, err := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	if err != nil {
		t.Fatal(err)
	}

	res, err := migrate.Run(context.Background(),
		migrate.NewPostgres(pool, "test"), migs, quietOpts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Applied) != len(migs) {
		t.Fatalf("applied %d of %d migrations", len(res.Applied), len(migs))
	}
	if res.Adopted {
		t.Error("Adopted = true on an empty database")
	}

	for _, table := range []string{
		"virtual_keys", "provider_credentials", "users", "audit_log",
		"eval_scores", "eval_plugins", "eval_plugin_keys",
		"benchmark_runs", "benchmark_schedules",
		// Was never created: 014 was absent from the boot list, so every
		// fresh install answered POST /api/invites with a 500.
		"invite_tokens",
		migrate.LedgerTable,
	} {
		if !tableExists(t, pool, table) {
			t.Errorf("table %q was not created", table)
		}
	}

	// Was never created either: 013's ALTER ran before 012's CREATE, the error
	// was logged and swallowed, and the column silently never appeared.
	if !columnExists(t, pool, "benchmark_runs", "schedule_id") {
		t.Error("benchmark_runs.schedule_id missing — migration order regressed")
	}
}

// Every pod restart re-runs this. It must be a no-op.
func TestIntegrationRerunAppliesNothing(t *testing.T) {
	pool := freshSchema(t, pgURL(t))
	migs, _ := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	ex := migrate.NewPostgres(pool, "test")
	ctx := context.Background()

	if _, err := migrate.Run(ctx, ex, migs, quietOpts()); err != nil {
		t.Fatal(err)
	}
	second, err := migrate.Run(ctx, ex, migs, quietOpts())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(second.Applied) != 0 {
		t.Errorf("second run applied %v, want nothing", second.Applied)
	}
	if len(second.Skipped) != len(migs) {
		t.Errorf("second run skipped %d of %d", len(second.Skipped), len(migs))
	}

	pending, err := migrate.Pending(ctx, ex, migs)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("Pending = %v after a full run", pending)
	}
}

// Three gateway replicas rolling at once used to mean three processes issuing
// identical DDL concurrently. The advisory lock must turn that into a queue,
// and each migration must be recorded exactly once.
func TestIntegrationConcurrentMigratorsApplyEachMigrationOnce(t *testing.T) {
	url := pgURL(t)
	pool := freshSchema(t, url)
	migs, _ := migrate.Load(nexus.Migrations, migrate.EnginePostgres)

	// Reuse the same schema across all racers by cloning the pool config.
	var schema string
	if err := pool.QueryRow(context.Background(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}

	const racers = 5
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		applied int
		errs    []error
	)
	start := make(chan struct{})

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			cfg, err := pgxpool.ParseConfig(url)
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			cfg.ConnConfig.RuntimeParams["search_path"] = schema
			p, err := pgxpool.NewWithConfig(context.Background(), cfg)
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			defer p.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			<-start // maximise overlap
			res, err := migrate.Run(ctx, migrate.NewPostgres(p, "test"), migs, quietOpts())
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			applied += len(res.Applied)
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("concurrent migrator failed: %v", err)
	}
	// The lock guarantees exactly one process does the work; the others find a
	// complete ledger and apply nothing.
	if applied != len(migs) {
		t.Errorf("total migrations applied across %d racers = %d, want exactly %d "+
			"(a higher number means the advisory lock did not serialise them)",
			racers, applied, len(migs))
	}

	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM `+migrate.LedgerTable).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != len(migs) {
		t.Errorf("ledger has %d rows, want %d (duplicates mean a race)", rows, len(migs))
	}
}

// A deployment that predates the ledger has tables but no schema_migrations.
// Adoption must be non-destructive AND must apply what earlier defects skipped.
func TestIntegrationAdoptsPreLedgerDatabaseAndRepairsIt(t *testing.T) {
	pool := freshSchema(t, pgURL(t))
	ctx := context.Background()
	migs, _ := migrate.Load(nexus.Migrations, migrate.EnginePostgres)

	// Reconstruct the state the old boot list actually produced: no ledger, and
	// crucially neither 014_invite_tokens.sql nor 013_scheduled_benchmarks.sql,
	// both of which earlier defects skipped.
	//
	// 016 is skipped as a consequence rather than a premise: it ALTERs
	// benchmark_schedules, so it cannot be seeded into a database where 013 never
	// created that table. A migration added later that also touches a table 013
	// creates has to be listed here for the same reason.
	skipped := map[string]bool{
		"014_invite_tokens.sql":               true,
		"013_scheduled_benchmarks.sql":        true,
		"016_benchmark_schedule_last_run.sql": true, // depends on 013
	}
	order := []string{}
	for _, m := range migs {
		if !skipped[m.Name] {
			order = append(order, m.Name)
		}
	}
	byName := map[string]migrate.Migration{}
	for _, m := range migs {
		byName[m.Name] = m
	}
	for _, name := range order {
		if _, err := pool.Exec(ctx, byName[name].SQL); err != nil {
			t.Fatalf("seeding legacy schema with %s: %v", name, err)
		}
	}

	// Prove the starting point really is the broken one.
	if tableExists(t, pool, migrate.LedgerTable) {
		t.Fatal("test setup error: ledger should not exist yet")
	}
	if tableExists(t, pool, "invite_tokens") {
		t.Fatal("test setup error: invite_tokens should be missing")
	}
	// Seed a row so we can prove adoption does not wipe customer data.
	// virtual_keys.org_id is a NOT NULL FK, so the org has to exist first.
	if _, err := pool.Exec(ctx,
		`INSERT INTO organizations (id, name) VALUES ('org-legacy', 'Legacy Co')`); err != nil {
		t.Fatalf("seeding organizations: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO virtual_keys (id, org_id, name, key_hash, key_prefix, key_last4)
VALUES ('k1', 'org-legacy', 'survivor', 'h1', 'nxs_live_ab12', 'ab12')`); err != nil {
		t.Fatalf("seeding virtual_keys: %v", err)
	}

	res, err := migrate.Run(ctx, migrate.NewPostgres(pool, "test"), migs, quietOpts())
	if err != nil {
		t.Fatalf("adoption run: %v", err)
	}
	if !res.Adopted {
		t.Error("Adopted = false, want true for a pre-ledger database")
	}

	// Repaired: the previously-missing migration is now applied.
	if !tableExists(t, pool, "invite_tokens") {
		t.Error("adoption did not create invite_tokens")
	}
	if !columnExists(t, pool, "benchmark_runs", "schedule_id") {
		t.Error("adoption did not add benchmark_runs.schedule_id")
	}
	// Non-destructive: pre-existing customer data is untouched.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM virtual_keys WHERE name = 'survivor'`).Scan(&n); err != nil {
		t.Fatalf("re-reading seeded row: %v", err)
	} else if n != 1 {
		t.Errorf("pre-existing virtual_keys row count = %d, want 1 — adoption destroyed data", n)
	}

	// And it is now under management: a second run does nothing.
	second, err := migrate.Run(ctx, migrate.NewPostgres(pool, "test"), migs, quietOpts())
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Applied) != 0 {
		t.Errorf("run after adoption applied %v, want nothing", second.Applied)
	}
}

// Editing an applied migration must stop the deploy, not silently diverge.
func TestIntegrationChecksumDriftIsFatal(t *testing.T) {
	pool := freshSchema(t, pgURL(t))
	ctx := context.Background()
	migs, _ := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	ex := migrate.NewPostgres(pool, "test")

	if _, err := migrate.Run(ctx, ex, migs, quietOpts()); err != nil {
		t.Fatal(err)
	}

	tampered := append([]migrate.Migration(nil), migs...)
	tampered[0].Checksum = "0000000000000000"
	if _, err := migrate.Run(ctx, ex, tampered, quietOpts()); !errors.Is(err, migrate.ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
}

// A failing migration must not be recorded as applied, so the retry re-attempts
// it rather than skipping past a schema change that never happened.
func TestIntegrationFailedMigrationIsNotRecordedAsApplied(t *testing.T) {
	pool := freshSchema(t, pgURL(t))
	ctx := context.Background()
	migs, _ := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	ex := migrate.NewPostgres(pool, "test")

	broken := []migrate.Migration{
		migs[0],
		{
			ID: "postgres/999_broken.sql", Engine: migrate.EnginePostgres, Ordinal: 999,
			Name: "999_broken.sql", SQL: "THIS IS NOT SQL;", Checksum: "abc123",
		},
	}
	if _, err := migrate.Run(ctx, ex, broken, quietOpts()); err == nil {
		t.Fatal("Run succeeded on invalid SQL")
	}

	applied, err := ex.Applied(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := applied["postgres/999_broken.sql"]; ok {
		t.Error("the failed migration was recorded as successfully applied")
	}
	if _, ok := applied["postgres/001_init.sql"]; !ok {
		t.Error("the migration that DID succeed before the failure was not recorded")
	}
}
