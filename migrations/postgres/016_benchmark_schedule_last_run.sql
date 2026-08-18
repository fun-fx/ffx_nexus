-- Add the last-run tracking columns that internal/core/benchmark_schedules.go
-- has always read but 013_scheduled_benchmarks.sql never created.
--
-- Every SELECT in that file lists last_run_id and last_launched_at, and
-- MarkScheduleLaunched UPDATEs them. On any database built from the migration
-- set, all of those statements fail with SQLSTATE 42703 (undefined column).
-- Benchmark schedules can be created, but reading one back, listing them, and
-- recording a launch are all broken on a fresh install.
--
-- This is the same defect class as the missing invite_tokens table: code and
-- schema diverged because no test exercised the pair against a database built
-- the way a customer's is. It went unnoticed because
-- internal/core/benchmark_schedules_test.go is not named *Integration and so was
-- not selected by CI's `go test ./internal/core/ -run Integration` step, while
-- the default `go test ./...` skips it for want of NEXUS_TEST_POSTGRES_URL. CI
-- now runs the whole package against a real Postgres.
--
-- Additive and idempotent: ADD COLUMN IF NOT EXISTS is a catalogue-only
-- operation here because both columns are nullable with no default, so it does
-- not rewrite the table and is safe on a live database.
--
-- Both columns are NULLABLE on purpose. A schedule that has never launched has
-- no run id and no launch time, and NULL states that correctly. A sentinel like
-- the empty string or the epoch would make "never ran" indistinguishable from
-- "ran and we lost the value", and the scanner uses NULL-ness to decide whether
-- a schedule is due for its first launch.

ALTER TABLE benchmark_schedules
    ADD COLUMN IF NOT EXISTS last_run_id TEXT;

ALTER TABLE benchmark_schedules
    ADD COLUMN IF NOT EXISTS last_launched_at TIMESTAMPTZ;

-- The scheduler asks "which enabled schedules are due", ordering by
-- next_launch_at; the existing partial index already serves that. This index
-- serves the console's per-org list, which sorts by most recently launched and
-- would otherwise sort the whole table.
CREATE INDEX IF NOT EXISTS idx_benchmark_schedules_org_last_launched
    ON benchmark_schedules (org_id, last_launched_at DESC NULLS LAST);
