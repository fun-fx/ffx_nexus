package core

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ScanRows is a small alias used across this package so the schedule
// store can accept the same pool.Query result without depending on
// every signature change pgx.Rows goes through.
type ScanRows = pgx.Rows

// Compile-time guard: pool.Query returns pgx.Rows, which has all the
// methods we need (Next, Scan, Close, Err). If at some point the
// driver changes, this line breaks before any caller does.
var _ ScanRows = (pgx.Rows)(nil)

// BenchmarkSchedule is one row of benchmark_schedules: operator intent
// about when to fire a run, plus the latest linkage marker so the
// console can answer "what was the last row this plan produced".
type BenchmarkSchedule struct {
	ID             string
	OrgID          string
	Name           string
	Environments   []string
	Model          string
	NumExamples    int
	Rollouts       int
	ViaGateway     bool
	CadenceSeconds int
	NextLaunchAt   time.Time
	Enabled        bool
	LastRunID      string
	LastLaunchedAt *time.Time
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// CreateBenchmarkSchedule persists a new schedule row. The ID must
// be supplied by the caller (uuid.NewString) so it can be referenced
// by benchmark_runs.schedule_id without an additional round-trip.
func (s *Store) CreateBenchmarkSchedule(ctx context.Context, r BenchmarkSchedule) error {
	if s == nil || s.pool == nil {
		return errors.New("core: store not configured")
	}
	if r.ID == "" {
		return errors.New("core: schedule id is required")
	}
	if r.Model == "" {
		return errors.New("core: schedule model is required")
	}
	if r.CadenceSeconds <= 0 {
		return errors.New("core: cadence must be positive")
	}
	envs := r.Environments
	if envs == nil {
		envs = []string{}
	}
	if r.NextLaunchAt.IsZero() {
		r.NextLaunchAt = time.Now().UTC().Add(time.Duration(r.CadenceSeconds) * time.Second)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO benchmark_schedules (
			id, org_id, name, environments, model, num_examples, rollouts,
			via_gateway, cadence_seconds, next_launch_at, enabled, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		r.ID, r.OrgID, r.Name, envs, r.Model, r.NumExamples, r.Rollouts,
		r.ViaGateway, r.CadenceSeconds, r.NextLaunchAt, r.Enabled, r.CreatedBy)
	return err
}

// ListBenchmarkSchedules returns schedules for an org, newest first.
// An empty org filter returns the cluster-wide list, matching the
// list semantics of benchmark_runs.
func (s *Store) ListBenchmarkSchedules(ctx context.Context, orgID string, limit int) ([]BenchmarkSchedule, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("core: store not configured")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows ScanRows
	var err error
	if orgID == "" {
		rows, err = s.pool.Query(ctx, `
			SELECT id, org_id, name, environments, model, num_examples, rollouts,
			       via_gateway, cadence_seconds, next_launch_at, enabled,
			       last_run_id, last_launched_at, created_by, created_at, updated_at
			FROM benchmark_schedules ORDER BY created_at DESC LIMIT $1`, limit)
	} else {
		rows, err = s.pool.Query(ctx, `
			SELECT id, org_id, name, environments, model, num_examples, rollouts,
			       via_gateway, cadence_seconds, next_launch_at, enabled,
			       last_run_id, last_launched_at, created_by, created_at, updated_at
			FROM benchmark_schedules WHERE org_id = $1 ORDER BY created_at DESC LIMIT $2`,
			orgID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BenchmarkSchedule{}
	for rows.Next() {
		r := BenchmarkSchedule{}
		var lastRun *string
		var lastLaunched *time.Time
		if err := rows.Scan(&r.ID, &r.OrgID, &r.Name, &r.Environments, &r.Model,
			&r.NumExamples, &r.Rollouts, &r.ViaGateway, &r.CadenceSeconds,
			&r.NextLaunchAt, &r.Enabled, &lastRun, &lastLaunched, &r.CreatedBy,
			&r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		if lastRun != nil {
			r.LastRunID = *lastRun
		}
		r.LastLaunchedAt = lastLaunched
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListDueBenchmarkSchedules returns enabled schedules whose next
// launch time is at or before now. The runner calls this on each
// tick to find what to fire next.
func (s *Store) ListDueBenchmarkSchedules(ctx context.Context, now time.Time, limit int) ([]BenchmarkSchedule, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("core: store not configured")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, org_id, name, environments, model, num_examples, rollouts,
		       via_gateway, cadence_seconds, next_launch_at, enabled,
		       last_run_id, last_launched_at, created_by, created_at, updated_at
		FROM benchmark_schedules
		WHERE enabled = TRUE AND next_launch_at <= $1
		ORDER BY next_launch_at ASC LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BenchmarkSchedule{}
	for rows.Next() {
		r := BenchmarkSchedule{}
		var lastRun *string
		var lastLaunched *time.Time
		if err := rows.Scan(&r.ID, &r.OrgID, &r.Name, &r.Environments, &r.Model,
			&r.NumExamples, &r.Rollouts, &r.ViaGateway, &r.CadenceSeconds,
			&r.NextLaunchAt, &r.Enabled, &lastRun, &lastLaunched, &r.CreatedBy,
			&r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		if lastRun != nil {
			r.LastRunID = *lastRun
		}
		r.LastLaunchedAt = lastLaunched
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateBenchmarkScheduleNext re-stamps a schedule's next-launch
// time. Distinct from the full UpdateBenchmarkSchedule because the
// runner fires this on every successful and failed launch, and the
// other fields should not be touched.
func (s *Store) UpdateBenchmarkScheduleNext(ctx context.Context, id string, when time.Time) error {
	if s == nil || s.pool == nil {
		return errors.New("core: store not configured")
	}
	if id == "" {
		return errors.New("core: schedule id is required")
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE benchmark_schedules SET next_launch_at = $2, updated_at = NOW()
		WHERE id = $1`, id, when)
	return err
}

// MarkBenchmarkScheduleLaunched stamps the linkage column so the
// console can present "the last run produced by this plan".
func (s *Store) MarkBenchmarkScheduleLaunched(ctx context.Context, scheduleID, runID string, when time.Time) error {
	if s == nil || s.pool == nil {
		return errors.New("core: store not configured")
	}
	if scheduleID == "" {
		return errors.New("core: schedule id is required")
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE benchmark_schedules
		SET last_run_id = $2, last_launched_at = $3, updated_at = NOW()
		WHERE id = $1`, scheduleID, runID, when)
	return err
}

// DeleteBenchmarkSchedule removes a schedule. Existing in-flight
// runs linked via schedule_id are unaffected — the linkage is just a
// string and the run row still carries it.
func (s *Store) DeleteBenchmarkSchedule(ctx context.Context, id string) error {
	if s == nil || s.pool == nil {
		return errors.New("core: store not configured")
	}
	if id == "" {
		return errors.New("core: schedule id is required")
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM benchmark_schedules WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetBenchmarkSchedule reads one schedule by id. Mainly used by the
// console to round-trip a single row for editing; the runner never
// needs this path because it scans by due time.
func (s *Store) GetBenchmarkSchedule(ctx context.Context, id string) (BenchmarkSchedule, error) {
	if s == nil || s.pool == nil {
		return BenchmarkSchedule{}, errors.New("core: store not configured")
	}
	if id == "" {
		return BenchmarkSchedule{}, errors.New("core: schedule id is required")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, org_id, name, environments, model, num_examples, rollouts,
		       via_gateway, cadence_seconds, next_launch_at, enabled,
		       last_run_id, last_launched_at, created_by, created_at, updated_at
		FROM benchmark_schedules WHERE id = $1`, id)
	if err != nil {
		return BenchmarkSchedule{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return BenchmarkSchedule{}, err
		}
		return BenchmarkSchedule{}, ErrNotFound
	}
	r := BenchmarkSchedule{}
	var lastRun *string
	var lastLaunched *time.Time
	if err := rows.Scan(&r.ID, &r.OrgID, &r.Name, &r.Environments, &r.Model,
		&r.NumExamples, &r.Rollouts, &r.ViaGateway, &r.CadenceSeconds,
		&r.NextLaunchAt, &r.Enabled, &lastRun, &lastLaunched, &r.CreatedBy,
		&r.CreatedAt, &r.UpdatedAt); err != nil {
		return BenchmarkSchedule{}, err
	}
	if lastRun != nil {
		r.LastRunID = *lastRun
	}
	r.LastLaunchedAt = lastLaunched
	return r, nil
}
