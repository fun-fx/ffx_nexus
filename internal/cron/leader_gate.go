package cron

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ffxnexus/nexus/internal/leaser"
)

// LeaderGate decides whether the calling replica is the leader
// for a given role. Implementations return a context that is
// cancelled when the lease is lost; the runner observes Done()
// and stops scheduling until the gate is reacquired.
//
// Production wires this through internal/leaser.Manager. Tests
// can substitute NoopLeader or any hand-rolled stub.
type LeaderGate interface {
	// Acquire returns (leaseCtx, err). When err is ErrAlreadyHeld
	// the caller is a follower; the runner must skip ticks until
	// Acquire succeeds. leaseCtx is non-nil only when the caller
	// is the leader, and is cancelled when leadership is lost.
	Acquire(ctx context.Context, role string) (leaseCtx context.Context, err error)
	// Release relinquishes the lease. Called at graceful pod
	// shutdown.
	Release(ctx context.Context, role string) error
}

// Leader returns the gate that has been wired into this Runner.
// Returns nil if no gate was set; the runner preserves the
// "every replica fires" model in that case (matching the
// pre-Phase-D-1 behaviour).
func (r *Runner) Leader() LeaderGate { return r.leader }

// NoopLeader lets tests run the runner without single-leader
// gating. Replaces a real LeaderGate so we do not need a live
// Postgres for cron.Runner tests.
type NoopLeader struct{}

func (NoopLeader) Acquire(ctx context.Context, role string) (context.Context, error) {
	return ctx, nil
}
func (NoopLeader) Release(ctx context.Context, role string) error { return nil }

// ErrLeaderAlreadyHeld is re-exported here so callers can branch
// on it without importing internal/leaser directly.
var ErrLeaderAlreadyHeld = leaser.ErrAlreadyHeld

// WaitForLeadership loops Acquire until it succeeds, treating
// ErrAlreadyHeld as "another replica is the leader, retry
// shortly". The loop terminates if ctx is cancelled. When the
// returned context is done, the caller has lost leadership.
//
// The wait-loop is intentionally narrow — short enough that a
// crash and handover should converge within DefaultTTL, but long
// enough that we do not pound Postgres during handover.
func WaitForLeadership(ctx context.Context, gate LeaderGate, role string) (context.Context, error) {
	if gate == nil {
		return ctx, nil
	}
	for {
		leaseCtx, err := gate.Acquire(ctx, role)
		switch {
		case err == nil && leaseCtx != nil:
			return leaseCtx, nil
		case err == nil && leaseCtx == nil:
			// Follower. Sleep briefly and retry.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(2 * time.Second):
				continue
			}
		case errors.Is(err, ErrLeaderAlreadyHeld):
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(2 * time.Second):
				continue
			}
		default:
			return nil, err
		}
	}
}

// ScheduleGate decides whether the calling replica is the
// leader for a specific schedule. Per-schedule gating is what
// Phase D-1 specifies: each schedule has its own advisory lock
// key so two long-running schedules do not serialise on a
// global mutex.
//
// Production wires this through internal/leaser.Manager.AcquireSchedule.
type ScheduleGate interface {
	AcquireSchedule(ctx context.Context, scheduleID string) (leaseCtx context.Context, err error)
	ReleaseSchedule(ctx context.Context, scheduleID string) error
}

// ScheduleGateFromManager returns a ScheduleGate wrapping the
// manager with the same OwnerID as the role-based gate.
func ScheduleGateFromManager(mgr *leaser.Manager, ownerID string) ScheduleGate {
	return &managerScheduleGate{Mgr: mgr, OwnerID: ownerID}
}

type managerScheduleGate struct {
	Mgr     *leaser.Manager
	OwnerID string
}

func (g *managerScheduleGate) AcquireSchedule(ctx context.Context, scheduleID string) (context.Context, error) {
	_, err := g.Mgr.AcquireSchedule(ctx, scheduleID, g.OwnerID)
	if err != nil {
		if errors.Is(err, leaser.ErrAlreadyHeld) {
			return nil, nil
		}
		return nil, err
	}
	return ctx, nil
}

func (g *managerScheduleGate) ReleaseSchedule(ctx context.Context, scheduleID string) error {
	return g.Mgr.Release(ctx, scheduleID)
}

// ManagerGate adapts a *leaser.Manager to the LeaderGate interface
// for cron.Runner.
type ManagerGate struct {
	Mgr     *leaser.Manager
	OwnerID string
	Log     *slog.Logger
}

// LeaderGateFromManager returns a ManagerGate wrapping the manager.
func LeaderGateFromManager(mgr *leaser.Manager, ownerID string, log *slog.Logger) LeaderGate {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &ManagerGate{Mgr: mgr, OwnerID: ownerID, Log: log}
}

// Acquire delegates to leaser.Manager.Acquire. When the manager
// reports ErrAlreadyHeld, we return (nil, nil) — the caller is a
// follower and the runner will skip ticks. Successful Acquire
// returns ctx; the renew loop will exit the role from the
// manager's ActiveRoles if heartbeat fails past TTL, which the
// runner observes by polling.
func (g *ManagerGate) Acquire(ctx context.Context, role string) (context.Context, error) {
	_, err := g.Mgr.Acquire(ctx, role, g.OwnerID)
	if err != nil {
		if errors.Is(err, leaser.ErrAlreadyHeld) {
			return nil, nil
		}
		return nil, err
	}
	if g.Log != nil {
		g.Log.Info("cron: scheduler lease acquired",
			"role", role, "owner", g.OwnerID)
	}
	return ctx, nil
}

// Release delegates to leaser.Manager.Release.
func (g *ManagerGate) Release(ctx context.Context, role string) error {
	return g.Mgr.Release(ctx, role)
}
