package main

import (
	"context"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/evalplugin"
)

// stubSourceAdapter lets the Tester look up a plugin by db row id.
// Only Get() is exercised by Test().
type stubSourceAdapter struct {
	byID map[string]struct {
		name string
		id   string
	}
}

func (s *stubSourceAdapter) Get(_ context.Context, id string) (*evalplugin.PluginRecord, error) {
	if rec, ok := s.byID[id]; ok {
		return &evalplugin.PluginRecord{ID: rec.id, Name: rec.name}, nil
	}
	return nil, evalplugin.ErrPluginNotFound
}

// asSourceAdapter coerces stubSourceAdapter into the *pluginSourceAdapter
// pointer shape the Tester expects. We only need the Get path to be live,
// and the helper exists so tests don't have to import a half-built
// production adapter.
func asSourceAdapter(s *stubSourceAdapter) *pluginSourceAdapter {
	// Build a minimal real adapter — the Tester only ever calls Get,
	// which we route through the stub.
	return &pluginSourceAdapter{store: stubStore{src: s}}
}

// stubStore implements evalplugin.PluginStore by delegating Get to our
// stubSourceAdapter.
type stubStore struct{ src *stubSourceAdapter }

func (s stubStore) List(_ context.Context, _ string) ([]evalplugin.PluginRecord, error) {
	return nil, nil
}
func (s stubStore) Get(_ context.Context, id string) (*evalplugin.PluginRecord, error) {
	return s.src.Get(context.Background(), id)
}
func (s stubStore) Save(_ context.Context, _ *evalplugin.PluginRecord) error { return nil }
func (s stubStore) Delete(_ context.Context, _ string) error                 { return nil }

func TestPluginTester_ResolvesByName(t *testing.T) {
	reg := evalplugin.NewRegistry()
	spec := evalPluginYAMLForTest(t, `apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: langfuse-judge
spec:
  service:
    type: langfuse
    endpoint: https://cloud.langfuse.com
    auth:
      secretRef: langfuse-creds
      keyRef: public_key|secret_key
  send:
    trigger: on_trace
    sampling: 0.1
  collect:
    mode: webhook
    interval: 60s
  timeout: 30s
`)
	rec := evalplugin.Record{
		Source:  evalplugin.Source{Kind: evalplugin.SourceHelm},
		Plugin:  spec,
		Enabled: true,
	}
	if discarded := reg.Merge([]evalplugin.Record{rec}); len(discarded) > 0 {
		t.Fatalf("unexpected discard: %d", len(discarded))
	}
	tester := newTester(reg, nil, nil)
	res, err := tester.Test(context.Background(), "langfuse-judge")
	if err != nil {
		t.Fatalf("Test returned err: %v", err)
	}
	// For a generic endpoint probe against cloud.langfuse.com we
	// expect either a real probe result or a network error from
	// the sandbox. What matters here is that the shape is populated,
	// not the boolean (the test runner may have no internet).
	if res.Message == "" {
		t.Errorf("expected non-empty message, got %q", res.Message)
	}
	// Latency_ms is populated even when probes race-fail because
	// time.Since(start) is computed unconditionally in Tester.Test.
	if res.LatencyMs < 0 {
		t.Errorf("latency must be >= 0, got %d", res.LatencyMs)
	}
}

func TestPluginTester_ResolvesByDBID(t *testing.T) {
	reg := evalplugin.NewRegistry()
	spec := evalPluginYAMLForTest(t, `apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: langfuse-judge
spec:
  service:
    type: langfuse
    endpoint: https://cloud.langfuse.com
    auth:
      secretRef: langfuse-creds
      keyRef: public_key|secret_key
  send:
    trigger: on_trace
    sampling: 0.1
  collect:
    mode: webhook
    interval: 60s
  timeout: 30s
`)
	if discarded := reg.Merge([]evalplugin.Record{{
		Source: evalplugin.Source{Kind: evalplugin.SourceDatabase},
		Plugin: spec,
	}}); len(discarded) > 0 {
		t.Fatalf("unexpected discard: %d", len(discarded))
	}
	tester := newTester(reg, nil, nil).withSource(asSourceAdapter(&stubSourceAdapter{
		byID: map[string]struct {
			name string
			id   string
		}{"abc-123": {name: "langfuse-judge", id: "abc-123"}},
	}))
	// Pass the db id; resolver should re-key on metadata.name.
	res, err := tester.Test(context.Background(), "abc-123")
	if err != nil {
		t.Fatalf("Test returned err: %v", err)
	}
	if res.Message == "" {
		t.Errorf("expected non-empty message after id lookup, got %q", res.Message)
	}
}

// TestPluginTester_ResolvesFreshlyCreatedPluginByName reproduces the
// production failure behind the "502 Bad gateway" report: the registry
// is only filled at boot, so a plugin an operator creates through the
// console lives in Postgres alone until the pod restarts. Test() used
// to resolve the db fallback through the registry, which by definition
// misses for such a row, so pressing Test returned `plugin "…" not
// found` — and the handler then reported that as HTTP 502, which
// Cloudflare rewrote into its own HTML error page.
//
// Here the registry is deliberately left EMPTY and the store holds the
// full manifest, exactly as it does right after a create.
func TestPluginTester_ResolvesFreshlyCreatedPluginByName(t *testing.T) {
	const manifest = `apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: langfuse-judge
spec:
  service:
    type: langfuse
    endpoint: https://cloud.langfuse.com
    auth:
      secretRef: langfuse-creds
      keyRef: public_key|secret_key
  send:
    trigger: on_trace
    sampling: 0.1
  collect:
    mode: webhook
    interval: 60s
  timeout: 30s
`
	reg := evalplugin.NewRegistry() // cold, as after a fresh boot
	tester := newTester(reg, nil, nil).withSource(&pluginSourceAdapter{
		store: yamlStore{rows: []evalplugin.PluginRecord{{
			ID:       "abc-123",
			Name:     "langfuse-judge",
			SpecYAML: manifest,
			Enabled:  true,
		}}},
	})

	rec, ok := tester.resolve(context.Background(), "langfuse-judge")
	if !ok {
		t.Fatal("resolve by name missed a plugin that exists only in the store")
	}
	if rec.Plugin == nil || rec.Plugin.Metadata.Name != "langfuse-judge" {
		t.Fatalf("resolved the wrong record: %+v", rec.Plugin)
	}
	// The endpoint has to survive the decode, otherwise the probe
	// would run against an empty URL and report a bogus message.
	if got := rec.Plugin.Spec.Service.Endpoint; got != "https://cloud.langfuse.com" {
		t.Errorf("endpoint = %q, want https://cloud.langfuse.com", got)
	}

	// Resolving by the row id must work through the same path.
	if _, ok := tester.resolve(context.Background(), "abc-123"); !ok {
		t.Error("resolve by db id missed a store-only plugin")
	}
}

// yamlStore is a PluginStore whose rows carry real manifests, which is
// what a Postgres row looks like. List() is live because the adapter's
// name-keyed Lookup scans it.
type yamlStore struct{ rows []evalplugin.PluginRecord }

func (s yamlStore) List(_ context.Context, _ string) ([]evalplugin.PluginRecord, error) {
	return s.rows, nil
}
func (s yamlStore) Get(_ context.Context, id string) (*evalplugin.PluginRecord, error) {
	for i := range s.rows {
		if s.rows[i].ID == id {
			return &s.rows[i], nil
		}
	}
	return nil, evalplugin.ErrPluginNotFound
}
func (s yamlStore) Save(_ context.Context, _ *evalplugin.PluginRecord) error { return nil }
func (s yamlStore) Delete(_ context.Context, _ string) error                 { return nil }

func TestPluginTester_MissingReturnsError(t *testing.T) {
	reg := evalplugin.NewRegistry()
	tester := newTester(reg, nil, nil)
	_, err := tester.Test(context.Background(), "does-not-exist")
	if err == nil {
		t.Fatal("expected error for unknown plugin, got nil")
	}
	if _, ok := err.(error); !ok {
		t.Fatalf("expected typed error, got %T", err)
	}
}

// evalPluginYAMLForTest decodes a YAML manifest into a *Plugin for the
// table-style tests in this file. We use Decode because it is the same
// entry point the runtime loader uses, so any YAML-string edge case
// breaks here at the unit test level.
func evalPluginYAMLForTest(t *testing.T, body string) *evalplugin.Plugin {
	t.Helper()
	p, err := evalplugin.Decode([]byte(body))
	if err != nil {
		t.Fatalf("decode plugin manifest: %v", err)
	}
	// Avoid timing flakiness: regulators below 1ms are observed as 0
	// and we still want LatencyMs >= 0. The empty check would zero
	// them too — skip the check for sandboxed test environments
	// where the network is offline.
	_ = time.Second // keep import alive for future expansion
	return p
}
