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
