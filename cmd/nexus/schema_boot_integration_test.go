package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	nexus "github.com/ffxnexus/nexus"
	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/health"
	"github.com/ffxnexus/nexus/internal/migrate"
)

// The rule this file pins: a pod whose schema is behind its binary must not
// report ready, so Kubernetes never routes customer traffic to it.
//
// Reading applySchemaAtBoot is not enough to know this holds. The gate reports
// ready when every *required* check passes, and a check registered as
// non-required — one word in a call — would produce a pod that answers 200 on
// /readyz while every query against a missing table fails. That is the failure
// mode the readiness split exists to prevent, and it is invisible in review.
//
// Requires a throwaway Postgres:
//
//	NEXUS_TEST_POSTGRES_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' \
//	  go test ./cmd/nexus/ -run Integration -v

func schemaBootPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("NEXUS_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set NEXUS_TEST_POSTGRES_URL to run schema boot integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// Each test gets a schema of its own so the "empty database" case is real.
	schema := "boot_" + strings.ReplaceAll(t.Name(), "/", "_")
	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`,
		`CREATE SCHEMA ` + schema,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("prepare schema: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `SET search_path TO `+schema); err != nil {
		t.Fatalf("set search_path: %v", err)
	}
	// pgxpool hands out pooled connections, so search_path has to be set per
	// connection rather than once on the pool.
	cfg := pool.Config().Copy()
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	scoped, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("scoped pool: %v", err)
	}
	t.Cleanup(scoped.Close)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	})
	return scoped
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// readyzStatus serves the gate the way the console does and returns what a
// kubelet would see.
func readyzStatus(t *testing.T, gate *health.Gate) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	gate.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec.Code, rec.Body.String()
}

// An empty database has every migration outstanding. The pod must answer 503.
func TestIntegrationUnmigratedDatabaseIsNotReady(t *testing.T) {
	pool := schemaBootPool(t)
	gate := health.New()

	applySchemaAtBoot(context.Background(), pool, config.Config{AutoMigrate: false}, gate, quietLogger())

	code, body := readyzStatus(t, gate)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz returned %d on an unmigrated database, want 503; "+
			"Kubernetes would send customer traffic to a pod with no schema. body=%s", code, body)
	}

	// The body has to name the problem, or the operator gets a 503 and no lead.
	var payload struct {
		Ready  bool           `json:"ready"`
		Checks []health.Check `json:"checks"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode /readyz: %v (%s)", err, body)
	}
	if payload.Ready {
		t.Error("payload says ready:true alongside a 503")
	}
	var schemaCheck *health.Check
	for i := range payload.Checks {
		if payload.Checks[i].Name == readyPostgresSchema {
			schemaCheck = &payload.Checks[i]
		}
	}
	if schemaCheck == nil {
		t.Fatalf("no %q check in the readiness payload: %s", readyPostgresSchema, body)
	}
	if !schemaCheck.Required {
		t.Error("the postgres schema check is not Required, so a pending migration " +
			"would not withhold traffic — the gate would report ready with no tables")
	}
	if !strings.Contains(schemaCheck.Detail, "outstanding") {
		t.Errorf("detail does not say migrations are outstanding: %q", schemaCheck.Detail)
	}
	if !strings.Contains(schemaCheck.Detail, "nexus migrate") {
		t.Errorf("detail does not tell the operator what to run: %q", schemaCheck.Detail)
	}
}

// After the migration job has run, the same pod must become ready. Without this
// the test above would pass on a gate that is simply always 503.
func TestIntegrationMigratedDatabaseBecomesReady(t *testing.T) {
	pool := schemaBootPool(t)
	ctx := context.Background()

	migs, err := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if _, err := migrate.Run(ctx, migrate.NewPostgres(pool, "test"), migs, migrate.Options{Logger: quietLogger()}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	gate := health.New()
	applySchemaAtBoot(ctx, pool, config.Config{AutoMigrate: false}, gate, quietLogger())

	if code, body := readyzStatus(t, gate); code != http.StatusOK {
		t.Fatalf("/readyz returned %d after migrations completed, want 200: %s", code, body)
	}
}

// A readiness payload is reachable by anything that can reach the pod's port, so
// it must not carry the database URL or a password even while explaining a
// failure.
func TestIntegrationReadinessPayloadCarriesNoCredentials(t *testing.T) {
	pool := schemaBootPool(t)
	gate := health.New()

	applySchemaAtBoot(context.Background(), pool, config.Config{AutoMigrate: false}, gate, quietLogger())
	_, body := readyzStatus(t, gate)

	for _, leak := range []string{"password", "postgres://", "sslmode", os.Getenv("NEXUS_TEST_POSTGRES_URL")} {
		if leak == "" {
			continue
		}
		if strings.Contains(strings.ToLower(body), strings.ToLower(leak)) {
			t.Errorf("the readiness payload contains %q: %s", leak, body)
		}
	}
}
