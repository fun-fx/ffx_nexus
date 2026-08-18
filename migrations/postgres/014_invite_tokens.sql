-- v1.2 invite tokens: admin-driven member onboarding without an SMTP relay.
--
-- A row is created by an admin via `POST /api/admin/invites`. The console
-- renders a shareable accept URL (`/invite/{token}`) that the admin passes
-- to the invitee out-of-band (Slack DM, ticket comment, etc.). When the
-- invitee visits the URL, `POST /api/invite/{token}/accept` swaps the token
-- for a real `users` row.
--
-- Tokens are stored as sha256 hashes so a leaked DB dump cannot be used to
-- hit the accept endpoint. The raw token is only ever returned once, at
-- creation time, in the create response (and re-surfaced on the invites
-- page via a "Copy link" action).
--
-- Idempotent for the additive shape so the same migration can be re-run
-- in environments where the prior step partially landed.
CREATE TABLE IF NOT EXISTS invite_tokens (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    email           TEXT NOT NULL,
    role            TEXT NOT NULL DEFAULT 'member',
    token_hash      TEXT NOT NULL UNIQUE,
    created_by      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,
    accepted_at     TIMESTAMPTZ NULL,
    accepted_by     TEXT NULL REFERENCES users(id) ON DELETE SET NULL,
    revoked_at      TIMESTAMPTZ NULL,
    UNIQUE (org_id, email)
);

CREATE INDEX IF NOT EXISTS idx_invite_tokens_org ON invite_tokens(org_id);
CREATE INDEX IF NOT EXISTS idx_invite_tokens_email ON invite_tokens(org_id, email);
