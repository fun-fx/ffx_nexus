package benchmark

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ScheduleSpec is the cron package's view of one row in
// benchmark_schedules. Kept as a separate struct so the cron package
// does not depend on internal/cron directly and we can keep its
// handlers short. The fields mirror Cron.Spec one-for-one except the
// cron-side cadence is encoded as time.Duration rather than seconds.
type ScheduleSpec struct {
	ID           string
	OrgID        string
	Name         string
	Environments []string
	Model        string
	NumExamples  int
	Rollouts     int
	ViaGateway   bool
	Cadence      time.Duration
	NextLaunchAt time.Time
}

// ScheduleLander is what cron.Runner calls to fire a schedule. The
// benchmark package implements it on Runner, so the cron package has
// to depend on us only via this interface — same shape cron.Runner
// uses internally (see internal/cron/runner.go).
type ScheduleLander interface {
	RunSchedule(ctx context.Context, s ScheduleSpec) (runID string, err error)
}

// Compile-time assertion: benchmark.Runner implements ScheduleLander.
// Keeps a future signature change visible at build time.
var _ ScheduleLander = (*Runner)(nil)

// RunSchedule is the cron-facing entry point. It is a thin shim over
// Launch that (1) refuses to act when the schedule row points at an
// org without a credential, (2) stamps the produced run with the
// schedule id so the row can be linked back, and (3) does not re-fire
// when the same call is replayed: a fire() that produced a row whose
// external_id is non-empty is already on the way to settlement.
func (r *Runner) RunSchedule(ctx context.Context, s ScheduleSpec) (string, error) {
	if r == nil || r.store == nil {
		return "", errors.New("benchmark: runner not configured")
	}
	if s.ID == "" {
		return "", fmt.Errorf("%w: schedule id is required", ErrInvalidRequest)
	}
	if s.Model == "" {
		return "", fmt.Errorf("%w: schedule model is required", ErrInvalidRequest)
	}
	if len(s.Environments) == 0 {
		return "", fmt.Errorf("%w: at least one environment is required", ErrInvalidRequest)
	}
	if s.Cadence <= 0 {
		return "", fmt.Errorf("%w: cadence must be positive", ErrInvalidRequest)
	}
	spec := LaunchSpec{
		OrgID:        s.OrgID,
		ActorID:      "cron:schedule:" + s.ID,
		Name:         s.Name,
		ScheduleID:   s.ID,
		Environments: s.Environments,
		Model:        s.Model,
		NumExamples:  s.NumExamples,
		Rollouts:     s.Rollouts,
		ViaGateway:   s.ViaGateway,
	}
	run, err := r.Launch(ctx, spec)
	if err != nil {
		// Launch returns the partially-recorded run alongside a launch
		// failure, but RunSchedule only reports the id. A failure that
		// returned a row (status=failed) still has a usable id from
		// Nexus's perspective — the row tracks the refusal for the
		// console even when the provider never accepted.
		if run.ID != "" {
			return run.ID, err
		}
		return "", err
	}
	return run.ID, nil
}
