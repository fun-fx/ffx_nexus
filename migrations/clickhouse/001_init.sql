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
