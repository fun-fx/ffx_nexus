package core

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	nexus "github.com/ffxnexus/nexus"
	"github.com/ffxnexus/nexus/internal/core/crypto"
	"github.com/ffxnexus/nexus/internal/migrate"
)

// Runs against a real Postgres: the value of this table is that a run
// survives the poller restarting mid-flight, and the text[] and jsonb
// round-trips are exactly what a fake would paper over.

// bootDBSchema applies the *production* migration set end-to-end on a
// fresh database. A partial list (e.g. manually applying only 012 and
// 013) is the bug class that hid the missing 016 columns from CI; the
// helper pins us to migrate.Load(nexus.Migrations, ...) so the test
// database carries the same shape as a customer's first boot.
//
// Performance: every test that calls bootDBSchema pays one
// migrate.Run round-trip to load migrations, but the migration ledger
// short-circuits re-runs because all migrations have already been
// applied. Subsequent calls in the same package run therefore only run
// the EnsureLedger check.
//
// Phase D-1 note: 023/024 are hot in development, so we accept
// checksum drift on pre-existing ledger rows. The package-level lease
// integration tests reuse this path during Phase D-1 work; allowing
// drift here is a temporary concession and must NOT propagate to
// production boot policies. Production migrates are strict and will
// refuse drift.
func bootDBSchema(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	migs, err := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	if err != nil {
		t.Fatalf("migrate.Load: %v", err)
	}
	exec := migrate.NewPostgres(store.pool, "test-"+t.Name())
	if _, err := migrate.Run(ctx, exec, migs, migrate.Options{
		Logger:              slog.Default(),
		AllowChecksumDrift: true,
	}); err != nil {
		t.Fatalf("migrate.Run: %v", err)
	}
}

func benchTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("NEXUS_POSTGRES_URL")
	if dsn == "" {
		dsn = "postgres://nexus:nexus@localhost:5432/nexus"
	}
	cipher, err := crypto.NewCipher(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := NewStore(ctx, dsn, cipher)
	if err != nil {
		t.Skipf("postgres not reachable at %s: %v", dsn, err)
	}
	// Apply every embedded migration the production boot brings along
	// — and only those. The numeric order, idempotency, and ledger
	// updates are the migration package's contract; the test must
	// replay the production path verbatim, not a hand-picked subset.
	bootDBSchema(t, ctx, store)
	if _, err := store.pool.Exec(ctx, `DELETE FROM benchmark_runs`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func seedRun(t *testing.T, store *Store, id, org, status string) BenchmarkRun {
	t.Helper()
	r := BenchmarkRun{
		ID:           id,
		OrgID:        org,
		Provider:     "primeintellect",
		Name:         "nightly " + id,
		Environments: []string{"ffx/gsm8k", "ffx/alphabet-sort"},
		Model:        "gpt-4o-mini",
		NumExamples:  20,
		Rollouts:     2,
		ViaGateway:   true,
		VKeyID:       "vk-" + id,
		Status:       status,
		CreatedBy:    "user-1",
	}
	if err := store.CreateBenchmarkRun(context.Background(), r); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	return r
}

func TestBenchmarkRunRoundTrip(t *testing.T) {
	store := benchTestStore(t)
	ctx := context.Background()
	seedRun(t, store, "run-1", "org-a", "pending")

	got, err := store.GetBenchmarkRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Model != "gpt-4o-mini" || got.NumExamples != 20 || got.Rollouts != 2 {
		t.Errorf("scalars wrong: %+v", got)
	}
	// text[] must come back as a real slice, in order.
	if len(got.Environments) != 2 || got.Environments[0] != "ffx/gsm8k" {
		t.Errorf("environments = %#v", got.Environments)
	}
	if !got.ViaGateway || got.VKeyID != "vk-run-1" {
		t.Errorf("gateway fields wrong: %+v", got)
	}
	if got.Status != "pending" {
		t.Errorf("status = %q", got.Status)
	}
	// Nothing has scored it yet, so the aggregate must be absent
	// rather than a misleading zero.
	if got.AvgScore != nil || got.TotalSamples != nil {
		t.Errorf("unscored run has values: avg=%v samples=%v", got.AvgScore, got.TotalSamples)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps not defaulted: %+v", got)
	}
}

func TestBenchmarkRunCreateRejectsIncompleteRows(t *testing.T) {
	store := benchTestStore(t)
	ctx := context.Background()
	base := BenchmarkRun{ID: "x", Provider: "primeintellect", Model: "m", Status: "pending"}

	cases := map[string]func(*BenchmarkRun){
		"no id":       func(r *BenchmarkRun) { r.ID = "" },
		"no model":    func(r *BenchmarkRun) { r.Model = "" },
		"no provider": func(r *BenchmarkRun) { r.Provider = "" },
		"no status":   func(r *BenchmarkRun) { r.Status = "" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			r := base
			mut(&r)
			if err := store.CreateBenchmarkRun(ctx, r); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestBenchmarkRunCreateAcceptsNilEnvironments(t *testing.T) {
	// A nil slice must not trip the NOT NULL default on text[].
	store := benchTestStore(t)
	ctx := context.Background()
	r := BenchmarkRun{ID: "run-nil", Provider: "primeintellect", Model: "m", Status: "pending"}
	if err := store.CreateBenchmarkRun(ctx, r); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.GetBenchmarkRun(ctx, "run-nil")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Environments) != 0 {
		t.Fatalf("environments = %#v", got.Environments)
	}
}

func TestBenchmarkRunProgressWritesAggregate(t *testing.T) {
	store := benchTestStore(t)
	ctx := context.Background()
	seedRun(t, store, "run-2", "org-a", "pending")

	avg, min, max := 0.82, 0.0, 1.0
	samples := 40
	started := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	done := time.Now().UTC().Truncate(time.Second)

	err := store.UpdateBenchmarkRunProgress(ctx, BenchmarkRun{
		ID:             "run-2",
		ExternalID:     "ev_abc",
		Status:         "completed",
		ExternalStatus: "COMPLETED",
		AvgScore:       &avg,
		MinScore:       &min,
		MaxScore:       &max,
		TotalSamples:   &samples,
		Metrics:        json.RawMessage(`{"accuracy":0.82}`),
		ViewerURL:      "https://app.primeintellect.ai/evals/ev_abc",
		StartedAt:      &started,
		CompletedAt:    &done,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := store.GetBenchmarkRun(ctx, "run-2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ExternalID != "ev_abc" || got.Status != "completed" || got.ExternalStatus != "COMPLETED" {
		t.Errorf("status fields wrong: %+v", got)
	}
	if got.AvgScore == nil || *got.AvgScore != 0.82 {
		t.Errorf("avg = %v", got.AvgScore)
	}
	// A genuine zero must persist as zero, not as NULL.
	if got.MinScore == nil || *got.MinScore != 0 {
		t.Errorf("min = %v, want a stored zero", got.MinScore)
	}
	if got.TotalSamples == nil || *got.TotalSamples != 40 {
		t.Errorf("samples = %v", got.TotalSamples)
	}
	var metrics map[string]float64
	if err := json.Unmarshal(got.Metrics, &metrics); err != nil {
		t.Fatalf("metrics not valid json: %v (%s)", err, got.Metrics)
	}
	if metrics["accuracy"] != 0.82 {
		t.Errorf("metrics = %v", metrics)
	}
	if got.StartedAt == nil || got.CompletedAt == nil {
		t.Errorf("timestamps missing: %+v", got)
	}
}

func TestBenchmarkRunProgressPreservesFieldsItIsNotTold(t *testing.T) {
	// The poller reports status on every pass but only learns the
	// viewer URL and metrics once. A later status-only update must not
	// erase what an earlier one established.
	store := benchTestStore(t)
	ctx := context.Background()
	seedRun(t, store, "run-3", "org-a", "pending")

	first := BenchmarkRun{
		ID: "run-3", ExternalID: "ev_1", Status: "running", ExternalStatus: "RUNNING",
		Metrics:   json.RawMessage(`{"partial":1}`),
		ViewerURL: "https://app.primeintellect.ai/evals/ev_1",
	}
	if err := store.UpdateBenchmarkRunProgress(ctx, first); err != nil {
		t.Fatalf("first update: %v", err)
	}
	second := BenchmarkRun{ID: "run-3", Status: "running", ExternalStatus: "PROCESSING"}
	if err := store.UpdateBenchmarkRunProgress(ctx, second); err != nil {
		t.Fatalf("second update: %v", err)
	}
	got, err := store.GetBenchmarkRun(ctx, "run-3")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ExternalID != "ev_1" {
		t.Errorf("external id lost: %q", got.ExternalID)
	}
	if got.ViewerURL == "" {
		t.Error("viewer url lost on a status-only update")
	}
	if len(got.Metrics) == 0 {
		t.Error("metrics lost on a status-only update")
	}
	if got.ExternalStatus != "PROCESSING" {
		t.Errorf("external status = %q, want the newer value", got.ExternalStatus)
	}
}

func TestBenchmarkRunProgressUnknownIDIsNotFound(t *testing.T) {
	store := benchTestStore(t)
	err := store.UpdateBenchmarkRunProgress(context.Background(),
		BenchmarkRun{ID: "nope", Status: "running"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestBenchmarkRunListScopesByOrg(t *testing.T) {
	store := benchTestStore(t)
	ctx := context.Background()
	seedRun(t, store, "run-a1", "org-a", "pending")
	seedRun(t, store, "run-a2", "org-a", "running")
	seedRun(t, store, "run-b1", "org-b", "pending")

	a, err := store.ListBenchmarkRuns(ctx, "org-a", 0)
	if err != nil {
		t.Fatalf("list org-a: %v", err)
	}
	if len(a) != 2 {
		t.Fatalf("org-a saw %d runs, want 2", len(a))
	}
	for _, r := range a {
		if r.OrgID != "org-a" {
			t.Errorf("leaked %s from %s", r.ID, r.OrgID)
		}
	}
	// An empty org means "every org", which the poller relies on.
	all, err := store.ListBenchmarkRuns(ctx, "", 0)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("cluster-wide list saw %d runs, want 3", len(all))
	}
}

func TestBenchmarkRunListUnsettledSkipsSettledAndUnlaunched(t *testing.T) {
	store := benchTestStore(t)
	ctx := context.Background()

	// Launched and still going: must be polled.
	seedRun(t, store, "run-live", "org-a", "running")
	if err := store.UpdateBenchmarkRunProgress(ctx, BenchmarkRun{
		ID: "run-live", ExternalID: "ev_live", Status: "running", ExternalStatus: "RUNNING",
	}); err != nil {
		t.Fatalf("update live: %v", err)
	}
	// Settled: nothing left to ask about.
	seedRun(t, store, "run-done", "org-a", "running")
	if err := store.UpdateBenchmarkRunProgress(ctx, BenchmarkRun{
		ID: "run-done", ExternalID: "ev_done", Status: "completed", ExternalStatus: "COMPLETED",
	}); err != nil {
		t.Fatalf("update done: %v", err)
	}
	// Launch never returned an id: there is nothing to poll for, so
	// polling it would 404 forever.
	seedRun(t, store, "run-orphan", "org-a", "pending")

	got, err := store.ListUnsettledBenchmarkRuns(ctx, 0)
	if err != nil {
		t.Fatalf("list unsettled: %v", err)
	}
	if len(got) != 1 || got[0].ID != "run-live" {
		ids := make([]string, len(got))
		for i, r := range got {
			ids[i] = r.ID
		}
		t.Fatalf("unsettled = %v, want just run-live", ids)
	}
}

func TestBenchmarkRunClearVKeyAndDelete(t *testing.T) {
	store := benchTestStore(t)
	ctx := context.Background()
	seedRun(t, store, "run-4", "org-a", "pending")

	if err := store.ClearBenchmarkRunVKey(ctx, "run-4"); err != nil {
		t.Fatalf("clear vkey: %v", err)
	}
	got, err := store.GetBenchmarkRun(ctx, "run-4")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.VKeyID != "" {
		t.Errorf("vkey_id = %q, want cleared", got.VKeyID)
	}

	if err := store.DeleteBenchmarkRun(ctx, "run-4"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.GetBenchmarkRun(ctx, "run-4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound after delete", err)
	}
	if err := store.DeleteBenchmarkRun(ctx, "run-4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v, want ErrNotFound", err)
	}
}

func TestBenchmarkRunGetUnknownIsNotFound(t *testing.T) {
	store := benchTestStore(t)
	if _, err := store.GetBenchmarkRun(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
