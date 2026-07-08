-- Eval scores produced by async workers (Phase 3). Used when ClickHouse is
-- not configured but Postgres is (lighter deployments). Quality-aware routing
-- still prefers ClickHouse stats when available.
CREATE TABLE IF NOT EXISTS eval_scores (
    trace_id        TEXT NOT NULL,
    timestamp       TIMESTAMPTZ NOT NULL,
    evaluator       TEXT NOT NULL,
    metric          TEXT NOT NULL,
    score           DOUBLE PRECISION NOT NULL,
    passed          BOOLEAN NOT NULL,
    rationale       TEXT NOT NULL DEFAULT '',
    judge_model     TEXT NOT NULL DEFAULT '',
    user_id         TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_eval_scores_metric_ts ON eval_scores (metric, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_eval_scores_trace ON eval_scores (trace_id);
CREATE INDEX IF NOT EXISTS idx_eval_scores_user_ts ON eval_scores (user_id, timestamp DESC);
