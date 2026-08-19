-- c0.x fix #2: the burst unique-index on (action, actor,
-- resource_fingerprint, first_at) omitted org_id, so a pre-auth denial
-- (actor = '') that falls back to "system" collapsed pre-auth denies
-- from different orgs into the same row. Two unrelated tenants sharing
-- a "system" burst row silently lost the per-org forensic detail.
--
-- The fix widens the key to include org_id. org_id is already in the
-- WHERE clause of every audit surface (org_id = $1) so widening does
-- not affect read path performance beyond a longer key. We DROP and
-- RECREATE so the partial-indexed form keeps its (count > 0) guard,
-- which is what makes the non-aggregated path's inserts pass through
-- without conflict.
--
-- ADD CONSTRAINT-style migrations on a hot table are not free; this
-- migration is small (the index is narrow on every burst row), runs
-- within a fraction of a second on a busy install, and the
-- application is double-writes safe because ON CONFLICT in code now
-- matches the wider key. The application code in store.go was updated
-- to write the wider key in the same PR; readers do not need changes
-- because the org_id column already exists.
DROP INDEX IF EXISTS audit_log_burst_key;

CREATE UNIQUE INDEX IF NOT EXISTS audit_log_burst_key
    ON audit_log (org_id, action, actor, resource_fingerprint, first_at)
    WHERE count > 0;
