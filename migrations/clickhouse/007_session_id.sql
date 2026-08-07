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
