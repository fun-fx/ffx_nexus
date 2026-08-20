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

// immediateLeaderScheduleGate / immediateFollowerScheduleGate
// are stub ScheduleGate implementations that mirror the role
// fixtures above. They let us assert the per-schedule lock
// path without bringing up Postgres.

type immediateLeaderScheduleGate struct{}

func (immediateLeaderScheduleGate) AcquireSchedule(ctx context.Context, _ string) (context.Context, error) {
	return ctx, nil
}
func (immediateLeaderScheduleGate) ReleaseSchedule(_ context.Context, _ string) error {
	return nil
}

type immediateFollowerScheduleGate struct{}

func (immediateFollowerScheduleGate) AcquireSchedule(_ context.Context, _ string) (context.Context, error) {
	return nil, nil
}
func (immediateFollowerScheduleGate) ReleaseSchedule(_ context.Context, _ string) error {
	return nil
}

// TestFireAcquiresPerScheduleLock pins that the runner
// attempts the per-schedule advisory lock around fire():
// when the gate reports "follower" the runner must not call
// the underlying Lander.
//
// The contract is exposed through a recording Lander so the
// test can count invocations.
func TestFireAcquiresPerScheduleLock(t *testing.T) {
	lander := &recordingLander{}
	r := New(stubStore{next: []Spec{{ID: "sch-x", Model: "m", Cadence: time.Minute}}},
		lander,
		nil)
	r.SetScheduleGate(immediateFollowerScheduleGate{})

	r.tick(context.Background())

	if len(lander.calls) != 0 {
		t.Errorf("follower schedule gate must not invoke lander; got %d calls", len(lander.calls))
	}
	if r.lastErrors["sch-x"] == "" {
		t.Errorf("follower path should leave a lastErrors trace so operators can grep")
	}
}

// TestFireLeaderRunsLanderOnce is the reverse-pass: under a
// leader schedule gauge the runner must invoke the lander
// exactly once per Spec, regardless of how many Specs the
// store returned.
func TestFireLeaderRunsLanderOnce(t *testing.T) {
	lander := &recordingLander{}
	r := New(stubStore{next: []Spec{
		{ID: "sch-a", Model: "m", Cadence: time.Minute},
		{ID: "sch-b", Model: "m", Cadence: time.Minute},
	}}, lander, nil)
	r.SetScheduleGate(immediateLeaderScheduleGate{})

	r.tick(context.Background())

	if len(lander.calls) != 2 {
		t.Errorf("expected exactly 2 lander invocations (one per schedule); got %d", len(lander.calls))
	}
}

type stubStore struct {
	next []Spec
}

func (s stubStore) ListDueSchedules(_ context.Context, _ time.Time, _ int) ([]Spec, error) {
	return s.next, nil
}
func (s stubStore) UpdateNextLaunchAt(_ context.Context, _ string, _ time.Time) error { return nil }
func (s stubStore) MarkLaunched(_ context.Context, _, _ string, _ time.Time) error   { return nil }
func (s stubStore) GetScheduleByID(_ context.Context, _ string) (Spec, error) {
	return Spec{}, nil
}

type recordingLander struct {
	calls []string
}

func (r *recordingLander) RunSchedule(_ context.Context, s Spec) (string, error) {
	r.calls = append(r.calls, s.ID)
	return "run-" + s.ID, nil
}
