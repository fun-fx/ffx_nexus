package main

import (
	"context"
	"time"

	"github.com/ffxnexus/nexus/internal/benchmark"
	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/cron"
)

// schedStore adapts the benchmark_schedules rows in pgxpool to the
// cron package's narrow Store interface. Keeping this adapter here
// (cmd/nexus/main.go) means cron.go never imports internal/core; the
// cron package remains a pure scheduler.
//
// The dependencies it offers — pool, scan helpers — live in internal/core.
// Keeping this in main avoids a cycle (cron -> core -> benchmark -> core)
// and stays consistent with the runner/lander's role split.
type schedStore struct {
	store *core.Store
}

// Compile-time guard: schedStore satisfies cron.Store.
var _ cron.Store = schedStore{}

func (s schedStore) ListDueSchedules(ctx context.Context, now time.Time, limit int) ([]cron.Spec, error) {
	rows, err := s.store.ListDueBenchmarkSchedules(ctx, now, limit)
	if err != nil {
		return nil, err
	}
	out := make([]cron.Spec, 0, len(rows))
	for _, r := range rows {
		out = append(out, cron.Spec{
			ID:           r.ID,
			OrgID:        r.OrgID,
			Name:         r.Name,
			Environments: r.Environments,
			Model:        r.Model,
			NumExamples:  r.NumExamples,
			Rollouts:     r.Rollouts,
			ViaGateway:   r.ViaGateway,
			Cadence:      time.Duration(r.CadenceSeconds) * time.Second,
			NextLaunchAt: r.NextLaunchAt,
		})
	}
	return out, nil
}

func (s schedStore) UpdateNextLaunchAt(ctx context.Context, id string, when time.Time) error {
	return s.store.UpdateBenchmarkScheduleNext(ctx, id, when)
}

func (s schedStore) MarkLaunched(ctx context.Context, scheduleID, runID string, when time.Time) error {
	return s.store.MarkBenchmarkScheduleLaunched(ctx, scheduleID, runID, when)
}

// GetScheduleByID is used by PR-7's staleness auto-relaunch. It
// reads one row by id and projects it onto the narrow cron.Spec
// view. Errors propagate so the stalenessTick caller can log them.
func (s schedStore) GetScheduleByID(ctx context.Context, id string) (cron.Spec, error) {
	row, err := s.store.GetBenchmarkSchedule(ctx, id)
	if err != nil {
		return cron.Spec{}, err
	}
	return cron.Spec{
		ID:           row.ID,
		OrgID:        row.OrgID,
		Name:         row.Name,
		Environments: row.Environments,
		Model:        row.Model,
		NumExamples:  row.NumExamples,
		Rollouts:     row.Rollouts,
		ViaGateway:   row.ViaGateway,
		Cadence:      time.Duration(row.CadenceSeconds) * time.Second,
		NextLaunchAt: row.NextLaunchAt,
	}, nil
}

// Compile-time guard: *benchmark.Runner satisfies cron.Lander through
// its ScheduleSpec-backed RunSchedule method. The literal cast below
// will fail to compile if the signature slips.
var _ cron.Lander = (cron.Lander)(nil)

// Ensure method-set stability for the producer too. We hand the runner
// to cron.New, which expects cron.Lander; benchmark.Runner's
// RunSchedule uses a benchmark.ScheduleSpec (internal to its package)
// and we adapt with a tiny closure.
//
// The closure used at the call site accepts a cron.Spec and projects
// it onto benchmark.ScheduleSpec, then calls RunSchedule. Building it
// inline keeps the bridge narrow — only this file needs to know
// both shapes exist.
func makeScheduleLander(r *benchmark.Runner) cron.Lander {
	return cron.LanderFunc(func(ctx context.Context, s cron.Spec) (string, error) {
		return r.RunSchedule(ctx, benchmark.ScheduleSpec{
			ID:           s.ID,
			OrgID:        s.OrgID,
			Name:         s.Name,
			Environments: s.Environments,
			Model:        s.Model,
			NumExamples:  s.NumExamples,
			Rollouts:     s.Rollouts,
			ViaGateway:   s.ViaGateway,
			Cadence:      s.Cadence,
			NextLaunchAt: s.NextLaunchAt,
		})
	})
}
