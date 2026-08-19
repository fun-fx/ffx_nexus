package core

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	nexus "github.com/ffxnexus/nexus"
	"github.com/ffxnexus/nexus/internal/migrate"
)

// This file guards one defect class: production SQL naming a column the migration
// set does not create.
//
// It has happened twice. invite_tokens was never created at all, so every invite
// on a fresh install returned 500. Then benchmark_schedules was created without
// last_run_id or last_launched_at, which every SELECT in benchmark_schedules.go
// lists — so creating a schedule worked and reading one back failed with
// SQLSTATE 42703.
//
// Both slipped through for the same reason: the tests that would have caught them
// need a real Postgres, and the selector CI used to run this package did not match
// their names. Nothing about writing the query tells you the column is missing,
// and unit tests with a fake store cannot know.
//
// So this test builds the schema exactly as a customer's install does — from the
// embedded migration set, on an empty database — and then compares the columns
// the code references against the columns that exist.
//
//	NEXUS_TEST_POSTGRES_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' \
//	  go test ./internal/core/ -run SchemaContract -v

func migratedSchema(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("NEXUS_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set NEXUS_TEST_POSTGRES_URL to run the schema contract test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close()

	schema := "contract_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`,
		`CREATE SCHEMA ` + schema,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("prepare schema: %v", err)
		}
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("scoped pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanup, err := pgxpool.New(context.Background(), url)
		if err == nil {
			_, _ = cleanup.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
			cleanup.Close()
		}
	})

	migs, err := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if _, err := migrate.Run(ctx, migrate.NewPostgres(pool, "schema-contract-test"), migs, migrate.Options{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// expectedColumns is the contract: for each table, the columns production SQL in
// this package reads or writes.
//
// Add to this when you add a column reference to a query. The list is
// deliberately manual rather than derived by parsing the SQL strings: a parser
// would have to understand aliases, CTEs and dynamic fragments, and would fail
// open on anything it could not read — which is precisely the case where a column
// is missing.
var expectedColumns = map[string][]string{
	"benchmark_schedules": {
		"id", "org_id", "name", "environments", "model", "num_examples",
		"rollouts", "via_gateway", "cadence_seconds", "next_launch_at",
		"enabled", "last_run_id", "last_launched_at", "created_by",
		"created_at", "updated_at",
	},
	"benchmark_runs": {
		"id", "org_id", "model", "status", "created_at", "updated_at",
	},
	"invite_tokens": {
		"token_hash", "org_id", "email", "role", "expires_at", "accepted_at",
	},
	"audit_log": {
		"id", "org_id", "actor", "action", "target_id", "detail", "created_at",
	},
	"eval_scores": {
		"org_id",
	},
}

func TestIntegrationSchemaContractHoldsOnAFreshInstall(t *testing.T) {
	pool := migratedSchema(t)
	ctx := context.Background()

	tables := make([]string, 0, len(expectedColumns))
	for table := range expectedColumns {
		tables = append(tables, table)
	}
	sort.Strings(tables)

	for _, table := range tables {
		rows, err := pool.Query(ctx, `
			SELECT column_name
			FROM information_schema.columns
			WHERE table_name = $1
			  AND table_schema = current_schema()`, table)
		if err != nil {
			t.Fatalf("%s: read columns: %v", table, err)
		}
		present := map[string]bool{}
		for rows.Next() {
			var col string
			if err := rows.Scan(&col); err != nil {
				rows.Close()
				t.Fatalf("%s: scan: %v", table, err)
			}
			present[col] = true
		}
		rows.Close()

		if len(present) == 0 {
			t.Errorf("table %q does not exist after applying every migration. "+
				"Production code queries it, so this install is broken for that "+
				"feature — the same way invite_tokens was.", table)
			continue
		}

		var missing []string
		for _, col := range expectedColumns[table] {
			if !present[col] {
				missing = append(missing, col)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			t.Errorf("table %q is missing %v after applying every migration.\n"+
				"Production SQL in this package names these columns, so the query "+
				"fails with SQLSTATE 42703 on any database built from the migration "+
				"set — including every customer's. Add a migration; do not remove "+
				"the column from the contract.", table, missing)
		}
	}
}

// A schedule must survive a write-then-read against the real schema. The column
// contract above catches a missing column; this catches the rest of the round
// trip — a type that does not scan, a NOT NULL the code does not populate.
func TestIntegrationBenchmarkScheduleRoundTripsOnAFreshInstall(t *testing.T) {
	pool := migratedSchema(t)
	ctx := context.Background()
	store := &Store{pool: pool}

	row := BenchmarkSchedule{
		ID:             "sched-contract-1",
		OrgID:          "org-a",
		Name:           "nightly",
		Environments:   []string{"gsm8k"},
		Model:          "gpt-4o",
		NumExamples:    5,
		Rollouts:       1,
		ViaGateway:     true,
		CadenceSeconds: 3600,
		Enabled:        true,
		CreatedBy:      "u-1",
	}
	if err := store.CreateBenchmarkSchedule(ctx, row); err != nil {
		t.Fatalf("create: %v", err)
	}

	// The read that was failing before migration 016.
	got, err := store.GetBenchmarkSchedule(ctx, row.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "nightly" {
		t.Errorf("round trip lost the name: %+v", got)
	}
	if got.LastRunID != "" || got.LastLaunchedAt != nil {
		t.Errorf("a schedule that has never launched reported a run: id=%q at=%v",
			got.LastRunID, got.LastLaunchedAt)
	}

	// And the write that records a launch.
	if err := store.MarkBenchmarkScheduleLaunched(ctx, row.ID, "run-1", time.Now().UTC()); err != nil {
		t.Fatalf("mark launched: %v", err)
	}
	after, err := store.GetBenchmarkSchedule(ctx, row.ID)
	if err != nil {
		t.Fatalf("get after launch: %v", err)
	}
	if after.LastRunID != "run-1" {
		t.Errorf("last_run_id did not persist: %q", after.LastRunID)
	}
	if after.LastLaunchedAt == nil {
		t.Error("last_launched_at did not persist")
	}
}
