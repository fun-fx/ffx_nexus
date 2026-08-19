-- c0.7 audit view/export indexes. The /api/audit view API supports
-- filters by org_id, action, actor, target, time range, request_id,
-- client_request_id. Each combination needs an index that satisfies
-- the WHERE clause without falling back to a full scan.
--
-- The view API always pins org_id (a tenant-isolation requirement)
-- so every index starts with org_id. created_at is the second column
-- because time-range queries are the most common.
--
-- Partial indexes are deliberately avoided here because the audit
-- table is append-only and small by total volume: every page in
-- /api/audit is a (org, time range) and Postgres picks the right
-- composite index.

CREATE INDEX IF NOT EXISTS idx_audit_log_org_action_time
    ON audit_log (org_id, action, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_audit_log_org_actor_time
    ON audit_log (org_id, actor, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_audit_log_org_target_time
    ON audit_log (org_id, target_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_audit_log_request_id
    ON audit_log (request_id)
    WHERE request_id <> '';

-- The export API uses cursor (created_at, id) pagination because
-- pure time-range pagination drifts on bursting traffic. The
-- composite index orders the cursor deterministically.
CREATE INDEX IF NOT EXISTS idx_audit_log_org_cursor
    ON audit_log (org_id, created_at DESC, id DESC);
