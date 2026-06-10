-- Dashboard columns: persist semantic-cache hits and inline guardrail actions so
-- the console can show cache-hit rate and guardrail activity over a time window
-- (previously these were only visible on the live trace feed).

ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS cache_hit UInt8 DEFAULT 0;
ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS guardrail_action LowCardinality(String) DEFAULT '';
