package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/evalplugin"
)

// TestReconcileLoopRepairsRegistryDrift is the end-to-end shape of the
// backstop: a row exists and is enabled, the live registry does not have
// it, and no admin write is coming. Within one interval the plugin has to
// start dispatching on its own.
func TestReconcileLoopRepairsRegistryDrift(t *testing.T) {
	store := evalplugin.NewMemoryStore(nil)
	if err := store.Save(context.Background(), &evalplugin.PluginRecord{
		OrgID: "", Name: "langfuse-judge",
		SpecYAML: adapterManifest, Enabled: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	reg := evalplugin.NewRegistry()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runPluginRegistryReconcile(ctx, reg, store, 5*time.Millisecond, log)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(reg.EnabledForOrg("default")) == 1 {
			if !strings.Contains(buf.String(), "reconciled from database") {
				t.Fatal("drift repair must be reported in the log")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("registry was never repaired")
}

// TestReconcileLoopStopsWithContext keeps a shutdown from leaking a
// ticker goroutine that queries a closing pool.
func TestReconcileLoopStopsWithContext(t *testing.T) {
	store := evalplugin.NewMemoryStore(nil)
	reg := evalplugin.NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runPluginRegistryReconcile(ctx, reg, store, time.Millisecond, nil)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile loop ignored context cancellation")
	}
}

// TestReconcileLoopIgnoresMissingDependencies: deployments without
// Postgres pass a nil store, and the loop must return instead of ticking
// forever against nothing.
func TestReconcileLoopIgnoresMissingDependencies(t *testing.T) {
	done := make(chan struct{})
	go func() {
		runPluginRegistryReconcile(context.Background(),
			evalplugin.NewRegistry(), nil, time.Minute, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop should return immediately without a store")
	}
}

// TestAdapterSaveLogsLiveState: a plugin that is stored but not live has
// no symptom other than an empty vendor dashboard, so the save path has
// to say which of the two it produced.
func TestAdapterSaveLogsLiveState(t *testing.T) {
	var buf bytes.Buffer
	a := pluginSourceAdapter{
		reg:   evalplugin.NewRegistry(),
		store: evalplugin.NewMemoryStore(nil),
		log:   slog.New(slog.NewTextHandler(&buf, nil)),
	}
	err := a.Save(context.Background(), &evalplugin.PluginRecord{
		OrgID: "", Name: "langfuse-judge",
		SpecYAML: adapterManifest, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "eval plugin live") {
		t.Fatalf("expected a live confirmation, got: %s", out)
	}
	if !strings.Contains(out, "(cluster-wide)") {
		t.Fatalf("expected the cluster-wide scope to be named, got: %s", out)
	}
}

// TestAdapterSaveWarnsWhenRegistryRejectsManifest: the merge used to
// return silently when the stored manifest failed strict decode, leaving
// an enabled row that never dispatched.
func TestAdapterSaveWarnsWhenRegistryRejectsManifest(t *testing.T) {
	var buf bytes.Buffer
	reg := evalplugin.NewRegistry()
	a := pluginSourceAdapter{
		reg:   reg,
		store: strictRejectStore{},
		log:   slog.New(slog.NewTextHandler(&buf, nil)),
	}
	rec := &evalplugin.PluginRecord{
		OrgID: "", Name: "langfuse-judge",
		SpecYAML: "not: a: manifest", Enabled: true,
	}
	if err := a.Save(context.Background(), rec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "failed strict decode") {
		t.Fatalf("expected a decode warning, got: %s", out)
	}
	if !strings.Contains(out, "stored but not live") {
		t.Fatalf("expected a not-live warning, got: %s", out)
	}
}

// strictRejectStore accepts any row so the test can drive the registry
// merge with a manifest the real store's re-validation would refuse.
type strictRejectStore struct{}

func (strictRejectStore) List(context.Context, string) ([]evalplugin.PluginRecord, error) {
	return nil, nil
}

func (strictRejectStore) Get(context.Context, string) (*evalplugin.PluginRecord, error) {
	return nil, evalplugin.ErrPluginNotFound
}

func (strictRejectStore) Save(context.Context, *evalplugin.PluginRecord) error { return nil }

func (strictRejectStore) Delete(context.Context, string) error { return nil }
