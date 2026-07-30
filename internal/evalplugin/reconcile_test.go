package evalplugin

import (
	"context"
	"strings"
	"testing"
)

// reconcileManifest is a minimal valid manifest; tests vary the name and
// the endpoint to tell revisions apart.
const reconcileManifest = `apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: langfuse-judge
spec:
  service:
    type: langfuse
    endpoint: https://us.cloud.langfuse.com
    auth:
      secretRef: langfuse-judge
      keyRef: public_key|secret_key
  send:
    trigger: on_trace
    sampling: 1
  collect:
    mode: webhook
    interval: 60s
    mapping: { name: name, score: value, explanation: comment, trace_id: traceId }
  timeout: 30s
`

func seedStore(t *testing.T, rows ...*PluginRecord) *MemoryStore {
	t.Helper()
	store := NewMemoryStore(nil)
	for _, row := range rows {
		if err := store.Save(context.Background(), row); err != nil {
			t.Fatalf("seed store: %v", err)
		}
	}
	return store
}

// TestReconcileAdoptsRowTheRegistryMissed is the case that cost a long
// debugging session: the row is stored and enabled, the console lists it,
// but the live registry never got it — so the dispatcher forwarded
// nothing and said nothing. A reconcile tick has to repair that.
func TestReconcileAdoptsRowTheRegistryMissed(t *testing.T) {
	store := seedStore(t, &PluginRecord{
		OrgID: "", Name: "langfuse-judge",
		SpecYAML: reconcileManifest, Enabled: true,
	})
	reg := NewRegistry()
	if got := len(reg.EnabledForOrg("default")); got != 0 {
		t.Fatalf("precondition: registry should be empty, got %d", got)
	}

	changed, err := reg.ReconcileFromStore(context.Background(), store)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1", changed)
	}
	if got := len(reg.EnabledForOrg("default")); got != 1 {
		t.Fatalf("plugin should dispatch for a trace org after reconcile, got %d", got)
	}
}

// TestReconcileIsQuietWhenInSync keeps the operator log free of a line
// every interval; only real drift should be reported.
func TestReconcileIsQuietWhenInSync(t *testing.T) {
	store := seedStore(t, &PluginRecord{
		OrgID: "", Name: "langfuse-judge",
		SpecYAML: reconcileManifest, Enabled: true,
	})
	reg := NewRegistry()
	if _, err := reg.ReconcileFromStore(context.Background(), store); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	changed, err := reg.ReconcileFromStore(context.Background(), store)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if changed != 0 {
		t.Fatalf("changed = %d, want 0 on a steady state", changed)
	}
}

// TestReconcileEvictsDeletedRow: a plugin deleted straight from the
// database (or through a replica whose registry we don't share) must stop
// receiving traces rather than keep sending until the next restart.
func TestReconcileEvictsDeletedRow(t *testing.T) {
	row := &PluginRecord{
		OrgID: "", Name: "langfuse-judge",
		SpecYAML: reconcileManifest, Enabled: true,
	}
	store := seedStore(t, row)
	reg := NewRegistry()
	if _, err := reg.ReconcileFromStore(context.Background(), store); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := store.Delete(context.Background(), row.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	changed, err := reg.ReconcileFromStore(context.Background(), store)
	if err != nil {
		t.Fatalf("reconcile after delete: %v", err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1", changed)
	}
	if got := len(reg.All()); got != 0 {
		t.Fatalf("registry should be empty after eviction, got %d", got)
	}
}

// TestReconcilePicksUpEnabledToggle covers the admin switch: a row
// disabled elsewhere has to stop dispatching here too.
func TestReconcilePicksUpEnabledToggle(t *testing.T) {
	row := &PluginRecord{
		OrgID: "", Name: "langfuse-judge",
		SpecYAML: reconcileManifest, Enabled: true,
	}
	store := seedStore(t, row)
	reg := NewRegistry()
	if _, err := reg.ReconcileFromStore(context.Background(), store); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	row.Enabled = false
	if err := store.Save(context.Background(), row); err != nil {
		t.Fatalf("save disabled: %v", err)
	}

	changed, err := reg.ReconcileFromStore(context.Background(), store)
	if err != nil {
		t.Fatalf("reconcile after toggle: %v", err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1", changed)
	}
	if got := len(reg.Enabled()); got != 0 {
		t.Fatalf("disabled plugin should not be enabled, got %d", got)
	}
}

// TestReconcileLeavesHelmEntryAlone: the ConfigMap install is the
// operator's floor. A database row of the same name must not override it,
// and reconcile must not evict it either — it has no row to match.
func TestReconcileLeavesHelmEntryAlone(t *testing.T) {
	helmPlugin, err := Decode([]byte(strings.Replace(reconcileManifest,
		"endpoint: https://us.cloud.langfuse.com",
		"endpoint: https://helm.example.com", 1)))
	if err != nil {
		t.Fatalf("decode helm manifest: %v", err)
	}
	reg := NewRegistry()
	reg.Merge([]Record{{
		Plugin:  helmPlugin,
		Source:  Source{Kind: SourceHelm, Ref: "langfuse.yaml"},
		Enabled: true,
	}})
	store := seedStore(t, &PluginRecord{
		OrgID: "", Name: "langfuse-judge",
		SpecYAML: reconcileManifest, Enabled: true,
	})

	if _, err := reg.ReconcileFromStore(context.Background(), store); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, ok := reg.Lookup("langfuse-judge")
	if !ok {
		t.Fatal("helm entry disappeared")
	}
	if !strings.Contains(got.Plugin.Spec.Service.Endpoint, "helm.example.com") {
		t.Fatalf("helm entry was overwritten: endpoint=%s", got.Plugin.Spec.Service.Endpoint)
	}

	// And a second tick must not evict it for lacking a database row.
	if _, err := reg.ReconcileFromStore(context.Background(), store); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if _, ok := reg.Lookup("langfuse-judge"); !ok {
		t.Fatal("helm entry evicted by reconcile")
	}
}

// TestReconcileKeepsPerOrgAndClusterWideApart guards the tenancy rule: a
// per-org row and a cluster-wide row of the same name are separate
// entries, and reconciling one must not evict the other.
func TestReconcileKeepsPerOrgAndClusterWideApart(t *testing.T) {
	store := seedStore(t,
		&PluginRecord{OrgID: "", Name: "langfuse-judge",
			SpecYAML: reconcileManifest, Enabled: true},
		&PluginRecord{OrgID: "acme", Name: "langfuse-judge",
			SpecYAML: reconcileManifest, Enabled: true},
	)
	reg := NewRegistry()
	if _, err := reg.ReconcileFromStore(context.Background(), store); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := len(reg.All()); got != 2 {
		t.Fatalf("want 2 entries (cluster-wide + acme), got %d", got)
	}
	// The org row shadows the inherited one rather than doubling the send.
	if got := len(reg.EnabledForOrg("acme")); got != 1 {
		t.Fatalf("acme should see exactly one entry, got %d", got)
	}
}

// TestReconcileRejectsNilStore keeps the loop from spinning on a
// deployment without Postgres.
func TestReconcileRejectsNilStore(t *testing.T) {
	reg := NewRegistry()
	if _, err := reg.ReconcileFromStore(context.Background(), nil); err == nil {
		t.Fatal("expected an error for a nil store")
	}
}
