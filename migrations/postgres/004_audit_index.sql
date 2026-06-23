-- v1.1 audit log: index on actor for per-user audit queries.
-- The audit_log table itself (created in 001_init.sql) already has
-- actor TEXT NOT NULL DEFAULT 'system' and an (org_id, created_at) index.
-- Adding an index on actor so admin /api/audit?user_id=<id> filters stay
-- fast as the table grows. Old rows have actor = 'system' (the DB default)
-- and continue to scan fine via idx_audit_log_org.
CREATE INDEX IF NOT EXISTS idx_audit_log_actor ON audit_log(org_id, actor, created_at);