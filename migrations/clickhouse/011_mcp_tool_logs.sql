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
