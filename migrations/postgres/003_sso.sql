-- Nexus SSO (OIDC) identity binding.
-- Additive: existing email/password users keep working unchanged. The new
-- columns are populated lazily when a user signs in via SSO for the first
-- time and the email matches an existing row (link) or a new user is
-- provisioned (JIT). The unique partial index on (org, provider, subject)
-- is what lets us detect re-binding attempts at the DB level.

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS sso_subject  TEXT,
    ADD COLUMN IF NOT EXISTS sso_provider TEXT,
    ADD COLUMN IF NOT EXISTS sso_issuer   TEXT;

-- A user may be bound to exactly one (provider, subject) pair per org. The
-- partial index keeps it a no-op for rows that have not signed in via SSO.
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_sso_identity
    ON users(org_id, sso_provider, sso_subject)
    WHERE sso_subject IS NOT NULL;
