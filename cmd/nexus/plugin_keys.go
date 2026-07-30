package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evaluators/external"
)

// consoleKeyResolver resolves a plugin manifest's auth block from
// values the operator pasted into the in-product console (the Plugin
// Keys panel). It is the sole credential surface after this PR — the
// previous envSecretResolver path (which required a Kubernetes Secret
// and an envFrom projection through the Helm chart) is deprecated.
//
// Storage rules:
//
//   - Values are encrypted at rest in eval_plugin_keys through the
//     vault (core.Store) and cached in process memory. Memory alone
//     was the original design and it made the feature unusable: a
//     rolling update wiped every key, so dispatch failed auth on
//     every trace while the console still listed the plugin as
//     enabled and the Test button — run right after a paste, in the
//     same process — kept passing.
//
//   - Deployments without a control-plane database pass a nil vault
//     and keep the old memory-only behaviour.
//
//   - Looks up are concurrent-safe; dispatch and collect run on
//     different goroutines, and a UI submit can race them.
//
//   - Empty values count as missing: a stored nil vs a stored "" both
//     surface as a "not configured" error so dispatch can fail loudly
//     rather than sending an empty header.
//
// KeyRef semantics:
//
//   - The manifest's `auth.secretRef` is treated as the plugin's unique
//     name (no references to Kubernetes Secret objects are left).
//   - The manifest's `auth.keyRef` is a pipe-separated list of names
//     inside the per-plugin key dict, in the order they should appear
//     in `external.Credentials.Values`. For Langfuse this is
//     `public_key|secret_key` so Basic auth gets (public, secret) in
//     the correct order.
type consoleKeyResolver struct {
	mu   sync.RWMutex
	keys map[string]map[string]string // plugin-name -> key-name -> value

	vault pluginKeyVault
}

// pluginKeyVault is the durable backend for pasted plugin keys.
// Implemented by *core.Store; nil in single-binary deployments.
type pluginKeyVault interface {
	SaveEvalPluginKeys(ctx context.Context, plugin string, kv map[string]string) error
	LoadEvalPluginKeys(ctx context.Context, plugin string) (map[string]string, error)
	DeleteEvalPluginKeys(ctx context.Context, plugin string) error
	ListEvalPluginKeyOwners(ctx context.Context) ([]string, error)
}

// vaultTimeout bounds the read-through a cache miss performs on the
// eval worker's goroutine. Dispatch is best-effort, so a stalled
// database must surface as a dispatch error rather than a stuck trace.
const vaultTimeout = 5 * time.Second

func newConsoleKeyResolver(vault pluginKeyVault) *consoleKeyResolver {
	return &consoleKeyResolver{
		keys:  make(map[string]map[string]string),
		vault: vault,
	}
}

// Set replaces the key map for a single plugin. UI submit writes here.
// The map is owned by the resolver after this call; callers must not
// mutate it. Passing a nil value clears the plugin's keys, equivalent
// to Clear(name).
//
// The cache is updated before the vault write so the keys work in this
// process even when persistence fails; the returned error tells the
// operator they would not survive a restart, which is exactly the
// failure mode that used to be invisible.
func (r *consoleKeyResolver) Set(plugin string, kv map[string]string) error {
	c := make(map[string]string, len(kv))
	for k, v := range kv {
		if v != "" {
			c[k] = v
		}
	}

	r.mu.Lock()
	if r.keys == nil {
		r.keys = make(map[string]map[string]string)
	}
	if len(c) == 0 {
		delete(r.keys, plugin)
	} else {
		r.keys[plugin] = c
	}
	r.mu.Unlock()

	if r.vault == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), vaultTimeout)
	defer cancel()
	if err := r.vault.SaveEvalPluginKeys(ctx, plugin, c); err != nil {
		return fmt.Errorf("keys are active in this process but were not persisted, "+
			"so a restart will lose them: %w", err)
	}
	return nil
}

// Get returns a snapshot of the configured keys for a plugin. Used by
// the GET REST endpoint and by tests; values are returned as-is so
// the caller must treat them as sensitive.
//
// A cache miss falls through to the vault so the panel reports the
// truth on a pod that has not served a submit yet.
func (r *consoleKeyResolver) Get(plugin string) (map[string]string, bool) {
	if out, ok := r.cached(plugin); ok {
		return out, true
	}
	out, err := r.loadFromVault(plugin)
	if err != nil || len(out) == 0 {
		return nil, false
	}
	return out, true
}

// Clear removes all stored keys for a plugin, in memory and at rest.
func (r *consoleKeyResolver) Clear(plugin string) error {
	r.mu.Lock()
	delete(r.keys, plugin)
	r.mu.Unlock()
	if r.vault == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), vaultTimeout)
	defer cancel()
	return r.vault.DeleteEvalPluginKeys(ctx, plugin)
}

// Has reports whether a plugin has any keys configured. Used by the
// REST endpoint to decide whether to show the modal in "saved" state.
func (r *consoleKeyResolver) Has(plugin string) bool {
	_, ok := r.Get(plugin)
	return ok
}

// Hydrate warms the cache from the vault so the first trace after a
// restart does not pay a database round-trip on the eval worker's
// goroutine. Returns the number of plugins that have keys stored.
func (r *consoleKeyResolver) Hydrate(ctx context.Context) (int, error) {
	if r.vault == nil {
		return 0, nil
	}
	names, err := r.vault.ListEvalPluginKeyOwners(ctx)
	if err != nil {
		return 0, err
	}
	loaded := 0
	for _, name := range names {
		kv, err := r.vault.LoadEvalPluginKeys(ctx, name)
		if err != nil {
			return loaded, fmt.Errorf("load keys for plugin %q: %w", name, err)
		}
		if len(kv) == 0 {
			continue
		}
		r.mu.Lock()
		r.keys[name] = kv
		r.mu.Unlock()
		loaded++
	}
	return loaded, nil
}

// cached returns a copy of the in-memory entry for a plugin.
func (r *consoleKeyResolver) cached(plugin string) (map[string]string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src, ok := r.keys[plugin]
	if !ok || len(src) == 0 {
		return nil, false
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out, true
}

// loadFromVault reads a plugin's keys from the durable store and
// promotes them into the cache.
func (r *consoleKeyResolver) loadFromVault(plugin string) (map[string]string, error) {
	if r.vault == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), vaultTimeout)
	defer cancel()
	kv, err := r.vault.LoadEvalPluginKeys(ctx, plugin)
	if err != nil {
		return nil, err
	}
	if len(kv) == 0 {
		return nil, nil
	}
	r.mu.Lock()
	if r.keys == nil {
		r.keys = make(map[string]map[string]string)
	}
	r.keys[plugin] = kv
	r.mu.Unlock()
	out := make(map[string]string, len(kv))
	for k, v := range kv {
		out[k] = v
	}
	return out, nil
}

// Resolve implements external.SecretResolver. The manifest's
// `secretRef` is interpreted as the plugin's unique name, not a
// Kubernetes Secret name. Plugins without an auth block (self-hosted
// OTel collectors, in-process heuristics) resolve to empty credentials
// without consulting the resolver.
func (r *consoleKeyResolver) Resolve(_ context.Context, auth evalplugin.AuthSpec) (external.Credentials, error) {
	if auth.SecretRef == "" && auth.KeyRef == "" {
		// Self-hosted plugin needs no key. Don't even touch the map.
		return external.Credentials{}, nil
	}
	if auth.SecretRef == "" {
		return external.Credentials{}, errors.New(
			"auth.secretRef is empty (operator must paste the keys against a plugin " +
				"name that matches the saved manifest's metadata.name)")
	}
	keys := splitKeyRef(auth.KeyRef)
	if len(keys) == 0 {
		return external.Credentials{}, fmt.Errorf(
			"auth.keyRef is empty for plugin %q: name the keys the manifest needs, e.g. keyRef: public_key|secret_key",
			auth.SecretRef)
	}

	stored, ok := r.cached(auth.SecretRef)
	if !ok {
		var err error
		stored, err = r.loadFromVault(auth.SecretRef)
		if err != nil {
			return external.Credentials{}, fmt.Errorf(
				"load stored keys for plugin %q: %w", auth.SecretRef, err)
		}
	}
	if len(stored) == 0 {
		return external.Credentials{}, fmt.Errorf(
			"no keys set for plugin %q: paste them in the Plugin Keys panel and retry",
			auth.SecretRef)
	}

	out := make([]string, 0, len(keys))
	var missing []string
	for _, k := range keys {
		v, hit := stored[k]
		if !hit || v == "" {
			missing = append(missing, k)
			continue
		}
		out = append(out, v)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return external.Credentials{}, fmt.Errorf(
			"plugin %q is missing key(s) %s: paste them in the Plugin Keys panel and retry",
			auth.SecretRef, strings.Join(missing, ", "))
	}
	return external.Credentials{Values: out}, nil
}

// maskKey returns a UI/log-safe representation of a credential value.
// The intent is that operators reviewing error messages or log lines
// can see which key is configured without leaking bytes that would
// let an attacker reconstruct the secret.
//
// The shape keeps the kind prefix (the part before the first dash for
// every vendor Nexus supports today) and replaces the middle with a
// fixed-length marker; this is enough to disambiguate the two Langfuse
// keys in an error message while still trailing six hex-style
// characters that look like an arbiter-shown chunk for fields that
// have no dash-separated prefix (e.g. a plain bearer token).
func maskKey(v string) string {
	if v == "" {
		return ""
	}
	if len(v) <= 8 {
		return "***"
	}
	if idx := strings.IndexByte(v, '-'); idx > 0 && idx < len(v)-4 {
		// "pk-lf-XXXXX..." → "pk-lf-***XXXX"
		return v[:idx+1] + "***" + v[len(v)-4:]
	}
	return "***" + v[len(v)-4:]
}
