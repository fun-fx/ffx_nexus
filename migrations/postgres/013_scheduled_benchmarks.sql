-- 013_scheduled_benchmarks.sql
-- Operator-defined recurring benchmark launches. Drives the cron runner
-- in internal/cron. Each row is the persistent operator intent; the runner
-- reads enabled rows whose next_launch_at has passed, fires them, and
-- updates the schedule on success.

CREATE TABLE IF NOT EXISTS benchmark_schedules (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL DEFAULT '',
    name            TEXT NOT NULL DEFAULT '',
    environments    TEXT[] NOT NULL DEFAULT '{}',
    model           TEXT NOT NULL,
    num_examples    INTEGER NOT NULL DEFAULT 5,
    rollouts        INTEGER NOT NULL DEFAULT 1,
    via_gateway     BOOLEAN NOT NULL DEFAULT TRUE,
    cadence_seconds INTEGER NOT NULL,
    next_launch_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    created_by      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Only enabled schedules are scanned by the cron tick. The partial index
-- keeps the scan cheap as the table grows beyond a handful of rows.
CREATE INDEX IF NOT EXISTS idx_benchmark_schedules_due
    ON benchmark_schedules (next_launch_at)
    WHERE enabled = TRUE;

-- Backfill the schedule_id link so existing benchmark_runs remain
-- traceable once scheduled launches settle. Default '' means "manual",
-- preserving the historical default.
ALTER TABLE benchmark_runs
    ADD COLUMN IF NOT EXISTS schedule_id TEXT NOT NULL DEFAULT '';
