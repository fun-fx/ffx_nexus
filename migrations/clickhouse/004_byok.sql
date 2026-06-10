-- BYOK / multi-tenancy trace columns: attribute each request to its owning user
-- and record which upstream key served it (env / org / user), so the dashboard
-- can show per-user usage + quality and BYOK adoption.

ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS user_id LowCardinality(String) DEFAULT '';
ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS credential_source LowCardinality(String) DEFAULT '';
