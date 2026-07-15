-- v1.1 onboarding: mark when a user has finished the lightweight
-- "create your first provider key" flow so the UI can hide the banner.
--
-- Idempotent (IF NOT EXISTS just like the previous audit-actor migration
-- was kept idempotent in 004). Backfills NULL — every existing row is
-- treated as "not yet onboarded" which lets us demo the banner on the
-- legacy accounts in dev without writing timestamps.
ALTER TABLE users ADD COLUMN IF NOT EXISTS onboarded_at TIMESTAMPTZ NULL;
