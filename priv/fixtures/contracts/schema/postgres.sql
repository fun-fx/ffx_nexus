-- Reconstructable empty-DB fixture for postgres.
-- Concatenation of migrations/{engine}/*.sql in ordinal order.
-- This is the schema Elixir must recreate. Do not invent a parallel ledger.
-- Generated for Phase 0; re-run scripts/dump_schema.sh after adding migrations.

-- ===== postgres/001_init.sql =====
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

-- ===== postgres/002_byok.sql =====
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

-- ===== postgres/003_sso.sql =====
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

-- ===== postgres/004_audit_index.sql =====
-- v1.1 audit log: index on actor for per-user audit queries.
-- The audit_log table itself (created in 001_init.sql) already has
-- actor TEXT NOT NULL DEFAULT 'system' and an (org_id, created_at) index.
-- Adding an index on actor so admin /api/audit?user_id=<id> filters stay
-- fast as the table grows. Old rows have actor = 'system' (the DB default)
-- and continue to scan fine via idx_audit_log_org.
CREATE INDEX IF NOT EXISTS idx_audit_log_actor ON audit_log(org_id, actor, created_at);

-- ===== postgres/005_credential_models.sql =====
-- User-defined credentials may include a custom model inventory when the
-- provider speaks the OpenAI wire format at a non-default base URL. Storing
-- the model ids per-credential lets the gateway expose them at /v1/models
-- without round-tripping to the upstream at startup. The column is JSONB so
-- we keep room for capability tags (chat / embed / moderation / image) as
-- the dynamic compat provider grows.
--
-- Shape: {"chat": ["gpt-x"], "embed": [...]}  — absent keys mean the
-- owner is not advertising that capability.
--
-- Backward compatible: existing rows stay valid because the default satisfies
-- every credential that did not declare a list (semantics: "use provider
-- defaults", which behaves the same as today).
ALTER TABLE provider_credentials
    ADD COLUMN IF NOT EXISTS models JSONB NOT NULL DEFAULT '{}'::jsonb;

-- ===== postgres/006_eval_scores.sql =====
-- Eval scores produced by async workers (Phase 3). Used when ClickHouse is
-- not configured but Postgres is (lighter deployments). Quality-aware routing
-- still prefers ClickHouse stats when available.
CREATE TABLE IF NOT EXISTS eval_scores (
    trace_id        TEXT NOT NULL,
    timestamp       TIMESTAMPTZ NOT NULL,
    evaluator       TEXT NOT NULL,
    metric          TEXT NOT NULL,
    score           DOUBLE PRECISION NOT NULL,
    passed          BOOLEAN NOT NULL,
    rationale       TEXT NOT NULL DEFAULT '',
    judge_model     TEXT NOT NULL DEFAULT '',
    user_id         TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_eval_scores_metric_ts ON eval_scores (metric, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_eval_scores_trace ON eval_scores (trace_id);
CREATE INDEX IF NOT EXISTS idx_eval_scores_user_ts ON eval_scores (user_id, timestamp DESC);

-- ===== postgres/007_eval_scores_model.sql =====
-- Denormalize request_model onto eval scores so Postgres-only deployments can
-- aggregate routing stats without gateway_traces (which live in ClickHouse).
ALTER TABLE eval_scores ADD COLUMN IF NOT EXISTS request_model TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_eval_scores_model_ts ON eval_scores (request_model, timestamp DESC);

-- ===== postgres/008_onboarded_at.sql =====
-- v1.1 onboarding: mark when a user has finished the lightweight
-- "create your first provider key" flow so the UI can hide the banner.
--
-- Idempotent (IF NOT EXISTS just like the previous audit-actor migration
-- was kept idempotent in 004). Backfills NULL — every existing row is
-- treated as "not yet onboarded" which lets us demo the banner on the
-- legacy accounts in dev without writing timestamps.
ALTER TABLE users ADD COLUMN IF NOT EXISTS onboarded_at TIMESTAMPTZ NULL;

-- ===== postgres/009_eval_plugins.sql =====
-- Eval plugins: user/declarative adapters that wrap an external
-- evaluation service (LangSmith, Langfuse, Datadog, Braintrust,
-- Arize, OTel, webhook, …) into a Nexus `external` evaluator kind.
--
-- Plugins are stored as the raw YAML so a future schema bump can be
-- shipped without a destructive migration: we re-validate on read.
-- The `enabled` column is the admin override (Helm-installed plugins
-- can be toggled without re-templating). `org_id` is empty for
-- cluster-wide plugin entries; populated for per-org customizations.
CREATE TABLE IF NOT EXISTS eval_plugins (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL DEFAULT '',
    name           TEXT NOT NULL,
    spec_yaml      TEXT NOT NULL,
    enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT eval_plugins_unique_name UNIQUE (org_id, name)
);

CREATE INDEX IF NOT EXISTS idx_eval_plugins_org ON eval_plugins (org_id);

-- Lightweight secret reference table. We don't store plaintext keys;
-- the row points at either a K8s Secret mounted into the gateway
-- pod or at the existing in-cluster eval_credentials store.
CREATE TABLE IF NOT EXISTS eval_plugin_secrets (
    plugin_id      TEXT NOT NULL REFERENCES eval_plugins(id) ON DELETE CASCADE,
    secret_kind    TEXT NOT NULL,                       -- 'k8s_secret' | 'keyref'
    secret_ref     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (plugin_id, secret_kind)
);

-- ===== postgres/010_eval_scores_kind.sql =====
-- Allow the dispatcher to segment legacy vs plugin-originated scores
-- in Postgres-only deployments. `evaluator` is already TEXT, so we add
-- a generated `kind` computed column for cheap GROUP BY. Existing rows
-- are mapped to 'legacy' or 'heuristic' based on prefix; the runtime
-- uses 'plugin:<name>' so we leave a `kind = 'plugin'` partition.
ALTER TABLE eval_scores
    ADD COLUMN IF NOT EXISTS kind TEXT
    GENERATED ALWAYS AS (
        CASE
            WHEN evaluator LIKE 'plugin:%' THEN 'plugin'
            WHEN evaluator IN ('heuristic_pii', 'heuristic_completeness') THEN 'heuristic'
            ELSE 'legacy'
        END
    ) STORED;

CREATE INDEX IF NOT EXISTS idx_eval_scores_kind_ts ON eval_scores (kind, timestamp DESC);

-- ===== postgres/011_eval_plugin_keys.sql =====
-- Durable home for the credentials pasted into the console's Plugin
-- Keys panel. Before this table the values lived in process memory
-- only, so every rolling update silently un-configured every plugin:
-- dispatch kept failing auth while the console still listed the
-- plugin as enabled.
--
-- Values are encrypted with the same AES-256-GCM master key that
-- protects provider_credentials, so a database dump carries no
-- plaintext vendor keys. `plugin` is the manifest's metadata.name
-- (the same token auth.secretRef resolves against) and `key_name` is
-- one entry of auth.keyRef, e.g. public_key / secret_key.
CREATE TABLE IF NOT EXISTS eval_plugin_keys (
    plugin            TEXT NOT NULL,
    key_name          TEXT NOT NULL,
    secret_ciphertext BYTEA NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (plugin, key_name)
);

-- ===== postgres/012_benchmark_runs.sql =====
-- Benchmark runs: model-level quality measurements executed by an
-- external eval platform (PrimeIntellect hosted evaluations today).
--
-- This is deliberately NOT an eval_plugins row. A plugin scores one
-- trace that already happened; a benchmark run asks a vendor to drive
-- a dataset against a model and report an aggregate. The input is a
-- model, not a trace, so spec.send.payload has nothing to render and
-- the per-trace dispatcher has nothing to dispatch.
--
-- Rows are the durable record of a run we started elsewhere:
-- external_id is the vendor's evaluation id, and status is polled
-- until it reaches a terminal value. Scores are stored raw rather
-- than folded into eval_scores because nothing consumes them for
-- routing yet — that wiring is a separate decision.
CREATE TABLE IF NOT EXISTS benchmark_runs (
    id                TEXT PRIMARY KEY,
    org_id            TEXT NOT NULL DEFAULT '',
    provider          TEXT NOT NULL DEFAULT 'primeintellect',
    -- Vendor-side identifier. Empty until the launch call returns, so a
    -- row can exist in 'failed' state for a launch that never started.
    external_id       TEXT NOT NULL DEFAULT '',
    name              TEXT NOT NULL DEFAULT '',
    -- Hub slugs such as 'primeintellect/gsm8k'. Stored as text rather
    -- than a join table: the vendor owns the catalogue and there is no
    -- environments API to reference, so these are opaque to us.
    environments      TEXT[] NOT NULL DEFAULT '{}',
    model             TEXT NOT NULL,
    num_examples      INTEGER NOT NULL DEFAULT 5,
    rollouts          INTEGER NOT NULL DEFAULT 1,
    -- TRUE when the vendor was told to send inference through this
    -- Nexus gateway, which is what makes the score describe what we
    -- actually serve (routing, cache and provider choice included)
    -- rather than the vendor's own serving of the same model.
    via_gateway       BOOLEAN NOT NULL DEFAULT TRUE,
    -- Virtual key minted for that gateway access, kept so the run can
    -- revoke it once the vendor no longer needs to call us.
    vkey_id           TEXT NOT NULL DEFAULT '',
    -- Our lifecycle: pending | running | completed | failed | cancelled.
    status            TEXT NOT NULL DEFAULT 'pending',
    -- The vendor's raw status string, preserved because their state
    -- machine is richer than ours (PROCESSING, TIMEOUT, …) and we do
    -- not want to lose detail when collapsing it.
    external_status   TEXT NOT NULL DEFAULT '',
    avg_score         DOUBLE PRECISION,
    min_score         DOUBLE PRECISION,
    max_score         DOUBLE PRECISION,
    total_samples     INTEGER,
    metrics           JSONB,
    viewer_url        TEXT NOT NULL DEFAULT '',
    error             TEXT NOT NULL DEFAULT '',
    created_by        TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at        TIMESTAMPTZ,
    completed_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_benchmark_runs_org_created
    ON benchmark_runs (org_id, created_at DESC);

-- The poller scans for rows that have not settled yet. Partial index
-- so the scan cost stays flat as completed history accumulates.
CREATE INDEX IF NOT EXISTS idx_benchmark_runs_unsettled
    ON benchmark_runs (updated_at)
    WHERE status IN ('pending', 'running');

-- ===== postgres/013_scheduled_benchmarks.sql =====
-- 013_scheduled_benchmarks.sql
-- Operator-defined recurring benchmark launches. Drives the cron runner
-- in internal/cron. Each row is the persistent operator intent; the runner
-- reads enabled rows whose next_launch_at has passed, fires them, and
-- updates the schedule on success.

CREATE TABLE IF NOT EXISTS benchmark_schedules (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL DEFAULT '',
    name            TEXT NOT NULL DEFAULT '',
    environments    TEXT[] NOT NULL DEFAULT '{}',
    model           TEXT NOT NULL,
    num_examples    INTEGER NOT NULL DEFAULT 5,
    rollouts        INTEGER NOT NULL DEFAULT 1,
    via_gateway     BOOLEAN NOT NULL DEFAULT TRUE,
    cadence_seconds INTEGER NOT NULL,
    next_launch_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    created_by      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Only enabled schedules are scanned by the cron tick. The partial index
-- keeps the scan cheap as the table grows beyond a handful of rows.
CREATE INDEX IF NOT EXISTS idx_benchmark_schedules_due
    ON benchmark_schedules (next_launch_at)
    WHERE enabled = TRUE;

-- Backfill the schedule_id link so existing benchmark_runs remain
-- traceable once scheduled launches settle. Default '' means "manual",
-- preserving the historical default.
ALTER TABLE benchmark_runs
    ADD COLUMN IF NOT EXISTS schedule_id TEXT NOT NULL DEFAULT '';

-- ===== postgres/014_invite_tokens.sql =====
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

-- ===== postgres/015_eval_scores_org.sql =====
-- 015: attribute eval scores to an organization.
--
-- eval_scores recorded trace_id and user_id but never org_id, so every read of
-- evaluation quality was installation-wide. A single-customer deployment still
-- divides teams and departments into orgs, and a score row carries the metric,
-- the rationale text and the judged model — so one team's admin could read the
-- quality and safety findings for another team's traffic, including the
-- rationale strings a judge model wrote about their prompts.
--
-- Additive, per the expand/contract rule in migrations/README.md:
--
--   * The column is NOT NULL with a DEFAULT. The default — not nullability — is
--     what keeps version N of the binary working against this schema: N's
--     INSERT names its columns and omits org_id, so Postgres supplies 'default'
--     and the write succeeds. Rolling the application back therefore needs no
--     schema change. (NOT NULL is the stronger choice for the same
--     compatibility: it makes "a new row with no org" unrepresentable rather
--     than merely discouraged, so there is no NULL case for reads to handle.)
--   * No row is deleted and no column is dropped.
--
-- BACKFILL RULE. Attribution of pre-migration rows depends on how many orgs the
-- installation actually uses, because "put it in the default org" is a correct
-- statement about a single-org deployment and a guess about any other.
--
-- "Uses" is measured as the number of distinct orgs that own a user, NOT the
-- number of rows in `organizations`. 001_init.sql unconditionally seeds an org
-- called 'default', so the table never holds fewer than one row and a customer
-- who created their own org and moved everyone into it would look multi-org by
-- that measure — and their own history would be withheld from them for nothing.
--
--   * One org in use: every historical row belongs to it. Not an inference —
--     there was nowhere else for the traffic to have come from.
--   * More than one: rows are attributed from the score's own user where that
--     user still exists, which is a real signal rather than a guess. Rows with
--     no usable user (org-level or legacy traffic) are parked in
--     'unattributed', a scope no org's reads match, so they surface to nobody
--     until an operator decides where they belong. Reclaiming them is a
--     deliberate step documented in docs/customer-self-hosted-integrations.md;
--     it is not something a migration should decide on an operator's behalf.
--   * No users at all: an API-key-only installation, where the gateway stamps
--     'default' on live traffic anyway. Historical rows keep that same value,
--     so old and new data agree.

ALTER TABLE eval_scores
    ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT 'default';

DO $$
DECLARE
    orgs_in_use integer;
    sole_org    text;
BEGIN
    SELECT count(DISTINCT org_id) INTO orgs_in_use FROM users;

    IF orgs_in_use = 1 THEN
        SELECT DISTINCT org_id INTO sole_org FROM users;
        -- Skipped when the sole org is already literally 'default', where the
        -- ADD COLUMN default has put them.
        IF sole_org IS DISTINCT FROM 'default' THEN
            UPDATE eval_scores SET org_id = sole_org WHERE org_id = 'default';
        END IF;

    ELSIF orgs_in_use > 1 THEN
        -- Attribute from the owning user. Bounded to rows still sitting at the
        -- ADD COLUMN default so a re-run cannot overwrite an attribution a
        -- later insert set correctly.
        UPDATE eval_scores es
        SET org_id = u.org_id
        FROM users u
        WHERE es.user_id = u.id
          AND es.user_id <> ''
          AND es.org_id = 'default'
          AND u.org_id <> 'default';

        -- Whatever is left at 'default' and has no matching user cannot be
        -- attributed. Park it out of every org's reach. Rows whose user IS a
        -- member of an org genuinely named 'default' are excluded by the
        -- EXISTS check and correctly stay put.
        UPDATE eval_scores es
        SET org_id = 'unattributed'
        WHERE es.org_id = 'default'
          AND NOT EXISTS (SELECT 1 FROM users u WHERE u.id = es.user_id);
    END IF;
END
$$;

-- Reads are always "this org, this metric, over this window" and "this org,
-- this trace", so org_id leads both indexes. The pre-existing indexes stay:
-- dropping them would slow the scheduler's cross-org sweeps, which legitimately
-- query without an org filter.
CREATE INDEX IF NOT EXISTS idx_eval_scores_org_metric_ts
    ON eval_scores (org_id, metric, timestamp DESC);

CREATE INDEX IF NOT EXISTS idx_eval_scores_org_trace
    ON eval_scores (org_id, trace_id);

-- ===== postgres/016_benchmark_schedule_last_run.sql =====
-- Add the last-run tracking columns that internal/core/benchmark_schedules.go
-- has always read but 013_scheduled_benchmarks.sql never created.
--
-- Every SELECT in that file lists last_run_id and last_launched_at, and
-- MarkScheduleLaunched UPDATEs them. On any database built from the migration
-- set, all of those statements fail with SQLSTATE 42703 (undefined column).
-- Benchmark schedules can be created, but reading one back, listing them, and
-- recording a launch are all broken on a fresh install.
--
-- This is the same defect class as the missing invite_tokens table: code and
-- schema diverged because no test exercised the pair against a database built
-- the way a customer's is. It went unnoticed because
-- internal/core/benchmark_schedules_test.go is not named *Integration and so was
-- not selected by CI's `go test ./internal/core/ -run Integration` step, while
-- the default `go test ./...` skips it for want of NEXUS_TEST_POSTGRES_URL. CI
-- now runs the whole package against a real Postgres.
--
-- Additive and idempotent: ADD COLUMN IF NOT EXISTS is a catalogue-only
-- operation here because both columns are nullable with no default, so it does
-- not rewrite the table and is safe on a live database.
--
-- Both columns are NULLABLE on purpose. A schedule that has never launched has
-- no run id and no launch time, and NULL states that correctly. A sentinel like
-- the empty string or the epoch would make "never ran" indistinguishable from
-- "ran and we lost the value", and the scanner uses NULL-ness to decide whether
-- a schedule is due for its first launch.

ALTER TABLE benchmark_schedules
    ADD COLUMN IF NOT EXISTS last_run_id TEXT;

ALTER TABLE benchmark_schedules
    ADD COLUMN IF NOT EXISTS last_launched_at TIMESTAMPTZ;

-- The scheduler asks "which enabled schedules are due", ordering by
-- next_launch_at; the existing partial index already serves that. This index
-- serves the console's per-org list, which sorts by most recently launched and
-- would otherwise sort the whole table.
CREATE INDEX IF NOT EXISTS idx_benchmark_schedules_org_last_launched
    ON benchmark_schedules (org_id, last_launched_at DESC NULLS LAST);

-- ===== postgres/017_audit_request_id.sql =====
-- Add the request_id column to audit_log so an HTTP response, the server
-- log line, and the audit row are all joinable by a single id.
--
-- The customer-facing path renders one id in the X-Request-Id header and in
-- the response body (resp.HTTP sets it once per request). resp.HTTP also
-- logs the same id with the cause. If a state-changing console action runs
-- inside the request, the audit log must carry the same id so support can
-- join the three: customer tickets the response code -> asks for the id
-- -> support greps the server log by id -> sees the audit row by id.
--
-- Without request_id on the audit row, the join is by time + action + actor,
-- which fails the moment a high-volume org produces two audit rows in the
-- same minute. The id is constant within an HTTP request and is the only
-- field that uniquely ties the audit to the response.
--
-- ALTER ... ADD COLUMN IF NOT EXISTS is idempotent: a re-run on a partial
-- migration is a catalogue-only change and adds nothing. New rows insert
-- request_id explicitly; old rows keep request_id = '' because the column
-- is NULL-tolerant.
--
-- Phase D-1 will add an index on (request_id) once auditors routinely join
-- by id; the index is deferred because Phase E introduces role-split reads
-- which is when query patterns stabilise.
ALTER TABLE audit_log
    ADD COLUMN IF NOT EXISTS request_id TEXT NOT NULL DEFAULT '';

-- ===== postgres/018_audit_client_request_id.sql =====
-- Add client_request_id (separate column) so customer-supplied X-Request-Id
-- values survive into the row for forensics WITHOUT contaminating the
-- server-generated join key. c0.1 enforces this separation: the auditid
-- package writes the server-generated id (prefix "req-" / "job-") to
-- request_id, and the sanitised client header to client_request_id.
--
-- We do NOT use the header value as the join key — clients can put
-- anything in the header and we don't want id collisions or log-injection
-- vectors — but support sometimes needs it to reconcile a customer's
-- report ("my X-Request-Id was abc-123") with our server-generated id.
--
-- The auditid package writes "" whenever the header is empty, too long,
-- or contains characters outside [A-Za-z0-9._-]{1,128}. Reading "" plus
-- the server-generated request_id still gives support a single-grep path
-- back to the response.
--
-- Column rationale for NOT indexing: client_request_id is for forensics,
-- not for cardinality-bound queries. An index here would invite an org
-- key flood that mirrors the bug class b1.2 / 016_invite_tokens is
-- designed to defend against.
ALTER TABLE audit_log
    ADD COLUMN IF NOT EXISTS client_request_id TEXT NOT NULL DEFAULT '';

-- ===== postgres/019_audit_aggregation.sql =====
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

-- ===== postgres/020_audit_roles.sql =====
-- c0.4 Phase E role separation: split the four roles that interact with
-- audit_log so the append-only contract is enforceable by Postgres
-- permissions rather than only by application code.
--
-- The role boundaries aren't auto-applied: Nexus deployments use a
-- single DB account in the typical self-hosted case, and applying
-- GRANTs that assume a superuser is on hand would break the start-up
-- flow. The statements below are shipped as documentation and as the
-- canonical reconciliation a DBA applies in Phase E to harden an
-- installation where SU credentials are available.
--
-- Roles:
--   nexus_migration  — only used during migrate.Run. INSERT/UPDATE
--                      the schema_migrations ledger. SELECT/DELETE on
--                      the legacy schema_migrations row needed by
--                      numbered-migration maintenance. No rights on
--                      audit_log beyond SELECT for back-fill checks.
--   nexus_app        — the application runtime. INSERT only on
--                      audit_log (no UPDATE / DELETE). SELECT on
--                      audit_log to render the audit feed and export.
--   nexus_audit_read — analytics, read-only. SELECT only.
--   nexus_audit_purge — retention cleanup. DELETE only with a
--                      time-bounded predicate (`created_at < now() -
--                      interval N days`) refused by a CHECK constraint
--                      on the role privilege; a SQL safeguard function
--                      is created below to enforce this.
--
-- IMPORTANT: This script is conditional. The GRANTs only succeed
-- against a Postgres role that exists. The application installer
-- (cmd/nexus/main.go) does NOT auto-create these roles; the operator
-- provisions them via Helm secrets documented in docs/audit-log-roles.md
-- before a hardened install.

-- Migration user is allowed to bootstrap DDL but not to write audit_log.
-- The application's auditor identity is granted minimally on the table.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_migration') THEN
        GRANT SELECT, INSERT, UPDATE ON schema_migrations TO nexus_migration;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_app') THEN
        -- Append-only on audit_log. SELECT permitted for /api/audit.
        GRANT SELECT, INSERT ON audit_log TO nexus_app;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_audit_read') THEN
        GRANT SELECT ON audit_log TO nexus_audit_read;
    END IF;
END $$;

-- The retention cleanup relies on the `nexus_audit_purge_rows` SQL
-- function rather than raw DELETE privileges, because Postgres doesn't
-- expose a "DELETE only inside this WHERE clause" grant. The function
-- is SECURITY DEFINER and only accepts a time argument; it raises an
-- exception if `older_than` is passed as zero / negative.
CREATE OR REPLACE FUNCTION nexus_audit_purge_rows(older_than interval)
RETURNS integer
LANGUAGE plpgsql
AS $$
DECLARE
    deleted_count integer;
BEGIN
    IF older_than IS NULL OR older_than < interval '1 hour' THEN
        RAISE EXCEPTION 'audit purge refuses interval shorter than 1 hour; received %',
            older_than;
    END IF;
    DELETE FROM audit_log
     WHERE created_at < NOW() - older_than
       AND (count > 0) = FALSE; -- always-on truth: retain aggregated rows unconditionally
    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END $$;

-- The cleanup role is restricted to EXECUTE on the function; UPDATE /
-- DELETE on the table directly is revoked (not granted) so direct
-- DELETE calls fail with "permission denied". This implements the
-- "single cleanup path" protection c0.4 asked for.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_audit_purge') THEN
        GRANT EXECUTE ON FUNCTION nexus_audit_purge_rows(interval) TO nexus_audit_purge;
        REVOKE DELETE, UPDATE ON audit_log FROM nexus_audit_purge;
    END IF;
END $$;

-- ===== postgres/021_audit_view_indexes.sql =====
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

-- ===== postgres/022_mcp_servers.sql =====
-- MCP server registry: operator-declared tool servers the gateway proxies.
-- spec_yaml holds connection details (stdio/http), auth headers, and
-- optional virtual-key allow lists. Re-validated on read like eval_plugins.

CREATE TABLE IF NOT EXISTS mcp_servers (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL DEFAULT '',
    name           TEXT NOT NULL,
    spec_yaml      TEXT NOT NULL,
    enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT mcp_servers_unique_name UNIQUE (org_id, name)
);

CREATE INDEX IF NOT EXISTS idx_mcp_servers_org ON mcp_servers (org_id);

-- ===== postgres/023_mcp_org_settings.sql =====
-- Per-org MCP defaults applied when installing from the Library or creating servers.

CREATE TABLE IF NOT EXISTS mcp_org_settings (
    org_id              TEXT PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    default_timeout_ms  INT NOT NULL DEFAULT 60000,
    default_sticky_http BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

