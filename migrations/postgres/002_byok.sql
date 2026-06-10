-- Nexus BYOK + multi-tenancy schema (Postgres), additive and backward
-- compatible: every change is a new table or a nullable column, so existing
-- single-tenant deployments keep working unchanged (org stays "default",
-- user_id stays NULL, resolution falls back to org/env credentials).

-- Users: a human identity within an org. A virtual key may belong to a user,
-- and provider credentials may be owned by a user (BYOK).
CREATE TABLE IF NOT EXISTS users (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    email           TEXT NOT NULL,
    password_hash   TEXT NOT NULL,                 -- bcrypt
    role            TEXT NOT NULL DEFAULT 'member', -- 'admin' | 'member'
    -- Per-user toggle: when FALSE, Nexus does not enforce monthly budget / RPM
    -- for this user's keys (the provider's own limits still apply). The user
    -- controls this from their settings.
    enforce_limits  BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, email)
);

CREATE INDEX IF NOT EXISTS idx_users_org ON users(org_id);

-- Console login sessions: opaque token (sha256-hashed) -> user, with expiry.
CREATE TABLE IF NOT EXISTS user_sessions (
    token_hash  TEXT PRIMARY KEY,                  -- sha256 hex of the session token
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_user_sessions_user ON user_sessions(user_id);

-- Bind a virtual key to its owning user (nullable so existing keys keep working).
ALTER TABLE virtual_keys
    ADD COLUMN IF NOT EXISTS user_id TEXT REFERENCES users(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS idx_virtual_keys_user ON virtual_keys(user_id);

-- Provider credentials become ownable by a user (BYOK). When user_id is set the
-- credential is private to that user; when NULL it is an org-level (shared /
-- central) credential, preserving today's behavior.
ALTER TABLE provider_credentials
    ADD COLUMN IF NOT EXISTS user_id TEXT REFERENCES users(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS idx_provider_credentials_user ON provider_credentials(user_id);
