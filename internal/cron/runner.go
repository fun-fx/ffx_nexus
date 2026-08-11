// Package cron owns the scheduled-benchmark timetable.
//
// A schedule is operator intent ("re-run this shape on a cadence") and
// a launch is a single launched run against the provider. Keeping the
// two persistent surfaces in different tables is what lets the runner
// re-stamp due-times without re-asserting the row's history.
//
// The runner is one goroutine on each replica; the work it does is
// idempotent because PUT next_launch_at = NOW() + cadence is the only
// state transition, and two replicas racing the same stamp converge
// on identical rows. We do not elect a leader, matching the existing
// benchmark poll goroutine.
package cron

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Spec is one row from benchmark_schedules plus the interfaces the
// runner needs to launch it and record the row it produced.
type Spec struct {
	ID            string
	OrgID         string
	Name          string
	Environments  []string
	Model         string
	NumExamples   int
	Rollouts      int
	ViaGateway    bool
	Cadence       time.Duration
	NextLaunchAt  time.Time
}

// Lander asks the benchmark package to launch a run.
//
// Returning the id of the run the provider accepted lets the runner
// stamp the schedule with the linkage without a second read pass.
type Lander interface {
	RunSchedule(ctx context.Context, spec Spec) (runID string, err error)
}

// LanderFunc adapts an ordinary function to the Lander interface so
// the cron package can be wired up against any object that exposes
// launch logic without forcing that object to import internal/cron.
type LanderFunc func(ctx context.Context, spec Spec) (string, error)

// RunSchedule satisfies Lander by delegating to the wrapped function.
func (f LanderFunc) RunSchedule(ctx context.Context, spec Spec) (string, error) {
	return f(ctx, spec)
}

// Store persists the table. The runner does not need full CRUD — just
// the slices it scans each tick and the update for the next due stamp.
type Store interface {
	ListDueSchedules(ctx context.Context, now time.Time, limit int) ([]Spec, error)
	UpdateNextLaunchAt(ctx context.Context, id string, when time.Time) error
	// MarkLaunched stamps the schedule's last-known linkage to a launched
	// row. Persisting this here keeps the schedule row sufficient to
	// answer "what did we launch last for this plan" without joining.
	MarkLaunched(ctx context.Context, scheduleID, runID string, when time.Time) error
	// GetScheduleByID is used by the staleness auto-relaunch path to
	// walk one schedule's full row. Distinct from ListDueSchedules
	// because by then the schedule may have a last_launched_at older
	// than the staleness threshold, and we want to bump its
	// next_launch_at to "now" so the normal fire path takes care of
	// it on the next tick.
	GetScheduleByID(ctx context.Context, id string) (Spec, error)
}

// tickInterval matches the existing benchmark poller cadence. Half a
// minute keeps observed drift under one tick for a 1-minute schedule
// without doubling the database read load. The repository's poll loop
// at cmd/nexus/benchmarks.go uses time.Minute because each deposit is a
// provider round-trip; here the deposit is a DB scan, so we can run
// lighter without changing the user-visible behaviour.
const tickInterval = 30 * time.Second

// Runner owns the schedule loop until the context is cancelled.
type Runner struct {
	store  Store
	lander Lander
	log    *slog.Logger

	mu         sync.Mutex
	lastErrors map[string]string // schedule_id → last error string
}

// New builds a Runner. Tests do not pass a logger; nil is replaced
// with a discarding one so nil-deref cannot happen in a production
// panic.
func New(store Store, lander Lander, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Runner{
		store:      store,
		lander:     lander,
		log:        log,
		lastErrors: map[string]string{},
	}
}

// Run loops until ctx is done. The loop is safe to start on every
// replica: UPDATE next_launch_at is the only stateful write, and the
// result is independent of who wins the stamp because the next due
// time follows from cadence, not the timestamp.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	// Tickers drop ticks when the consumer takes too long; poll
	// immediately at boot so a fresh launch schedule does not have to
	// wait one tick before its first fire. The risk is double-fire on
	// the very first tick (this synth + first ticker) and the read
	// itself is cheap enough we accept it.
	r.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
			r.stalenessTick(ctx)
		}
	}
}

// stalenessTick is the PR-7 auto-relaunch path. The runner lists
// every enabled schedule whose last launch is older than the stale
// grace period and re-pins next_launch_at to "now", so the next
// ordinary fire lands a run. We deliberately do not launch directly:
// pushing through UpdateNextLaunchAt keeps a single firing path,
// and the audit log records the regression via the same "scheduled
// launch" trail the operator already audits.
//
// Operators who do NOT want the auto-relaunch can leave
// NEXUS_BENCH_AUTO_RELAUNCH unset (default). The default grace is
// 5x the configured cadence — long enough that a missed run is not
// treated as a regression but short enough that a slack of two full
// intervals is.
func (r *Runner) stalenessTick(ctx context.Context) {
	if r == nil || r.store == nil || !autoRelaunchEnabled() {
		return
	}
	now := time.Now().UTC()
	due, err := r.store.ListDueSchedules(ctx, now, 256)
	if err != nil {
		return
	}
	for _, s := range due {
		if isAutoRelaunchLookbackNeeded(s) {
			if err := r.store.UpdateNextLaunchAt(ctx, s.ID, now); err != nil {
				r.log.Warn("cron: auto-relaunch re-stamp failed",
					"schedule", s.ID, "err", err)
			}
			r.log.Info("cron: stale schedule bumped to now",
				"schedule", s.ID, "model", s.Model,
				"since_last_launch", time.Since(s.NextLaunchAt).Round(time.Minute))
		}
	}
}

// isAutoRelaunchLookbackNeeded asks: "did this schedule fail to fire
// at some point in its recent past — possibly because credentials
// were missing — and now it is silently looping on a long stale
// gap?". The heuristic: last fired > grace, and the schedule has
// not been re-pinned within the same window. A schedule with
// NextLaunchAt == NextLaunchAt (i.e. undefined) is the same as
// "we have no record of launching, treat it as a launch attempt".
func isAutoRelaunchLookbackNeeded(s Spec) bool {
	// No NextLaunchAt past: the schedule row is fresh or has been
	// recently inserted; no staleness yet.
	if s.NextLaunchAt.IsZero() {
		return false
	}
	grace := time.Duration(5) * s.Cadence
	if grace < time.Hour {
		grace = time.Hour
	}
	return time.Since(s.NextLaunchAt) > grace
}

// autoRelaunchEnabled reads the operator opt-out. The flag is
// negative-named so unconfigured deployments get the safer default
// (no silent re-fires).
func autoRelaunchEnabled() bool {
	if v := getEnv("NEXUS_BENCH_AUTO_RELAUNCH"); v != "" {
		return v != "0" && v != "false"
	}
	return false
}

// getEnv is wrapped so tests can substitute a fixed environment.
// Production paths read os.Getenv directly via this helper.
var getEnv = func(name string) string {
	return os.Getenv(name)
}

func (r *Runner) tick(ctx context.Context) {
	if r.store == nil || r.lander == nil {
		return
	}
	now := time.Now().UTC()
	specs, err := r.store.ListDueSchedules(ctx, now, 64)
	if err != nil {
		r.log.Warn("cron: list due schedules failed", "err", err)
		return
	}
	for _, s := range specs {
		r.fire(ctx, s, now)
	}
}

func (r *Runner) fire(ctx context.Context, s Spec, now time.Time) {
	runID, err := r.lander.RunSchedule(ctx, s)
	if err != nil {
		r.log.Warn("cron: schedule fire failed",
			"schedule", s.ID, "model", s.Model, "err", err)
		r.mu.Lock()
		r.lastErrors[s.ID] = err.Error()
		r.mu.Unlock()
		// Push the next attempt out by one cadence so a permanently
		// broken schedule cannot pound the provider. Otherwise next
		// launch stays at "...<= now()" and we re-enter fire() on the
		// next tick without backoff.
		next := now.Add(s.Cadence)
		if err := r.store.UpdateNextLaunchAt(ctx, s.ID, next); err != nil {
			r.log.Warn("cron: post-failure re-stamp failed",
				"schedule", s.ID, "err", err)
		}
		return
	}
	next := now.Add(s.Cadence)
	if err := r.store.UpdateNextLaunchAt(ctx, s.ID, next); err != nil {
		r.log.Warn("cron: post-launch re-stamp failed",
			"schedule", s.ID, "err", err)
		// Do not bail; the link between schedule and run is still
		// useful even if the next-due time falls back to schedule
		// definition. The next tick will re-stamp from the canonical
		// next_launch_at column.
	}
	if err := r.store.MarkLaunched(ctx, s.ID, runID, now); err != nil {
		r.log.Warn("cron: mark-launched failed",
			"schedule", s.ID, "run", runID, "err", err)
	}
	r.mu.Lock()
	delete(r.lastErrors, s.ID)
	r.mu.Unlock()
	r.log.Info("cron: schedule fired",
		"schedule", s.ID, "run", runID, "model", s.Model, "samples", s.NumExamples*s.Rollouts)
}

// LastError returns the most recent launch error for a schedule, or
// the empty string if the last launch succeeded.
func (r *Runner) LastError(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErrors[id]
}
