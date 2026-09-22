-- Reconstructable empty-DB fixture for clickhouse.
-- Concatenation of migrations/{engine}/*.sql in ordinal order.
-- This is the schema Elixir must recreate. Do not invent a parallel ledger.
-- Generated for Phase 0; re-run scripts/dump_schema.sh after adding migrations.

-- ===== clickhouse/001_init.sql =====
-- Nexus observability schema (ClickHouse).
-- Column names follow OpenTelemetry GenAI semantic conventions where applicable.

CREATE TABLE IF NOT EXISTS gateway_traces
(
    trace_id            String,
    span_id             String,
    parent_span_id      String,
    timestamp           DateTime64(3),

    org_id              String,
    virtual_key_id      String,

    operation_name      LowCardinality(String),  -- gen_ai.operation.name
    provider_name       LowCardinality(String),  -- gen_ai.provider.name
    request_model       LowCardinality(String),  -- gen_ai.request.model
    response_model      LowCardinality(String),  -- gen_ai.response.model
    input_tokens        UInt32,                   -- gen_ai.usage.input_tokens
    output_tokens       UInt32,                   -- gen_ai.usage.output_tokens
    finish_reason       LowCardinality(String),   -- gen_ai.response.finish_reasons
    temperature         Float64,
    top_p               Float64,
    max_tokens          UInt32,

    streamed            UInt8,
    ttft_ms             Int64,
    latency_ms          Int64,
    cost_usd            Float64,

    status_code         UInt16,
    error_type          LowCardinality(String),
    error_message       String,

    input_messages      String,
    output_messages     String
)
ENGINE = MergeTree
PARTITION BY toDate(timestamp)
ORDER BY (org_id, timestamp, trace_id)
TTL toDateTime(timestamp) + INTERVAL 90 DAY;

-- Eval scores produced by async workers (Phase 3). Joined to traces by trace_id.
CREATE TABLE IF NOT EXISTS eval_scores
(
    trace_id        String,
    timestamp       DateTime64(3),
    evaluator       LowCardinality(String),  -- e.g. "slm_judge", "heuristic_pii"
    metric          LowCardinality(String),  -- e.g. "hallucination", "relevance"
    score           Float64,
    passed          UInt8,
    rationale       String,
    judge_model     LowCardinality(String)
)
ENGINE = MergeTree
PARTITION BY toDate(timestamp)
ORDER BY (metric, timestamp, trace_id)
TTL toDateTime(timestamp) + INTERVAL 90 DAY;

-- ===== clickhouse/002_eval_context.sql =====
-- RAG eval context columns on gateway traces (client nexus_eval block).

ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS retrieval_contexts String DEFAULT '';
ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS eval_reference String DEFAULT '';

-- ===== clickhouse/003_dashboard.sql =====
-- Dashboard columns: persist semantic-cache hits and inline guardrail actions so
-- the console can show cache-hit rate and guardrail activity over a time window
-- (previously these were only visible on the live trace feed).

ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS cache_hit UInt8 DEFAULT 0;
ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS guardrail_action LowCardinality(String) DEFAULT '';

-- ===== clickhouse/004_byok.sql =====
-- BYOK / multi-tenancy trace columns: attribute each request to its owning user
-- and record which upstream key served it (env / org / user), so the dashboard
-- can show per-user usage + quality and BYOK adoption.

ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS user_id LowCardinality(String) DEFAULT '';
ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS credential_source LowCardinality(String) DEFAULT '';

-- ===== clickhouse/005_eval_user.sql =====
-- Per-user quality: denormalize the caller's user_id onto eval scores so the
-- dashboard can aggregate "this user's rolling quality score" without joining
-- back to gateway_traces. This is the eval/observability differentiator over
-- gateways that track only per-key spend (see docs/byok-multitenancy-design.md §9).
ALTER TABLE eval_scores ADD COLUMN IF NOT EXISTS user_id LowCardinality(String) DEFAULT '';

-- ===== clickhouse/006_replica_id.sql =====
-- Multi-node scaling: capture the gateway replica id on every trace so the
-- operator can group by replica (`SELECT count() FROM gateway_traces GROUP BY
-- replica_id`) and detect skew, hot pods, or LB misconfigurations. The column
-- is the value of NEXUS_REPLICA_ID, otherwise "<hostname>-<randhex>".

ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS replica_id LowCardinality(String) DEFAULT '';

-- ===== clickhouse/007_session_id.sql =====
-- Session roll-up: capture the per-call session marker so /api/stats
-- aggregates and the overview UI can fold N consecutive traces from one
-- conversation (Cursor agent loops, multi-tool runs) into a single
-- session row. Wire sources are, in order of preference:
--   1. chat-completions extras `metadata.session_id`
--   2. chat-completions extras `metadata.sessionId`
--   3. chat-completions extras `metadata.conversation_id`
--   4. the `user` field (OpenAI's per-end-user identifier, set by
--      some clients to a stable id)
--   5. response provider header `X-Cursor-session-id` if we ever
--      surface it (not currently observed in prod)
-- The gateway writes the resolved string to this column and falls
-- back to '' when none of the above is present, so older rows stay
-- empty without breaking aggregations.
--
-- Stored as LowCardinality(String) like virtual_key_id — values are
-- either an opaque UUID or a very small set of empty / sentinel
-- strings, so the dictionary encoding stays tiny even at high QPS.

ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS session_id LowCardinality(String) DEFAULT '';
ALTER TABLE gateway_traces ADD INDEX IF NOT EXISTS idx_gt_session_id (session_id) TYPE bloom_filter() GRANULARITY 3;

-- ===== clickhouse/008_turn_id.sql =====
-- Turn roll-up: group the N model calls an agent makes while answering a
-- single user question into one console row.
--
-- session_id (007) cannot do this job. It is only populated when the
-- client sets metadata.session_id, which no client we serve does today,
-- and its `user:<id>` fallback is per-end-user rather than per-turn.
-- parent_span_id cannot either — it carries X-Request-Id, so an agent
-- loop lands as one group per HTTP call instead of one group per turn.
--
-- turn_id is therefore derived gateway-side from the request payload:
-- sha256(user_id, system prompt, last user-role message), truncated to
-- 16 hex chars. See deriveTurnKey in internal/gateway/turnkey.go for why
-- the last user message is the stable turn boundary.
--
-- Plain String, not LowCardinality: unlike session_id these are hashes,
-- one per turn, so the dictionary would grow without bound. The bloom
-- filter carries the drill-down lookup (WHERE turn_id = ?) instead.
--
-- Rows written before this migration keep '' and the console renders
-- them one-per-row, so the backfill-free rollout stays non-destructive.

ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS turn_id String DEFAULT '';
ALTER TABLE gateway_traces ADD INDEX IF NOT EXISTS idx_gt_turn_id (turn_id) TYPE bloom_filter() GRANULARITY 3;

-- ===== clickhouse/009_benchmark_runs.sql =====
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

-- ===== clickhouse/010_eval_scores_org.sql =====
-- 010: attribute eval scores to an organization (ClickHouse side).
--
-- Mirrors migrations/postgres/015_eval_scores_org.sql. See that file for why the
-- column is needed; the reasoning is identical and the read paths are the same
-- queries against a different store.
--
-- ClickHouse specifics:
--
--   * ADD COLUMN IF NOT EXISTS is idempotent, which is what makes this safe to
--     replay. ClickHouse has no transactions and no advisory lock, so
--     replay-safety is the only thing making a concurrent or retried migration
--     correct (migrations/README.md).
--   * DEFAULT rather than a backfill UPDATE. ClickHouse mutations
--     (ALTER TABLE ... UPDATE) are asynchronous and rewrite parts in the
--     background, so a migration cannot wait for one and cannot report whether
--     it succeeded. Historical rows therefore read as the default org.
--   * The column is added at the end and is not part of the sorting key.
--     Changing ORDER BY would require rebuilding the table, which for a
--     customer's accumulated trace history is an outage, not a migration.
--
-- WHERE THIS DIFFERS FROM POSTGRES, AND WHY. The Postgres migration varies its
-- backfill by org count: a multi-org installation's unattributable rows are
-- parked in 'unattributed' instead of being guessed into the default org. This
-- migration cannot do the same. Org membership lives in Postgres (there is no
-- `organizations` table here), so ClickHouse has no way to know whether this
-- installation has one org or twenty, and a column DEFAULT cannot be
-- conditional. Historical rows therefore read as the default org.
--
-- This is not an inconsistency an operator can observe in one deployment:
-- exactly one score store is live at a time (cmd/nexus/compose.go prefers
-- ClickHouse and falls back to Postgres), so a given installation gets one rule
-- or the other, never both.
--
-- A multi-org installation upgrading with ClickHouse as its score store should
-- decide what to do with pre-migration history before granting console access to
-- a second org. To hide it from everyone:
--
--   ALTER TABLE eval_scores UPDATE org_id = 'unattributed'
--     WHERE org_id = 'default' AND timestamp < '<upgrade timestamp>';
--
-- Run out of band and watch system.mutations for progress; the cost is a rewrite
-- of every affected part. See docs/customer-self-hosted-upgrade-rollback.md.

ALTER TABLE eval_scores
    ADD COLUMN IF NOT EXISTS org_id String DEFAULT 'default';

-- ===== clickhouse/011_mcp_tool_logs.sql =====
-- MCP tool execution logs. High-volume analytics table separate from
-- gateway_traces so MCP-specific filters and joins stay cheap.

CREATE TABLE IF NOT EXISTS mcp_tool_logs
(
    id               String,
    timestamp        DateTime64(3),
    org_id           String,
    user_id          String,
    virtual_key_id   String,
    server_id        LowCardinality(String),
    server_label     LowCardinality(String),
    tool_name        LowCardinality(String),
    status           LowCardinality(String),
    latency_ms       Int64,
    cost_usd         Float64,
    llm_trace_id     String,
    session_id       String,
    turn_id          String,
    request_id       String,
    arguments        String,
    result           String,
    error_message    String,
    metadata         String
)
ENGINE = MergeTree
PARTITION BY toDate(timestamp)
ORDER BY (org_id, timestamp, id)
TTL toDateTime(timestamp) + INTERVAL 90 DAY;

-- ===== clickhouse/012_request_evidence.sql =====
-- Request evidence graph: persist provider attempt trails and typed policy
-- reasons on gateway_traces so a single trace_id can be exported and joined
-- to eval_scores. Columns are String JSON (empty default) so older binaries
-- keep writing without them.

ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS attempts String DEFAULT '';
ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS policy_reasons String DEFAULT '';
ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS guardrail_rule LowCardinality(String) DEFAULT '';
ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS egress_mode LowCardinality(String) DEFAULT '';

