package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

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
//   - Values are kept in process memory only. Nothing is written to
//     disk, so a pod restart or rolling update wipes them and the
//     operator re-pastes the next time. This matches the behaviour of
//     the other Nexus vendor keys (OpenAI / Groq / Mistral / TheGrid /
//     Google / DeepSeek) that the console already manages this way.
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
}

func newConsoleKeyResolver() *consoleKeyResolver {
	return &consoleKeyResolver{
		keys: make(map[string]map[string]string),
	}
}

// Set replaces the key map for a single plugin. UI submit writes here.
// The map is owned by the resolver after this call; callers must not
// mutate it. Passing a nil value clears the plugin's keys, equivalent
// to Clear(name).
func (r *consoleKeyResolver) Set(plugin string, kv map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.keys == nil {
		r.keys = make(map[string]map[string]string)
	}
	if len(kv) == 0 {
		delete(r.keys, plugin)
		return
	}
	c := make(map[string]string, len(kv))
	for k, v := range kv {
		if v != "" {
			c[k] = v
		}
	}
	if len(c) == 0 {
		delete(r.keys, plugin)
		return
	}
	r.keys[plugin] = c
}

// Get returns a snapshot of the configured keys for a plugin. Used by
// the GET REST endpoint and by tests; values are returned as-is so
// the caller must treat them as sensitive.
func (r *consoleKeyResolver) Get(plugin string) (map[string]string, bool) {
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

// Clear removes all stored keys for a plugin.
func (r *consoleKeyResolver) Clear(plugin string) {
	r.Set(plugin, nil)
}

// Has reports whether a plugin has any keys configured. Used by the
// REST endpoint to decide whether to show the modal in "saved" state.
func (r *consoleKeyResolver) Has(plugin string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src, ok := r.keys[plugin]
	return ok && len(src) > 0
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
			"auth.secretRef is empty: paste the keys in the Plugin Keys panel")
	}
	keys := splitKeyRef(auth.KeyRef)
	if len(keys) == 0 {
		return external.Credentials{}, fmt.Errorf(
			"auth.keyRef is empty for plugin %q: paste the keys in the Plugin Keys panel (they are stored in process, not in any secret back-end)",
			auth.SecretRef)
	}

	r.mu.RLock()
	stored, ok := r.keys[auth.SecretRef]
	r.mu.RUnlock()
	if !ok || len(stored) == 0 {
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
