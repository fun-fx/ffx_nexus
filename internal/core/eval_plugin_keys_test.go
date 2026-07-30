package core

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/core/crypto"
)

// These run against a real Postgres because the point of the table is
// surviving a process restart: an in-memory fake would assert nothing.
// Skipped when no database is reachable.

func keysTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("NEXUS_POSTGRES_URL")
	if dsn == "" {
		dsn = "postgres://nexus:nexus@localhost:5432/nexus"
	}
	// 32-byte hex key: the master key protecting provider_credentials
	// has the same shape, so plugin keys inherit its guarantees.
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
	schema, err := os.ReadFile(filepath.Join("..", "..", "migrations", "postgres", "011_eval_plugin_keys.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if err := store.Migrate(ctx, string(schema)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM eval_plugin_keys`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func TestEvalPluginKeysRoundTrip(t *testing.T) {
	store := keysTestStore(t)
	ctx := context.Background()

	want := map[string]string{
		"public_key": "pk-lf-4bcc31ab-267b-48ed-81e5-d4b26c3ec487",
		"secret_key": "sk-lf-4fd2431b-d959-414d-b47e-31d6172007cc",
	}
	if err := store.SaveEvalPluginKeys(ctx, "langfuse-judge", want); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := store.LoadEvalPluginKeys(ctx, "langfuse-judge")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %s: got %q want %q", k, got[k], v)
		}
	}
}

// A database dump must not carry plaintext vendor keys.
func TestEvalPluginKeysEncryptedAtRest(t *testing.T) {
	store := keysTestStore(t)
	ctx := context.Background()

	const secret = "sk-lf-super-secret-value"
	if err := store.SaveEvalPluginKeys(ctx, "langfuse-judge",
		map[string]string{"secret_key": secret}); err != nil {
		t.Fatalf("save: %v", err)
	}

	var ct []byte
	if err := store.pool.QueryRow(ctx,
		`SELECT secret_ciphertext FROM eval_plugin_keys WHERE plugin = $1 AND key_name = $2`,
		"langfuse-judge", "secret_key").Scan(&ct); err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	if bytes.Contains(ct, []byte(secret)) {
		t.Fatal("stored bytes contain the plaintext key")
	}
}

// Saving replaces the whole set: a rotation must not leave the previous
// half of a key pair behind for dispatch to pick up.
func TestEvalPluginKeysSaveReplacesSet(t *testing.T) {
	store := keysTestStore(t)
	ctx := context.Background()

	if err := store.SaveEvalPluginKeys(ctx, "langfuse-judge", map[string]string{
		"public_key": "pk-old",
		"secret_key": "sk-old",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.SaveEvalPluginKeys(ctx, "langfuse-judge", map[string]string{
		"public_key": "pk-new",
	}); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	got, err := store.LoadEvalPluginKeys(ctx, "langfuse-judge")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got["public_key"] != "pk-new" {
		t.Errorf("public_key: got %q want pk-new", got["public_key"])
	}
	if _, ok := got["secret_key"]; ok {
		t.Error("the replaced set must not retain the stale secret_key")
	}
}

// Empty values count as "not configured" everywhere else, so persisting
// them would make the panel report a key that dispatch cannot use.
func TestEvalPluginKeysSkipsEmptyValues(t *testing.T) {
	store := keysTestStore(t)
	ctx := context.Background()

	if err := store.SaveEvalPluginKeys(ctx, "langfuse-judge", map[string]string{
		"public_key": "pk-lf-abc",
		"secret_key": "",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := store.LoadEvalPluginKeys(ctx, "langfuse-judge")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 || got["public_key"] != "pk-lf-abc" {
		t.Fatalf("expected only the non-empty key, got %v", got)
	}
}

func TestEvalPluginKeysDeleteAndOwners(t *testing.T) {
	store := keysTestStore(t)
	ctx := context.Background()

	for _, name := range []string{"langfuse-judge", "braintrust-scorer"} {
		if err := store.SaveEvalPluginKeys(ctx, name,
			map[string]string{"api_key": "value-" + name}); err != nil {
			t.Fatalf("save %s: %v", name, err)
		}
	}

	owners, err := store.ListEvalPluginKeyOwners(ctx)
	if err != nil {
		t.Fatalf("owners: %v", err)
	}
	if len(owners) != 2 || owners[0] != "braintrust-scorer" || owners[1] != "langfuse-judge" {
		t.Fatalf("expected both plugins in sorted order, got %v", owners)
	}

	if err := store.DeleteEvalPluginKeys(ctx, "langfuse-judge"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := store.LoadEvalPluginKeys(ctx, "langfuse-judge")
	if err != nil {
		t.Fatalf("load after delete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no keys after delete, got %v", got)
	}
}

// Without a master key the keys cannot be sealed, and silently storing
// plaintext would be worse than refusing.
func TestEvalPluginKeysRequireCipher(t *testing.T) {
	store := keysTestStore(t)
	store.cipher = nil
	ctx := context.Background()

	if err := store.SaveEvalPluginKeys(ctx, "langfuse-judge",
		map[string]string{"api_key": "v"}); err != crypto.ErrNoMasterKey {
		t.Fatalf("expected ErrNoMasterKey, got %v", err)
	}
	if _, err := store.LoadEvalPluginKeys(ctx, "langfuse-judge"); err != crypto.ErrNoMasterKey {
		t.Fatalf("expected ErrNoMasterKey on load, got %v", err)
	}
}
