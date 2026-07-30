package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ffxnexus/nexus/internal/evalplugin"
)

// fakeVault stands in for core.Store. The Postgres behaviour it models
// is covered by internal/core/eval_plugin_keys_test.go; here we care
// about the resolver's cache/persistence contract.
type fakeVault struct {
	mu       sync.Mutex
	data     map[string]map[string]string
	saveErr  error
	loadErr  error
	loads    int
	saves    int
	deletes  int
	ownerErr error
}

func newFakeVault() *fakeVault {
	return &fakeVault{data: make(map[string]map[string]string)}
}

func (v *fakeVault) SaveEvalPluginKeys(_ context.Context, plugin string, kv map[string]string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.saves++
	if v.saveErr != nil {
		return v.saveErr
	}
	if len(kv) == 0 {
		delete(v.data, plugin)
		return nil
	}
	cp := make(map[string]string, len(kv))
	for k, val := range kv {
		cp[k] = val
	}
	v.data[plugin] = cp
	return nil
}

func (v *fakeVault) LoadEvalPluginKeys(_ context.Context, plugin string) (map[string]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.loads++
	if v.loadErr != nil {
		return nil, v.loadErr
	}
	src := v.data[plugin]
	out := make(map[string]string, len(src))
	for k, val := range src {
		out[k] = val
	}
	return out, nil
}

func (v *fakeVault) DeleteEvalPluginKeys(_ context.Context, plugin string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.deletes++
	delete(v.data, plugin)
	return nil
}

func (v *fakeVault) ListEvalPluginKeyOwners(_ context.Context) ([]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ownerErr != nil {
		return nil, v.ownerErr
	}
	out := make([]string, 0, len(v.data))
	for k := range v.data {
		out = append(out, k)
	}
	sort := func(s []string) {
		for i := 1; i < len(s); i++ {
			for j := i; j > 0 && s[j-1] > s[j]; j-- {
				s[j-1], s[j] = s[j], s[j-1]
			}
		}
	}
	sort(out)
	return out, nil
}

func langfuseAuth() evalplugin.AuthSpec {
	return evalplugin.AuthSpec{SecretRef: "langfuse-judge", KeyRef: "public_key|secret_key"}
}

func TestSetPersistsKeysToVault(t *testing.T) {
	vault := newFakeVault()
	r := newConsoleKeyResolver(vault)

	if err := r.Set("langfuse-judge", map[string]string{
		"public_key": "pk-lf-abc",
		"secret_key": "sk-lf-xyz",
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if vault.data["langfuse-judge"]["secret_key"] != "sk-lf-xyz" {
		t.Fatalf("keys did not reach the vault: %v", vault.data)
	}
}

// The regression this whole change exists for: a rolling update gives
// the process a fresh resolver, and dispatch has to keep working
// without the operator re-pasting anything.
func TestKeysSurviveProcessRestart(t *testing.T) {
	vault := newFakeVault()
	before := newConsoleKeyResolver(vault)
	if err := before.Set("langfuse-judge", map[string]string{
		"public_key": "pk-lf-abc",
		"secret_key": "sk-lf-xyz",
	}); err != nil {
		t.Fatalf("set: %v", err)
	}

	after := newConsoleKeyResolver(vault) // new pod, empty cache
	creds, err := after.Resolve(context.Background(), langfuseAuth())
	if err != nil {
		t.Fatalf("resolve after restart: %v", err)
	}
	if len(creds.Values) != 2 || creds.Values[0] != "pk-lf-abc" || creds.Values[1] != "sk-lf-xyz" {
		t.Fatalf("unexpected credentials after restart: %v", creds.Values)
	}
	if !after.Has("langfuse-judge") {
		t.Error("the panel must report the plugin as configured after a restart")
	}
}

// A read-through populates the cache, so the eval worker pays at most
// one database round-trip per plugin.
func TestResolveCachesVaultRead(t *testing.T) {
	vault := newFakeVault()
	vault.data["langfuse-judge"] = map[string]string{
		"public_key": "pk", "secret_key": "sk",
	}
	r := newConsoleKeyResolver(vault)

	for i := 0; i < 3; i++ {
		if _, err := r.Resolve(context.Background(), langfuseAuth()); err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
	}
	if vault.loads != 1 {
		t.Errorf("expected 1 vault read, got %d", vault.loads)
	}
}

func TestHydrateWarmsCache(t *testing.T) {
	vault := newFakeVault()
	vault.data["langfuse-judge"] = map[string]string{"public_key": "pk", "secret_key": "sk"}
	vault.data["braintrust-scorer"] = map[string]string{"api_key": "bt"}
	r := newConsoleKeyResolver(vault)

	n, err := r.Hydrate(context.Background())
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 hydrated plugins, got %d", n)
	}
	loadsAfterHydrate := vault.loads
	if _, err := r.Resolve(context.Background(), langfuseAuth()); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if vault.loads != loadsAfterHydrate {
		t.Error("a hydrated plugin must resolve from cache without another read")
	}
}

// A key that reaches memory but not the database looks configured until
// the next restart quietly unconfigures it, so Set has to say so while
// still leaving the plugin usable right now.
func TestSetReportsPersistenceFailureButStaysUsable(t *testing.T) {
	vault := newFakeVault()
	vault.saveErr = errors.New("connection refused")
	r := newConsoleKeyResolver(vault)

	err := r.Set("langfuse-judge", map[string]string{
		"public_key": "pk-lf-abc",
		"secret_key": "sk-lf-xyz",
	})
	if err == nil {
		t.Fatal("expected Set to report the failed persistence")
	}
	if !r.Has("langfuse-judge") {
		t.Error("the keys must still work in this process")
	}
	if _, resolveErr := r.Resolve(context.Background(), langfuseAuth()); resolveErr != nil {
		t.Errorf("resolve should succeed from cache: %v", resolveErr)
	}
}

func TestClearRemovesKeysFromVault(t *testing.T) {
	vault := newFakeVault()
	r := newConsoleKeyResolver(vault)
	if err := r.Set("langfuse-judge", map[string]string{"public_key": "pk", "secret_key": "sk"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := r.Clear("langfuse-judge"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if vault.deletes != 1 {
		t.Errorf("expected the vault delete to run, got %d calls", vault.deletes)
	}
	if r.Has("langfuse-judge") {
		t.Error("cleared keys must not resolve")
	}
	// A stale vault entry would resurrect the key on the next read-through.
	if _, ok := vault.data["langfuse-judge"]; ok {
		t.Error("vault still holds the cleared keys")
	}
}

// A failing vault read must surface as a dispatch error rather than
// "no keys set", which would send the operator to re-paste keys that
// are already there.
func TestResolveSurfacesVaultReadError(t *testing.T) {
	vault := newFakeVault()
	vault.loadErr = errors.New("connection refused")
	r := newConsoleKeyResolver(vault)

	_, err := r.Resolve(context.Background(), langfuseAuth())
	if err == nil {
		t.Fatal("expected an error when the vault read fails")
	}
	if !errors.Is(err, vault.loadErr) {
		t.Errorf("expected the vault error to be wrapped, got %v", err)
	}
}

// Deployments without a control plane keep the old memory-only path.
func TestNilVaultKeepsMemoryOnlyBehaviour(t *testing.T) {
	r := newConsoleKeyResolver(nil)
	if err := r.Set("langfuse-judge", map[string]string{"public_key": "pk", "secret_key": "sk"}); err != nil {
		t.Fatalf("set with no vault must succeed: %v", err)
	}
	if n, err := r.Hydrate(context.Background()); err != nil || n != 0 {
		t.Fatalf("hydrate with no vault: n=%d err=%v", n, err)
	}
	if _, err := r.Resolve(context.Background(), langfuseAuth()); err != nil {
		t.Fatalf("resolve: %v", err)
	}
}
