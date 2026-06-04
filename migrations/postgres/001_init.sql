-- Nexus control-plane schema (Postgres).
-- Holds tenancy, virtual keys, provider credentials, and budgets.

CREATE TABLE IF NOT EXISTS organizations (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Seed a default org so single-tenant setups work out of the box.
INSERT INTO organizations (id, name)
VALUES ('default', 'Default')
ON CONFLICT (id) DO NOTHING;

-- Virtual keys: the credential apps present to the gateway. We store only a
-- hash of the key, never the plaintext. The key is shown once at creation.
-- Beyond auth, a virtual key is the tenancy axis that observability, evals, and
-- routing policy bind to.
CREATE TABLE IF NOT EXISTS virtual_keys (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    key_hash        TEXT NOT NULL,            -- sha256 hex of the presented key
    key_prefix      TEXT NOT NULL,            -- e.g. "nxs_live_ab12" for display
    key_last4       TEXT NOT NULL,            -- last 4 chars for display
    allowed_models  TEXT[] NOT NULL DEFAULT '{}',  -- empty = all models allowed
    rpm_limit       INTEGER NOT NULL DEFAULT 0,    -- requests/min, 0 = unlimited
    monthly_budget_usd  DOUBLE PRECISION NOT NULL DEFAULT 0,  -- 0 = unlimited
    -- Quality SLA: if set, route up to higher-quality models when rolling eval
    -- score for this key falls below the threshold (Phase 4).
    min_quality_score   DOUBLE PRECISION NOT NULL DEFAULT 0,
    revoked         BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_virtual_keys_hash ON virtual_keys(key_hash);
CREATE INDEX IF NOT EXISTS idx_virtual_keys_org ON virtual_keys(org_id);

-- Provider credentials: upstream API keys (OpenAI/Anthropic/Gemini/...).
-- Secrets are stored encrypted (envelope encryption); plaintext is never
-- returned after creation. Only last4 is shown.
CREATE TABLE IF NOT EXISTS provider_credentials (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    provider        TEXT NOT NULL,            -- "openai" | "anthropic" | "gemini"
    name            TEXT NOT NULL,            -- human label
    base_url        TEXT NOT NULL DEFAULT '', -- optional override (e.g. OpenAI-compatible)
    secret_ciphertext   BYTEA NOT NULL,       -- AES-256-GCM ciphertext (nonce-prefixed)
    secret_last4    TEXT NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    rotated_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_provider_credentials_org ON provider_credentials(org_id);

-- Audit log for credential/key lifecycle events.
CREATE TABLE IF NOT EXISTS audit_log (
    id          BIGSERIAL PRIMARY KEY,
    org_id      TEXT NOT NULL,
    actor       TEXT NOT NULL DEFAULT 'system',
    action      TEXT NOT NULL,                -- e.g. "vkey.create", "credential.rotate"
    target_id   TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_audit_log_org ON audit_log(org_id, created_at);
