package cron

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestWaitForLeadershipFollowerKeepsRetrying locks the gate so
// every Acquire returns (nil, nil). The expected outcome is a
// timeout (the test cancels ctx before any leader hand-off).
func TestWaitForLeadershipFollowerKeepsRetrying(t *testing.T) {
	gate := &immediateFollowerGate{}
	ctx, cancelFn := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelFn()
	leaseCtx, err := WaitForLeadership(ctx, gate, "benchmark_scheduler")
	if err == nil {
		t.Fatalf("expected timeout/ctx error, got nil (leaseCtx=%v)", leaseCtx)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}

// TestWaitForLeadershipLeaderReturnsContext ensures the happy
// path returns a context promptly and does not block.
func TestWaitForLeadershipLeaderReturnsContext(t *testing.T) {
	gate := &immediateLeaderGate{}
	ctx, cancelFn := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelFn()
	leaseCtx, err := WaitForLeadership(ctx, gate, "benchmark_scheduler")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if leaseCtx == nil {
		t.Fatal("leader path returned nil context")
	}
}

type immediateLeaderGate struct{}

func (immediateLeaderGate) Acquire(ctx context.Context, role string) (context.Context, error) {
	return ctx, nil
}
func (immediateLeaderGate) Release(ctx context.Context, role string) error { return nil }

type immediateFollowerGate struct{}

func (immediateFollowerGate) Acquire(ctx context.Context, role string) (context.Context, error) {
	return nil, nil
}
func (immediateFollowerGate) Release(ctx context.Context, role string) error { return nil }

// TestNoopLeaderDoesNotBlock covers the convenience gate
// exposed by WaitForLeadership so cron.Runner tests that don't
// need a real Postgres pool can opt out cleanly.
func TestNoopLeaderDoesNotBlock(t *testing.T) {
	ctx, cancelFn := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelFn()
	leaseCtx, err := WaitForLeadership(ctx, NoopLeader{}, "benchmark_scheduler")
	if err != nil {
		t.Fatalf("noop gate errored: %v", err)
	}
	if leaseCtx == nil {
		t.Fatal("noop gate returned nil ctx")
	}
}
