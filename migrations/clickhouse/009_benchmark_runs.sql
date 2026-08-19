-- Benchmark runs in ClickHouse — the same model-level quality
-- measurements as Postgres migrations/012_benchmark_runs.sql, but
-- with a MergeTree shape that fits the read pattern (most-recent
-- settled row per model) rather than the Postgres partial-index
-- shape.
--
-- The router reads `WHERE status = 'completed' AND avg_score IS NOT
-- NULL ORDER BY completed_at DESC` once per `StatsProvider` cycle.
-- Sorting the engine by `(status, model)` keeps the scan on the
-- `completed` partition short, and the per-model argMax() picks
-- the most recent row without touching any rows we don't care
-- about. A separate `(completed_at)` projection is unnecessary —
-- MergeTree's primary key order gives us `completed_at` sorted
-- within each `(status, model)` group already.
--
-- A nullable String column is the standard CH idiom for "may be
-- unset"; we use `Default ''` everywhere fields mapping back to
-- PostgreSQL `NOT NULL DEFAULT ''` so the same wire shape arrives
-- at the Go layer. JSON is `String` (we read it as `JSONEachRow`
-- with low-cardinality parsing on demand), matching how the eval
-- subsystem stores its own dictionaries in
-- migrations/clickhouse/002_eval_context.sql.

CREATE TABLE IF NOT EXISTS benchmark_runs (
    id              String,
    org_id          LowCardinality(String) DEFAULT '',
    provider        LowCardinality(String) DEFAULT 'primeintellect',
    -- Vendor-side evaluation UUID; empty until the launch call returns so
    -- a row can exist in 'failed' state for a launch that never started.
    external_id     String DEFAULT '',
    name            String DEFAULT '',
    -- Hub slugs such as 'primeintellect/gsm8k'. Stored verbatim because
    -- the vendor owns the catalogue and there is no environments API to
    -- reference; the value is opaque to Nexus.
    environments    Array(String) DEFAULT [],
    model           String,
    num_examples    UInt32 DEFAULT 5,
    rollouts        UInt32 DEFAULT 1,
    -- TRUE when the vendor was told to send inference through this
    -- Nexus gateway (routing + cache + provider choice inclusive) rather
    -- than the vendor's own serving of the same model. Kept for the
    -- routing blend: a via_gateway=true row beats a via_gateway=false
    -- one with the same model + completed_at, because the former is
    -- measuring what Nexus actually serves.
    via_gateway     UInt8 DEFAULT 1,
    -- Virtual key mint; non-empty until the run finishes and we revoke
    -- it. Keeping it on the row avoids re-deriving from the audit log.
    vkey_id         String DEFAULT '',
    -- Our lifecycle: pending | running | completed | failed | cancelled.
    status          LowCardinality(String) DEFAULT 'pending',
    -- Vendor's raw status string preserved verbatim — their state machine
    -- (PROCESSING, TIMEOUT, …) is richer than ours and we don't want
    -- to lose detail when collapsing.
    external_status String DEFAULT '',
    avg_score       Nullable(Float64),
    min_score       Nullable(Float64),
    max_score       Nullable(Float64),
    total_samples   Nullable(UInt32),
    -- JSON blob; nullable because Phase 1 benchmarks don't always
    -- surface the same shape across vendors.
    metrics         Nullable(String),
    viewer_url      String DEFAULT '',
    error           String DEFAULT '',
    created_by      String DEFAULT '',
    created_at      DateTime64(9) DEFAULT now64(9),
    updated_at      DateTime64(9) DEFAULT now64(9),
    started_at      Nullable(DateTime64(9)),
    -- Nullable so unsettled runs do not pollute the ORDER BY tuple —
    -- a synthetic epoch would let argMax incorrectly promote a never-
    -- finished row. The matching `allow_nullable_key=1` setting
    -- tells MergeTree that being null is the storage default here;
    -- the value is still cheap to compare because nullable
    -- DateTime64 sorts nulls first under CH's default ordering.
    completed_at    Nullable(DateTime64(9))
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(created_at)
ORDER BY (status, model, completed_at)
SETTINGS index_granularity = 8192, allow_nullable_key = 1;

-- Status + model + completed_at is the read pattern that powers the
-- router's blending decision. The composite sort key above serves it
-- without a secondary skip-index in the common case; only a small
-- granule of rows is scanned because (status, model) is the leading
-- prefix, and the engine only navigates into rows whose
-- `completed_at` falls in the latest few entries when the row count
-- per model is small.
--
-- Skip index on the JSON payload, in case a dashboard query lands on
-- a specific metric. Cost is negligible because the granule is 8K
-- rows; the index only fires when the predicate names the column.
ALTER TABLE benchmark_runs
    ADD INDEX IF NOT EXISTS idx_benchmark_runs_metrics_metric
    (metrics) TYPE bloom_filter() GRANULARITY 4;
