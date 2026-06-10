-- Per-user quality: denormalize the caller's user_id onto eval scores so the
-- dashboard can aggregate "this user's rolling quality score" without joining
-- back to gateway_traces. This is the eval/observability differentiator over
-- gateways that track only per-key spend (see docs/byok-multitenancy-design.md §9).
ALTER TABLE eval_scores ADD COLUMN IF NOT EXISTS user_id LowCardinality(String) DEFAULT '';
