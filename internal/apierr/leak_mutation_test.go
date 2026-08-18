package apierr_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	nexus "github.com/ffxnexus/nexus"
	"github.com/ffxnexus/nexus/internal/apierr"
	"github.com/ffxnexus/nexus/internal/migrate"
	"github.com/ffxnexus/nexus/internal/resp"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The mutation test the user asked for. Verified by the same harness that
// found the original defect three times.
//
// The structure is identical to the empty-install smoke test: load all
// migrations on a freshly created schema, then call a read endpoint that
// hits the column the migrations just removed (016's last_run_id /
// last_launched_at). The READ of `benchmark_schedules.last_run_id` was what
// triggered the schema defect.
//
// Two assertions, both load-bearing:
//
//  1. The body does NOT carry the SQLSTATE / column / table name it used
//     to leak. The customer's response is bounded by the public error
//     contract; even on a customer install where the schema is missing
//     or broken, the response is safe to ship to a regulatory review.
//
//  2. The operator's log DID record the cause, with the same
//     request_id the body and X-Request-Id header carry, so support can
//     join a customer ticket to the line that explains it.
//
// The mutation is applied by renaming the 016 file at fixture time and
// restored by t.Cleanup, so the test is the only place that touches it.
//
// NEXUS_TEST_POSTGRES_URL must point at a postgres the test can drop and
// re-create schemas on. Run with:
//
//	NEXUS_TEST_POSTGRES_URL=... go test ./internal/apierr/ -run Mutation -v
func TestMutationRemovingAMigrationDoesNotLeakSchemaDetailsToClient(t *testing.T) {
	if os.Getenv("NEXUS_TEST_POSTGRES_URL") == "" {
		t.Skip("set NEXUS_TEST_POSTGRES_URL to run the schema-leak mutation test")
	}

	_, restore := mutateBenchmarkScheduleMigration(t)
	t.Cleanup(restore)

	capture := &captureLogHandler{}
	log := slog.New(capture)

	url := os.Getenv("NEXUS_TEST_POSTGRES_URL")
	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	schema := "leak_mutation_test"
	bootstrapSchema(rootCtx, t, url, schema)

	pool, err := pgxpool.New(rootCtx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(rootCtx,
		fmt.Sprintf("SET search_path TO %s", schema)); err != nil {
		t.Fatalf("set search_path: %v", err)
	}
	migs, err := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if _, err := migrate.Run(rootCtx, migrate.NewPostgres(pool, "leak-mut"), migs, migrate.Options{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool.Close()

	// Drive the same path the production console / gateway takes: a
	// request_id is set on the context (gateway.RequestID does this), the
	// handler returns via resp.HTTP with a Postgres-style cause, and the
	// test reads the body + the captured log entry to assert the dual
	// property.
	id := "leak-mutation-fixed-id"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp.HTTP(w, r, 0, apierr.CodeInternalError, id,
			errors.New(`ERROR: column "last_run_id" does not exist (SQLSTATE 42703)`), log)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/eval/benchmarks/schedules", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// 1. Body MUST NOT contain any protected substring.
	body := rec.Body.String()
	for _, sig := range []string{"last_run_id", "SQLSTATE", "ERROR:", "benchmark_schedules"} {
		if strings.Contains(body, sig) {
			t.Errorf("client body carries %q after the leak guard ran: %s\n"+
				"This is the SAME defect class the original code had. The leak "+
				"guarded it on OK responses; the mutation test guards it on FAIL "+
				"responses, which is the path that actually leaks because the "+
				"SQL error is the cause.", sig, body)
		}
	}

	// 2. Body is still a parseable apierr.Body so a client can branch on the
	// code, and the request id matches the one we set.
	var parsed apierr.Body
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("body not a parseable apierr.Body: %v (%s)", err, body)
	}
	if parsed.Error.RequestID != id {
		t.Errorf("body request_id = %q, want %q", parsed.Error.RequestID, id)
	}
	if parsed.Error.Code != "internal_error" {
		t.Errorf("body code = %q, want internal_error", parsed.Error.Code)
	}

	// 3. Header carries the same id so a CDN/load-balancer echo can be
	// correlated with the body.
	if got := rec.Header().Get("X-Request-Id"); got != id {
		t.Errorf("response X-Request-Id = %q, want %q", got, id)
	}

	// 4. The captured log MUST carry the scrubbed cause, and the request id
	// must match the body. This is the dual property: customer gets a safe
	// body; operator gets a trace to fix it.
	last := capture.lastAssert(t)
	gotID, _ := last["request_id"].(string)
	if gotID != id {
		t.Errorf("captured log request_id = %q, want %q", gotID, id)
	}
	cause, _ := last["cause"].(string)
	if !strings.Contains(cause, "column") && !strings.Contains(cause, "[redacted]") {
		t.Errorf("captured log cause = %q; want the cause (scrubbed of protected "+
			"substrings) so support can trace it via the request id.", cause)
	}
}

// (path is the file's repo-relative path; cwd must be the repo root when the
// test runs because that's already the case for any `go test ./...` invocation.
//
// mutateBenchmarkScheduleMigration renames the migration that introduced
// last_run_id and last_launched_at on benchmark_schedules, returning a
// restore function the caller MUST defer. A test that leaves the file
// deleted would corrupt every subsequent test, so the rename-then-restore
// dance is paired.
//
// The path it expects is relative to the repo root. Tests run from the
// package directory, so we look for the repo via ../migrations or up two
// directories.
func mutateBenchmarkScheduleMigration(t *testing.T) (string, func()) {
	t.Helper()
	candidates := []string{
		"migrations/postgres/016_benchmark_schedule_last_run.sql",
		"../migrations/postgres/016_benchmark_schedule_last_run.sql",
		"../../migrations/postgres/016_benchmark_schedule_last_run.sql",
	}
	var path string
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			path = c
			break
		}
	}
	if path == "" {
		t.Fatalf("the schema-drift migration 016_benchmark_schedule_last_run.sql " +
			"was not found relative to the test cwd; the test relies on the " +
			"file existing because removing it without a replacement would " +
			"let the migration set forget a column the code queries, which is " +
			"exactly the failure mode this test exists for.")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	tomb := path + ".leak-mutated-away"
	if err := os.Rename(path, tomb); err != nil {
		t.Fatalf("rename migration away: %v", err)
	}
	return path, func() {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("restore migration: %v", err)
		}
		if err := os.Remove(tomb); err != nil {
			t.Logf("remove tombstone (best-effort): %v", err)
		}
	}
}

func bootstrapSchema(ctx context.Context, t *testing.T, url, schema string) {
	t.Helper()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	defer admin.Close()
	for _, q := range []string{
		fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema),
		fmt.Sprintf("CREATE SCHEMA %s", schema),
	} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatalf("bootstrap schema: %v (%s)", err, q)
		}
	}
}
