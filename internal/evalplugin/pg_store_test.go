package evalplugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The plugin store is the record that an evaluation vendor is wired up
// at all, so these tests run against a real Postgres: the upsert and
// the (org_id, name) uniqueness handling are the parts that cannot be
// verified against an in-memory fake.
//
// Skipped when no database is reachable, mirroring the ClickHouse
// integration suite.

func pgDSN() string {
	if d := os.Getenv("NEXUS_POSTGRES_URL"); d != "" {
		return d
	}
	return "postgres://nexus:nexus@localhost:5432/nexus"
}

func openPluginStore(t *testing.T) (*PostgresStore, *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, pgDSN())
	if err != nil {
		t.Skipf("postgres not configured at %s: %v", pgDSN(), err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres not reachable at %s: %v", pgDSN(), err)
	}
	schema, err := os.ReadFile(filepath.Join("..", "..", "migrations", "postgres", "009_eval_plugins.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(schema)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM eval_plugins`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return NewPostgresStore(pool), pool
}

const testManifest = `apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: langfuse-judge
spec:
  service:
    type: langfuse
    endpoint: https://cloud.langfuse.com
    auth:
      secretRef: langfuse-judge
      keyRef: public_key|secret_key
  send:
    trigger: on_trace
    sampling: 1
  collect:
    mode: poll
    interval: 60s
    mapping:
      name: name
      score: value
      trace_id: traceId
  timeout: 30s
`

func TestPostgresStoreSaveRoundTrip(t *testing.T) {
	store, _ := openPluginStore(t)
	ctx := context.Background()

	rec := &PluginRecord{Name: "langfuse-judge", SpecYAML: testManifest, Enabled: true}
	if err := store.Save(ctx, rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	if rec.ID == "" {
		t.Fatal("save must stamp an id back onto the record")
	}
	if rec.CreatedAt.IsZero() || rec.UpdatedAt.IsZero() {
		t.Fatal("save must stamp created_at/updated_at back onto the record")
	}

	got, err := store.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "langfuse-judge" || !got.Enabled || got.SpecYAML != testManifest {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

// A console install and a console edit are the same manifest submit, so
// re-saving a name that already exists has to update that row rather
// than trip the (org_id, name) unique constraint.
func TestPostgresStoreSaveAdoptsExistingName(t *testing.T) {
	store, _ := openPluginStore(t)
	ctx := context.Background()

	first := &PluginRecord{Name: "langfuse-judge", SpecYAML: testManifest, Enabled: true}
	if err := store.Save(ctx, first); err != nil {
		t.Fatalf("save first: %v", err)
	}

	second := &PluginRecord{Name: "langfuse-judge", SpecYAML: testManifest, Enabled: false}
	if err := store.Save(ctx, second); err != nil {
		t.Fatalf("save second: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("expected the second save to adopt id %s, got %s", first.ID, second.ID)
	}

	all, err := store.List(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 row after re-save, got %d", len(all))
	}
	if all[0].Enabled {
		t.Error("expected the re-save to apply enabled=false")
	}
}

// Renaming through the manifest keeps the same row: the console patches
// spec_yaml in place and the store must follow the id, not the name.
func TestPostgresStoreSaveRenamesInPlace(t *testing.T) {
	store, _ := openPluginStore(t)
	ctx := context.Background()

	rec := &PluginRecord{Name: "langfuse-judge", SpecYAML: testManifest, Enabled: true}
	if err := store.Save(ctx, rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	id := rec.ID

	rec.Name = "langfuse-eu"
	if err := store.Save(ctx, rec); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if rec.ID != id {
		t.Errorf("rename must keep id %s, got %s", id, rec.ID)
	}
	all, err := store.List(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || all[0].Name != "langfuse-eu" {
		t.Fatalf("expected a single renamed row, got %+v", all)
	}
}

// Cluster-wide rows (org_id="") must be visible to every org, and an
// org's own rows must not leak into another org's list.
func TestPostgresStoreListOrgScoping(t *testing.T) {
	store, _ := openPluginStore(t)
	ctx := context.Background()

	cluster := &PluginRecord{Name: "cluster-wide", SpecYAML: testManifest, Enabled: true}
	if err := store.Save(ctx, cluster); err != nil {
		t.Fatalf("save cluster: %v", err)
	}
	mine := &PluginRecord{OrgID: "org-a", Name: "org-a-plugin", SpecYAML: testManifest, Enabled: true}
	if err := store.Save(ctx, mine); err != nil {
		t.Fatalf("save org-a: %v", err)
	}
	theirs := &PluginRecord{OrgID: "org-b", Name: "org-b-plugin", SpecYAML: testManifest, Enabled: true}
	if err := store.Save(ctx, theirs); err != nil {
		t.Fatalf("save org-b: %v", err)
	}

	forA, err := store.List(ctx, "org-a")
	if err != nil {
		t.Fatalf("list org-a: %v", err)
	}
	names := map[string]bool{}
	for _, r := range forA {
		names[r.Name] = true
	}
	if !names["cluster-wide"] || !names["org-a-plugin"] {
		t.Errorf("org-a must see its own and cluster-wide rows, got %v", names)
	}
	if names["org-b-plugin"] {
		t.Error("org-a must not see org-b's plugin")
	}

	// Boot hydrates the registry with the whole deployment.
	all, err := store.List(ctx, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("empty org must list every row, got %d", len(all))
	}
}

func TestPostgresStoreDelete(t *testing.T) {
	store, _ := openPluginStore(t)
	ctx := context.Background()

	rec := &PluginRecord{Name: "langfuse-judge", SpecYAML: testManifest, Enabled: true}
	if err := store.Save(ctx, rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.Delete(ctx, rec.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, rec.ID); err != ErrPluginNotFound {
		t.Fatalf("expected ErrPluginNotFound after delete, got %v", err)
	}
}

func TestPostgresStoreRejectsInvalidManifest(t *testing.T) {
	store, _ := openPluginStore(t)
	ctx := context.Background()

	if err := store.Save(ctx, &PluginRecord{Name: "bad", SpecYAML: "not: a: manifest"}); err == nil {
		t.Fatal("expected an invalid manifest to be rejected before it reaches the table")
	}
}

// NewPostgresStore(nil) returning nil is what lets main.go fall back to
// MemoryStore with a plain nil check.
func TestNewPostgresStoreNilPool(t *testing.T) {
	if s := NewPostgresStore(nil); s != nil {
		t.Fatal("a nil pool must yield a nil store so callers can fall back")
	}
}
