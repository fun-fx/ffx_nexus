package external

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/observability"
)

// inlineCounter is a TransmitFunc-shaped closure that increments a
// counter every time MultiEvaluator.Evaluate triggers an inline
// dispatch. We register it under evalplugin.ServiceLangfuse because
// every test plugin above uses langfuse (no vendor SDK needed).
func inlineCounter(counter *int32) func(ctx context.Context, target Target, payload map[string]any) error {
	return func(_ context.Context, _ Target, _ map[string]any) error {
		atomic.AddInt32(counter, 1)
		return nil
	}
}

// silentResolver satisfies SecretResolver by returning an empty
// Credentials; tests that exercise the dispatcher's auth path need
// at least a stub or Dispatch returns ErrNoSecretResolver.
type silentResolver struct{}

func (silentResolver) Resolve(_ context.Context, _ evalplugin.AuthSpec) (Credentials, error) {
	return Credentials{}, nil
}

// TestMultiEvaluator_TriggerOnTraceGoesInline verifies the regression
// guard: pre-fix the dispatcher ignored Send.Trigger and always acted
// as if it were `on_trace`. The on_trace branch must still send
// inline so existing manifests don't break.
func TestMultiEvaluator_TriggerOnTraceGoesInline(t *testing.T) {
	reg := newTestRegistry(t, onTracePlugin("on-trace"))
	var inline int32
	d := NewDispatcher(reg, nil)
	d.SetSecretResolver(silentResolver{})
	d.Register(evalplugin.ServiceLangfuse, TransmitFunc(inlineCounter(&inline)))
	m := NewMultiEvaluator(reg, d)

	if _, err := m.Evaluate(context.Background(), observability.Trace{TraceID: "t1"}); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got := atomic.LoadInt32(&inline); got != 1 {
		t.Errorf("on_trace should fire inline; got %d sends", got)
	}
}

// TestMultiEvaluator_TriggerScheduledRoutedToScheduler: scheduled
// traces must not fire inline — they go to the per-plugin buffer
// to be drained on the plugin's Collect.Interval.
func TestMultiEvaluator_TriggerScheduledRoutedToScheduler(t *testing.T) {
	reg := newTestRegistry(t, scheduledPlugin("sched", time.Hour))
	var inline int32
	d := NewDispatcher(reg, nil)
	d.SetSecretResolver(silentResolver{})
	d.Register(evalplugin.ServiceLangfuse, TransmitFunc(inlineCounter(&inline)))
	m := NewMultiEvaluator(reg, d)
	sched := NewScheduler(countingDispatchFn(new(int32)),
		SchedulerConfig{MaxBufferPerPlugin: 4, SweepInterval: 0})
	sched.Start(context.Background(), reg)
	defer sched.Stop()
	m.SetScheduler(sched)

	if _, err := m.Evaluate(context.Background(), observability.Trace{TraceID: "t1"}); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got := atomic.LoadInt32(&inline); got != 0 {
		t.Errorf("scheduled trigger should not fire inline; got %d sends", got)
	}
}

// TestMultiEvaluator_TriggerManualDropsInline: manual triggers
// don't enqueue inline traces — only the admin REST drives them.
func TestMultiEvaluator_TriggerManualDropsInline(t *testing.T) {
	reg := newTestRegistry(t, manualPlugin("manual-only"))
	var inline int32
	d := NewDispatcher(reg, nil)
	d.SetSecretResolver(silentResolver{})
	d.Register(evalplugin.ServiceLangfuse, TransmitFunc(inlineCounter(&inline)))
	m := NewMultiEvaluator(reg, d)
	m.SetScheduler(NewScheduler(countingDispatchFn(new(int32)),
		SchedulerConfig{MaxBufferPerPlugin: 4}))

	if _, err := m.Evaluate(context.Background(), observability.Trace{TraceID: "t1"}); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got := atomic.LoadInt32(&inline); got != 0 {
		t.Errorf("manual trigger should not fire inline; got %d sends", got)
	}
}
