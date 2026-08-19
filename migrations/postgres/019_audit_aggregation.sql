-- c0.3 audit aggregation: extend audit_log so a single row can record the
-- burst outcome of identical denied-attempts (same actor + same reason +
-- same target fingerprint) instead of writing one row per request. Adding
-- count, first_at, last_at is non-destructive: existing rows keep
-- count=0 and NULL timestamps because the columns have DEFAULT.
--
-- Aggregation policy (see c0.3 in docs/audit-action-constants.md):
--
--   - Aggregated (row per actor+reason+resource per window): auth.login.denied,
--     user.login.denied, rate_limited, request_too_large. Row collapses the
--     burst and stores count + first_at + last_at.
--
--   - Individual (row per occurrence): everything else (origin/CORS/egress,
--     org_boundary, audit_view_denied, secure paths).
--
-- resource_fingerprint is a length-bounded digest (default SHA256 first
-- 16 hex chars) of the request-target so two rows that share
-- (actor, reason, resource_fingerprint) within a 5-minute window merge.
-- The fingerprint lives in the table for forensic reproducibility; it is
-- NOT exposed on the operator-facing audit feed (admin sees target_id +
-- count instead).
ALTER TABLE audit_log
    ADD COLUMN IF NOT EXISTS count          INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS first_at       TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_at        TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS resource_fingerprint TEXT NOT NULL DEFAULT '';

-- The dedup-key index collapses bursts in c0.3. We do NOT cover the full
-- unique column set (action, actor, org_id) — an aggregation window is
-- scoped by action + actor + resource_fingerprint, with org_id redundant
-- because actor is already org-scoped.
--
-- A Postgres UNIQUE constraint over (action, actor, resource_fingerprint,
-- first_at) compiles to a btree that powers both UPSERT and the
-- /api/audit?action=X&actor=Y query path. Partial-indexing to elide
-- count=0 rows would be premature optimisation — the index is cheap and
-- keeps analysis queries simple.
--
-- Migration-time CHECK of pre-existing rows is non-trivial; a future
-- duplicate aggregate row (action, actor, resource_fingerprint, first_at)
-- would only happen if the production column values span multiple rows
-- across the upserts, in which case the SQL ON CONFLICT clause handles
-- the merge. The index is therefore a true ON CONFLICT target, not a
-- semantic constraint on past data.
CREATE UNIQUE INDEX IF NOT EXISTS audit_log_burst_key
    ON audit_log (action, actor, resource_fingerprint, first_at)
    WHERE count > 0;
