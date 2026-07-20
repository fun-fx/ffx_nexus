// Package gateway: dynamic model catalog backed by an in-memory slice that a
// background worker refreshes from upstream provider APIs. The dynamic
// provider advertises only the models it has under Models() (so /v1/models is
// always authoritative); the rest of the provider interface (ChatCompletion,
// etc.) is delegated to a backing adapter that can perform the real API calls.
//
// Why a separate struct instead of mutating the existing OpenAI / Gemini /
// Anthropic adapters:
//
//   - Hard-coded model slices in those adapters are the source of truth for
//     pre-defined capabilities (RPM budgets, guardrails policy, pricing) and
//     must stay deterministic for tests.
//   - A dynamic override layer sits beside them: when NEXUS_AUTO_UPDATE=true,
//     a background ticker refreshes the live list and the Registry's
//     by-model index so /v1/models stays up to date without a redeploy.
//   - Keeping the dynamic provider in its own type means the existing
//     adapters get a regression-free guarantee that their advertised model
//     id sets still match their test fixtures.
package gateway

import (
	"context"
	"sync"
)

// DynamicProvider advertises a mutable slice of model ids. The slice stays
// safe for concurrent reads via RWMutex so the hot path (registry.AllModels,
// handler.Models) takes only a read lock. Writers (the background sync
// worker) take the write lock briefly to swap the slice; no allocation under
// the lock happens for readers (we hand out a copy of the slice).
//
// DynamicProvider is intentionally NOT a full Provider implementation: it
// only owns Models(). It is meant to overlay (not replace) the existing
// embedded adapter via Registry.UpdateModels, which rewrites only the
// by-model index while the real ChatCompletion flow continues to use the
// statically registered adapter (which carries the actual API key / base
// URL / timeout).
type DynamicProvider struct {
	name string

	// mu protects models. Hot path readers MUST take the RLock and MUST
	// copy the slice they read so a later Set does not mutate the slice
	// they returned.
	mu     sync.RWMutex
	models []string
}

// NewDynamicProvider creates an empty dynamic provider registered under name.
// Initial models can be supplied via Set (typically called once at startup
// before the worker begins, so the registry has at least the static fallback
// visible during the very first tick).
func NewDynamicProvider(name string) *DynamicProvider {
	return &DynamicProvider{name: name, models: nil}
}

// Name implements gateway.Provider for type-conformance with Register.
// Returns the provider identifier.
func (d *DynamicProvider) Name() string { return d.name }

// Models returns a copy of the current model slice; safe for concurrent use.
// Callers can range over the result without holding any lock.
func (d *DynamicProvider) Models() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if len(d.models) == 0 {
		return nil
	}
	out := make([]string, len(d.models))
	copy(out, d.models)
	return out
}

// Set atomically replaces the advertised model slice. Returns the previous
// slice for callers that want to log a diff (typically the sync worker).
// An empty models argument clears the catalog (callers should pass nil
// instead, keeping an empty slice indistinguishable from "not yet
// fetched" so the registry index stays unchanged).
func (d *DynamicProvider) Set(models []string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	prev := d.models
	d.models = models
	if len(prev) == 0 {
		return nil
	}
	out := make([]string, len(prev))
	copy(out, prev)
	return out
}

// SnapshotLen returns the current count without copying. Useful for cheap
// log/metric lines ("refreshed 42 models for openai") on the worker side.
func (d *DynamicProvider) SnapshotLen() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.models)
}

// ensure context import is referenced when a future variant wants to
// arrange CancelFunc plumbing in the worker — currently no-op but keeps
// the import stable across edits and avoids drive-by goimports churn.
var _ = context.Background
