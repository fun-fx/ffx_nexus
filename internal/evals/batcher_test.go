package evals

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/observability"
)

func TestBatcher_FlushesOnSize(t *testing.T) {
	cfg := BatchConfig{MaxSize: 4, Window: 100 * time.Millisecond, Capacity: 2}
	b := NewBatcher(cfg)
	var batches int32
	var last TraceBatch
	var mu sync.Mutex
	b.Start(func(_ context.Context, tb TraceBatch) {
		atomic.AddInt32(&batches, 1)
		mu.Lock()
		last = tb
		mu.Unlock()
	})
	for i := 0; i < 4; i++ {
		b.Submit(observability.Trace{TraceID: "t", StatusCode: 200})
	}
	// Wait for the size-triggered flush.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&batches) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.Stop(context.Background())
	if got := atomic.LoadInt32(&batches); got != 1 {
		t.Fatalf("expected 1 batch flush, got %d", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(last.Traces) != 4 {
		t.Fatalf("expected 4 traces, got %d", len(last.Traces))
	}
}

func TestBatcher_FlushesOnWindow(t *testing.T) {
	cfg := BatchConfig{MaxSize: 100, Window: 50 * time.Millisecond, Capacity: 2}
	b := NewBatcher(cfg)
	var batches int32
	b.Start(func(_ context.Context, _ TraceBatch) {
		atomic.AddInt32(&batches, 1)
	})
	b.Submit(observability.Trace{StatusCode: 200})
	// Window is 50ms; wait at least one cycle + slack.
	time.Sleep(200 * time.Millisecond)
	b.Stop(context.Background())
	if got := atomic.LoadInt32(&batches); got < 1 {
		t.Fatalf("expected at least 1 batch flush, got %d", got)
	}
}

func TestBatcher_DropsOnBackpressure(t *testing.T) {
	cfg := BatchConfig{MaxSize: 4, Window: 100 * time.Millisecond, Capacity: 1}
	b := NewBatcher(cfg)
	// Don't start the run loop. Submit will block on the input channel
	// until capacity is full and drop with the default branch.
	b.Submit(observability.Trace{StatusCode: 200})
	b.Submit(observability.Trace{StatusCode: 200})
	// We've filled the buffer (Capacity * MaxSize). Both submissions
	// should not panic; the dispatcher log warns about drops in the
	// real worker — here we just check no crash, no deadlock.
	done := make(chan struct{})
	go func() {
		b.Submit(observability.Trace{StatusCode: 200})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("submit blocked under back-pressure")
	}
}

func TestBatcher_ProfilesSnapshotCaptured(t *testing.T) {
	cfg := BatchConfig{MaxSize: 2, Window: 100 * time.Millisecond, Capacity: 1}
	b := NewBatcher(cfg)
	var captured []EvalProfile
	var mu sync.Mutex
	b.SetProfiles([]EvalProfile{{ID: "p1", Name: "p", Kind: ProfileHeuristicPII, Scope: ScopeOrg}})
	b.Start(func(_ context.Context, tb TraceBatch) {
		mu.Lock()
		captured = append(captured, tb.Profiles...)
		mu.Unlock()
	})
	b.Submit(observability.Trace{StatusCode: 200})
	b.Submit(observability.Trace{StatusCode: 200})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		if len(captured) >= 1 {
			mu.Unlock()
			break
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	b.Stop(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("expected 1 profile in snapshot, got %d", len(captured))
	}
	if captured[0].ID != "p1" {
		t.Fatalf("profile id mismatch: %s", captured[0].ID)
	}
}
