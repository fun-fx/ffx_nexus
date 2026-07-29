package main

import (
	"context"
	"testing"

	"github.com/ffxnexus/nexus/internal/evalplugin"
)

const adapterManifest = `apiVersion: nexus.io/v1alpha1
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

// TestAdapterSaveMakesPluginLiveImmediately covers the gap that made a
// console-created plugin inert: rows were written to Postgres but never
// merged into the registry, which the dispatcher reads. Until the pod
// restarted, no trace was ever forwarded.
func TestAdapterSaveMakesPluginLiveImmediately(t *testing.T) {
	reg := evalplugin.NewRegistry()
	a := pluginSourceAdapter{reg: reg, store: evalplugin.NewMemoryStore(nil)}

	rec := &evalplugin.PluginRecord{
		OrgID:    "acme",
		Name:     "langfuse-judge",
		SpecYAML: adapterManifest,
		Enabled:  true,
	}
	if err := a.Save(context.Background(), rec); err != nil {
		t.Fatalf("Save: %v", err)
	}

	live := reg.EnabledForOrg("acme")
	if len(live) != 1 || live[0].Plugin.Metadata.Name != "langfuse-judge" {
		t.Fatalf("plugin not dispatchable right after Save: %+v", live)
	}
	if live[0].OrgID != "acme" {
		t.Errorf("OrgID = %q, want acme — without it the entry leaks to other tenants", live[0].OrgID)
	}
	// A different tenant must not inherit a per-org row.
	if got := reg.EnabledForOrg("globex"); len(got) != 0 {
		t.Errorf("globex sees acme's plugin: %+v", got)
	}
}

// TestAdapterSaveAppliesDisable is the toggle path: patching enabled to
// false has to reach the registry, or the dispatcher keeps sending.
func TestAdapterSaveAppliesDisable(t *testing.T) {
	reg := evalplugin.NewRegistry()
	a := pluginSourceAdapter{reg: reg, store: evalplugin.NewMemoryStore(nil)}
	rec := &evalplugin.PluginRecord{
		OrgID: "acme", Name: "langfuse-judge",
		SpecYAML: adapterManifest, Enabled: true,
	}
	if err := a.Save(context.Background(), rec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	rec.Enabled = false
	if err := a.Save(context.Background(), rec); err != nil {
		t.Fatalf("re-Save: %v", err)
	}
	if got := reg.EnabledForOrg("acme"); len(got) != 0 {
		t.Fatalf("disabled plugin still dispatchable: %+v", got)
	}
}

// TestAdapterDeleteEvictsFromRegistry stops a deleted plugin from
// continuing to receive traces until the next restart.
func TestAdapterDeleteEvictsFromRegistry(t *testing.T) {
	reg := evalplugin.NewRegistry()
	a := pluginSourceAdapter{reg: reg, store: evalplugin.NewMemoryStore(nil)}
	rec := &evalplugin.PluginRecord{
		OrgID: "acme", Name: "langfuse-judge",
		SpecYAML: adapterManifest, Enabled: true,
	}
	if err := a.Save(context.Background(), rec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if rec.ID == "" {
		t.Fatal("store did not assign an id")
	}
	if err := a.Delete(context.Background(), rec.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := reg.EnabledForOrg("acme"); len(got) != 0 {
		t.Fatalf("deleted plugin still dispatchable: %+v", got)
	}
}
