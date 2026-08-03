-- Benchmark runs: model-level quality measurements executed by an
-- external eval platform (PrimeIntellect hosted evaluations today).
--
-- This is deliberately NOT an eval_plugins row. A plugin scores one
-- trace that already happened; a benchmark run asks a vendor to drive
-- a dataset against a model and report an aggregate. The input is a
-- model, not a trace, so spec.send.payload has nothing to render and
-- the per-trace dispatcher has nothing to dispatch.
--
-- Rows are the durable record of a run we started elsewhere:
-- external_id is the vendor's evaluation id, and status is polled
-- until it reaches a terminal value. Scores are stored raw rather
-- than folded into eval_scores because nothing consumes them for
-- routing yet — that wiring is a separate decision.
CREATE TABLE IF NOT EXISTS benchmark_runs (
    id                TEXT PRIMARY KEY,
    org_id            TEXT NOT NULL DEFAULT '',
    provider          TEXT NOT NULL DEFAULT 'primeintellect',
    -- Vendor-side identifier. Empty until the launch call returns, so a
    -- row can exist in 'failed' state for a launch that never started.
    external_id       TEXT NOT NULL DEFAULT '',
    name              TEXT NOT NULL DEFAULT '',
    -- Hub slugs such as 'primeintellect/gsm8k'. Stored as text rather
    -- than a join table: the vendor owns the catalogue and there is no
    -- environments API to reference, so these are opaque to us.
    environments      TEXT[] NOT NULL DEFAULT '{}',
    model             TEXT NOT NULL,
    num_examples      INTEGER NOT NULL DEFAULT 5,
    rollouts          INTEGER NOT NULL DEFAULT 1,
    -- TRUE when the vendor was told to send inference through this
    -- Nexus gateway, which is what makes the score describe what we
    -- actually serve (routing, cache and provider choice included)
    -- rather than the vendor's own serving of the same model.
    via_gateway       BOOLEAN NOT NULL DEFAULT TRUE,
    -- Virtual key minted for that gateway access, kept so the run can
    -- revoke it once the vendor no longer needs to call us.
    vkey_id           TEXT NOT NULL DEFAULT '',
    -- Our lifecycle: pending | running | completed | failed | cancelled.
    status            TEXT NOT NULL DEFAULT 'pending',
    -- The vendor's raw status string, preserved because their state
    -- machine is richer than ours (PROCESSING, TIMEOUT, …) and we do
    -- not want to lose detail when collapsing it.
    external_status   TEXT NOT NULL DEFAULT '',
    avg_score         DOUBLE PRECISION,
    min_score         DOUBLE PRECISION,
    max_score         DOUBLE PRECISION,
    total_samples     INTEGER,
    metrics           JSONB,
    viewer_url        TEXT NOT NULL DEFAULT '',
    error             TEXT NOT NULL DEFAULT '',
    created_by        TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at        TIMESTAMPTZ,
    completed_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_benchmark_runs_org_created
    ON benchmark_runs (org_id, created_at DESC);

-- The poller scans for rows that have not settled yet. Partial index
-- so the scan cost stays flat as completed history accumulates.
CREATE INDEX IF NOT EXISTS idx_benchmark_runs_unsettled
    ON benchmark_runs (updated_at)
    WHERE status IN ('pending', 'running');
