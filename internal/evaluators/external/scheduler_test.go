package external

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/observability"
)

// countingDispatchFn satisfies the Scheduler's dispatchFn shape and
// records how many traces passed through. Used to assert that the
// per-plugin buffer and the manual-fire path wire through Dispatch
// exactly the number of times expected.
func countingDispatchFn(counter *int32) dispatchFn {
	return func(_ context.Context, _ observability.Trace, _ *evalplugin.Plugin) error {
		atomic.AddInt32(counter, 1)
		return nil
	}
}

// TestSchedulerFlushesOnTick: a registered scheduled plugin drains
// its per-plugin buffer on the plugin's interval. Without this fix
// `scheduled` was respected only by the YAML validator, never by the
// runtime — every trace went out at on_trace speed.
func TestSchedulerFlushesOnTick(t *testing.T) {
	reg := newTestRegistry(t, scheduledPlugin("langfuse-batcher", 100*time.Millisecond))
	var count int32
	sched := NewScheduler(countingDispatchFn(&count), SchedulerConfig{
		MaxBufferPerPlugin: 8, SweepInterval: 0,
	})
	sched.Start(context.Background(), reg)
	defer sched.Stop()

	plugin := mustPluginByName(t, reg, "langfuse-batcher")
	if err := sched.Enqueue(plugin, observability.Trace{TraceID: "trace-1"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Two ticks at 100ms — first to fire, second to confirm we
	// don't double-send on a missed drain.
	time.Sleep(250 * time.Millisecond)
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Errorf("expected exactly 1 dispatch after one tick; got %d", got)
	}
}

// TestSchedulerEnforcesBufferCap protects the bounded queue
// contract. ErrBufferFull must come back without blocking so a
// vendor outage doesn't stall the eval worker.
func TestSchedulerEnforcesBufferCap(t *testing.T) {
	reg := newTestRegistry(t, scheduledPlugin("overflow", time.Hour))
	var count int32
	sched := NewScheduler(countingDispatchFn(&count), SchedulerConfig{MaxBufferPerPlugin: 3})
	sched.Start(context.Background(), reg)
	defer sched.Stop()

	p := mustPluginByName(t, reg, "overflow")
	for i := 0; i < 3; i++ {
		if err := sched.Enqueue(p, observability.Trace{TraceID: "ok"}); err != nil {
			t.Fatalf("enqueue %d should succeed: %v", i, err)
		}
	}
	err := sched.Enqueue(p, observability.Trace{TraceID: "drop"})
	if !errors.Is(err, ErrBufferFull) {
		t.Fatalf("4th enqueue should overflow; got %v", err)
	}
}

// TestSchedulerManualFireOnEmptyPlugin returns (0, nil) so the
// admin REST renders "no traces collected yet" rather than 5xx'ing.
func TestSchedulerManualFireOnEmptyPlugin(t *testing.T) {
	reg := newTestRegistry(t, manualPlugin("manual-only"))
	var count int32
	sched := NewScheduler(countingDispatchFn(&count), SchedulerConfig{MaxBufferPerPlugin: 8})
	sched.Start(context.Background(), reg)
	defer sched.Stop()

	p := mustPluginByName(t, reg, "manual-only")
	countReturned, err := sched.FireManual(context.Background(), p, "test")
	if err != nil {
		t.Fatalf("FireManual on idle plugin should not error: %v", err)
	}
	if countReturned != 0 {
		t.Errorf("manual fire on idle plugin should return 0; got %d", countReturned)
	}
}

// TestSchedulerFireManualRejectsNonManual guards against accidental
// double-sends: firing an on_trace plugin through the manual REST
// would duplicate every trace.
func TestSchedulerFireManualRejectsNonManual(t *testing.T) {
	reg := newTestRegistry(t, onTracePlugin("on-trace"))
	var count int32
	sched := NewScheduler(countingDispatchFn(&count), SchedulerConfig{MaxBufferPerPlugin: 4})
	sched.Start(context.Background(), reg)
	defer sched.Stop()

	p := mustPluginByName(t, reg, "on-trace")
	_, err := sched.FireManual(context.Background(), p, "test")
	if err == nil {
		t.Fatal("FireManual on a non-manual plugin must surface an error")
	}
}

// TestSchedulerStopJoinsGoroutines asserts Stop is synchronous: a
// pod restart must not leave a flush goroutine referencing captured
// state from a previous run.
func TestSchedulerStopJoinsGoroutines(t *testing.T) {
	reg := newTestRegistry(t, scheduledPlugin("stop-test", 50*time.Millisecond))
	var count int32
	sched := NewScheduler(countingDispatchFn(&count), SchedulerConfig{MaxBufferPerPlugin: 4})
	sched.Start(context.Background(), reg)
	stopDone := make(chan struct{})
	go func() {
		sched.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return within 2s of cancellation")
	}
}

// TestSchedulerFireScheduledRejectsNonScheduled: FireScheduled must
// refuse anything that isn't actually a scheduled-trigger plugin.
// This is the mirror of FireManual's guard — without it, an operator
// could press the scheduled-flush button on an on_trace plugin and
// silently see "drained 0 traces" with no indication that their
// input was wrong. Returning ErrNotScheduled keeps the typed error
// path consistent with FireManual.
func TestSchedulerFireScheduledRejectsNonScheduled(t *testing.T) {
	var counter int32
	sched := NewScheduler(countingDispatchFn(&counter), SchedulerConfig{})
	defer sched.Stop()

	// on_trace plugin: buffered traces go out inline, never enqueued.
	reg := newTestRegistry(t, onTracePlugin("inline-only"))
	p := mustPluginByName(t, reg, "inline-only")

	_, err := sched.FireScheduled(context.Background(), p, "interval-bypass")
	if !errors.Is(err, ErrNotScheduled) {
		t.Fatalf("err = %v; want ErrNotScheduled", err)
	}
	if got := atomic.LoadInt32(&counter); got != 0 {
		t.Errorf("dispatched %d traces; want 0 — guard failed", got)
	}
}

// TestSchedulerFireScheduledDrainsBuffer: with a scheduled plugin
// that already has buffered traces (likely because of a recent
// dispatch failure holding them up), FireScheduled must drain the
// buffer through dispatch synchronously — without waiting for the
// next tick on `spec.collect.interval`. This is the operational
// escape hatch.
func TestSchedulerFireScheduledDrainsBuffer(t *testing.T) {
	var counter int32
	sched := NewScheduler(countingDispatchFn(&counter), SchedulerConfig{})
	defer sched.Stop()

	// Push a couple of traces directly into the buffer for the
	// plugin so we don't have to drive the scheduled flush goroutine
	// just to fill it. The same `Enqueue` API the dispatcher uses
	// on the hot path.
	reg := newTestRegistry(t, scheduledPlugin("batch-me", time.Hour))
	p := mustPluginByName(t, reg, "batch-me")
	for i := 0; i < 3; i++ {
		if err := sched.Enqueue(p, observability.Trace{TraceID: "t-" + string(rune('a'+i))}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	count, err := sched.FireScheduled(context.Background(), p, "interval-bypass")
	if err != nil {
		t.Fatalf("FireScheduled: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d; want 3", count)
	}
	if got := atomic.LoadInt32(&counter); got != 3 {
		t.Errorf("dispatched %d traces; want 3", got)
	}
	// A second fire on an empty buffer counts 0 — not an error. This
	// is the contract the admin REST endpoint relies on for idempotency.
	count, err = sched.FireScheduled(context.Background(), p, "second-call")
	if err != nil {
		t.Fatalf("second FireScheduled: %v", err)
	}
	if count != 0 {
		t.Errorf("second count = %d; want 0 on empty buffer", count)
	}
}

// TestSchedulerFireScheduledOnNilPlugin: the contract is the same as
// FireManual: nil plugin is a no-op (returns 0, nil), so a typo'd
// plugin name in the admin REST endpoint renders as "0 traces
// drained" rather than a 5xx. Operators have done this by accident
// often enough that we keep the policy even across plumbing changes.
func TestSchedulerFireScheduledOnNilPlugin(t *testing.T) {
	sched := NewScheduler(countingDispatchFn(new(int32)), SchedulerConfig{})
	defer sched.Stop()
	count, err := sched.FireScheduled(context.Background(), nil, "noop")
	if err != nil {
		t.Fatalf("err = %v; want nil on nil plugin", err)
	}
	if count != 0 {
		t.Errorf("count = %d; want 0", count)
	}
}

func scheduledPlugin(name string, ivl time.Duration) string {
	return `apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: ` + name + `
spec:
  service:
    type: langfuse
    endpoint: http://localhost:9999/never
    auth:
      keyRef: k1|k2
  send:
    trigger: scheduled
    sampling: 1
  collect:
    mode: webhook
    interval: ` + ivl.String() + `
  timeout: 30s
`
}

func manualPlugin(name string) string {
	return `apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: ` + name + `
spec:
  service:
    type: langfuse
    endpoint: http://localhost:9999/never
    auth:
      keyRef: k1|k2
  send:
    trigger: manual
    sampling: 1
  collect:
    mode: webhook
    interval: 60s
  timeout: 30s
`
}

func onTracePlugin(name string) string {
	return `apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: ` + name + `
spec:
  service:
    type: langfuse
    endpoint: http://localhost:9999/never
    auth:
      keyRef: k1|k2
  send:
    trigger: on_trace
    sampling: 1
  collect:
    mode: webhook
    interval: 60s
  timeout: 30s
`
}

func newTestRegistry(t *testing.T, manifest string) *evalplugin.Registry {
	t.Helper()
	reg := evalplugin.NewRegistry()
	p, err := evalplugin.Decode([]byte(manifest))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	rec := evalplugin.Record{
		Source:  evalplugin.Source{Kind: evalplugin.SourceHelm},
		Plugin:  p,
		Enabled: true,
	}
	if discarded := reg.Merge([]evalplugin.Record{rec}); len(discarded) > 0 {
		t.Fatalf("merge discarded %d records; need unique names", len(discarded))
	}
	return reg
}

func mustPluginByName(t *testing.T, reg *evalplugin.Registry, name string) *evalplugin.Plugin {
	t.Helper()
	for _, rec := range reg.Enabled() {
		if rec.Plugin.Metadata.Name == name {
			return rec.Plugin
		}
	}
	t.Fatalf("plugin %q not found in registry", name)
	return nil
}
