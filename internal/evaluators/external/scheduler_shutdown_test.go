package external

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/observability"
)

// Stop() releases s.mu before it blocks on wg.Wait(), so every code path
// that reaches the scheduler after Stop has cleared the maps runs against
// nil `s.buffers` / `s.schedules`. Writing to a nil map is a panic, not an
// error, so a trace arriving during pod shutdown used to take down the whole
// gateway process rather than being dropped.
//
// The tests below pin the post-Stop contract deterministically: they call
// Stop first and then drive each entry point, instead of trying to win the
// cancel-vs-ticker race that surfaced this in CI.

// TestSchedulerReconcileAfterStopDoesNotPanic covers the exact CI stack:
// the sweep goroutine's select can pick `case <-ticker.C` even though ctx
// was just cancelled, so reconcile lands after Stop nils the maps and
// bufferFor panics on `s.buffers[name] = buf`.
func TestSchedulerReconcileAfterStopDoesNotPanic(t *testing.T) {
	reg := newTestRegistry(t, scheduledPlugin("late-sweep", 50*time.Millisecond))
	sched := NewScheduler(countingDispatchFn(new(int32)), SchedulerConfig{
		MaxBufferPerPlugin: 4, SweepInterval: 10 * time.Millisecond,
	})
	sched.Start(context.Background(), reg)
	sched.Stop()

	// Simulates the in-flight sweep tick that already committed to the
	// ticker branch when Stop ran.
	sched.reconcile(reg)

	if got := sched.DroppedReports(); len(got) != 0 {
		t.Errorf("DroppedReports after Stop = %v; want empty", got)
	}
}

// TestSchedulerEnqueueAfterStopReturnsError is the production-facing half of
// the same bug: eval workers drain concurrently with shutdown, so Enqueue
// races Stop. It must report that the scheduler is closed rather than
// panicking the process.
func TestSchedulerEnqueueAfterStopReturnsError(t *testing.T) {
	reg := newTestRegistry(t, scheduledPlugin("late-enqueue", time.Hour))
	sched := NewScheduler(countingDispatchFn(new(int32)), SchedulerConfig{MaxBufferPerPlugin: 4})
	sched.Start(context.Background(), reg)
	p := mustPluginByName(t, reg, "late-enqueue")

	// Enqueue works while running.
	if err := sched.Enqueue(p, observability.Trace{TraceID: "before-stop"}); err != nil {
		t.Fatalf("Enqueue before Stop: %v", err)
	}

	sched.Stop()

	err := sched.Enqueue(p, observability.Trace{TraceID: "after-stop"})
	if !errors.Is(err, ErrSchedulerStopped) {
		t.Fatalf("Enqueue after Stop = %v; want ErrSchedulerStopped", err)
	}
}

// TestSchedulerFireAfterStopReturnsError pins the admin-REST paths. An
// operator pressing "run now" while the pod drains should see a clean
// error, and the handler should not have to recover from a panic.
func TestSchedulerFireAfterStopReturnsError(t *testing.T) {
	reg := newTestRegistry(t, manualPlugin("manual-late"))
	sched := NewScheduler(countingDispatchFn(new(int32)), SchedulerConfig{MaxBufferPerPlugin: 4})
	sched.Start(context.Background(), reg)
	manual := mustPluginByName(t, reg, "manual-late")

	schedReg := newTestRegistry(t, scheduledPlugin("scheduled-late", time.Hour))
	scheduled := mustPluginByName(t, schedReg, "scheduled-late")

	sched.Stop()

	if _, err := sched.FireManual(context.Background(), manual, "test"); !errors.Is(err, ErrSchedulerStopped) {
		t.Errorf("FireManual after Stop = %v; want ErrSchedulerStopped", err)
	}
	if _, err := sched.FireScheduled(context.Background(), scheduled, "test"); !errors.Is(err, ErrSchedulerStopped) {
		t.Errorf("FireScheduled after Stop = %v; want ErrSchedulerStopped", err)
	}
}

// TestSchedulerStartAfterStopStaysStopped enforces the documented contract
// ("After Stop the scheduler cannot be restarted"). Stop clears s.cancel, so
// without an explicit guard a second Start would spawn a sweep goroutine
// against nil maps and panic on its first tick.
func TestSchedulerStartAfterStopStaysStopped(t *testing.T) {
	reg := newTestRegistry(t, scheduledPlugin("restart-guard", 10*time.Millisecond))
	sched := NewScheduler(countingDispatchFn(new(int32)), SchedulerConfig{
		MaxBufferPerPlugin: 4, SweepInterval: 10 * time.Millisecond,
	})
	sched.Start(context.Background(), reg)
	sched.Stop()

	sched.Start(context.Background(), reg)
	// Give a restarted sweep goroutine several ticks to panic if the
	// guard regressed.
	time.Sleep(60 * time.Millisecond)

	// Stop must stay idempotent too — a second call cannot double-cancel.
	sched.Stop()
}

// TestSchedulerConcurrentStopAndEnqueue drives the real race: many
// concurrent Enqueue callers while Stop runs. Under -race this is the shape
// that failed in CI; the assertion is simply "no panic, and every call
// returns either nil or ErrSchedulerStopped".
func TestSchedulerConcurrentStopAndEnqueue(t *testing.T) {
	reg := newTestRegistry(t, scheduledPlugin("racer", 5*time.Millisecond))
	sched := NewScheduler(countingDispatchFn(new(int32)), SchedulerConfig{
		MaxBufferPerPlugin: 1024, SweepInterval: time.Millisecond,
	})
	sched.Start(context.Background(), reg)
	p := mustPluginByName(t, reg, "racer")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				err := sched.Enqueue(p, observability.Trace{TraceID: "t"})
				if err != nil && !errors.Is(err, ErrSchedulerStopped) && !errors.Is(err, ErrBufferFull) {
					t.Errorf("unexpected Enqueue error: %v", err)
					return
				}
			}
		}()
	}
	go sched.Stop()
	wg.Wait()
}
