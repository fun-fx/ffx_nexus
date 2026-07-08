-- Denormalize request_model onto eval scores so Postgres-only deployments can
-- aggregate routing stats without gateway_traces (which live in ClickHouse).
ALTER TABLE eval_scores ADD COLUMN IF NOT EXISTS request_model TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_eval_scores_model_ts ON eval_scores (request_model, timestamp DESC);
