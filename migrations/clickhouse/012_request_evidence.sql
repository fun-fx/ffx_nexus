-- Request evidence graph: persist provider attempt trails and typed policy
-- reasons on gateway_traces so a single trace_id can be exported and joined
-- to eval_scores. Columns are String JSON (empty default) so older binaries
-- keep writing without them.

ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS attempts String DEFAULT '';
ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS policy_reasons String DEFAULT '';
ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS guardrail_rule LowCardinality(String) DEFAULT '';
ALTER TABLE gateway_traces ADD COLUMN IF NOT EXISTS egress_mode LowCardinality(String) DEFAULT '';
