# BYOK + Multi-Tenancy Design (Per-User Provider Keys)

Status: **DRAFT — for review before implementation**
Author: design pass (Nexus)
Scope: enable many users, each registering their own provider API keys (BYOK),
calling through their own Nexus virtual key, billed to their own provider account.

---

## 1. Goal & Decision

**Goal (user-confirmed):** Each user registers their *own* OpenAI/Gemini/Anthropic
key and calls through it. Each user pays their own provider bill. The operator
(you) does not pay for users' usage.

This is **BYOK (Bring Your Own Key) + multi-tenancy**, not the LiteLLM/Bifrost
*default* (which is a shared central key). We make BYOK a first-class feature.

**What stays:** the virtual key (`nxs_live_...`) remains the credential a user
presents to Nexus — it authenticates the user and carries policy (allowed
models, RPM, budget). What changes: the **upstream provider key is now resolved
per request from the calling user's own stored credentials**, instead of a single
global key chosen at boot.

### Mental model

```
User A → presents vkey A → Nexus → looks up A's OpenAI key → calls OpenAI (A pays)
User B → presents vkey B → Nexus → looks up B's Gemini key → calls Gemini (B pays)
```

---

## 2. Current State (what exists today)

| Concern | Today | File |
|---|---|---|
| Tenancy axis | `org_id` hardcoded `"default"` everywhere | `internal/core/store.go`, `migrations/postgres/001_init.sql` |
| User entity | **none** | — |
| Virtual key | authenticates + policy (allowed_models/rpm/budget) | `internal/core/types.go`, `middleware.go` |
| Provider credentials | per-`org` only, encrypted (AES-256-GCM) | `provider_credentials` table, `store.go` |
| Provider registration | **once at boot**, global registry, `org="default"` | `cmd/nexus/main.go:registerStoredCredentials` |
| Upstream key selection | none per request — every request uses the same global provider instance | `internal/gateway/handler.go:resolveChain` / `registry.Resolve` |

**Key gap:** `Registry` maps `model → Provider`, and a `Provider` is constructed
with a *baked-in* API key at boot. There is no path to pick a key based on *who
is calling*.

---

## 3. Target Architecture

### 3.1 Data model changes (Postgres)

New migration `migrations/postgres/002_byok.sql`:

```sql
-- Users: a human/identity within an org. A virtual key belongs to a user.
CREATE TABLE IF NOT EXISTS users (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    email           TEXT NOT NULL,
    password_hash   TEXT NOT NULL,                 -- bcrypt
    role            TEXT NOT NULL DEFAULT 'member', -- 'admin' | 'member'
    -- Per-user toggle: when false, Nexus does not enforce monthly budget / RPM
    -- for this user's keys (provider's own limits still apply). User-controlled.
    enforce_limits  BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, email)
);

-- Console login sessions (opaque token hash → user).
CREATE TABLE IF NOT EXISTS user_sessions (
    token_hash  TEXT PRIMARY KEY,                  -- sha256 of the session token
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_user_sessions_user ON user_sessions(user_id);

-- Bind a virtual key to its owning user (nullable → existing keys keep working).
ALTER TABLE virtual_keys
    ADD COLUMN IF NOT EXISTS user_id TEXT REFERENCES users(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS idx_virtual_keys_user ON virtual_keys(user_id);

-- Provider credentials become ownable by a user (BYOK). When user_id is set,
-- the credential is private to that user; when null, it is an org-level
-- (shared/central) credential — preserves today's behavior.
ALTER TABLE provider_credentials
    ADD COLUMN IF NOT EXISTS user_id TEXT REFERENCES users(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS idx_provider_credentials_user ON provider_credentials(user_id);
```

Notes:
- All new columns are nullable → **zero-downtime, backward compatible**. Existing
  single-tenant deployments keep working (everything stays `org=default`,
  `user_id=null`).
- We keep `org_id` as the top-level boundary so future team/org features fit.

### 3.2 Credential resolution order (the core change)

On each chat request, after auth we know `org_id`, `user_id` (via vkey), and the
target `provider` (derived from the resolved model). We resolve the upstream key:

1. **User credential** — `provider_credentials` where `user_id = <caller>` and
   `provider = <p>` and `enabled`. (BYOK — the user's own key.)
2. *(optional, mode-dependent)* **Org credential** — `user_id IS NULL`,
   `org_id = <caller org>`. (shared/central fallback.)
3. *(optional, mode-dependent)* **System env key** — `NEXUS_OPENAI_API_KEY` etc.

The active fallback policy is controlled by config:

- `NEXUS_KEY_MODE=byok` → step 1 only. If the user has no key for that provider →
  `402`/`403` "no credential for provider X; register your key".
- `NEXUS_KEY_MODE=byok_fallback` → 1 → 2 → 3 (LiteLLM-style "both").
- `NEXUS_KEY_MODE=central` → 2 → 3 only (today's behavior; default for upgrades).

**User-confirmed target = `byok`.** We will default new installs to `byok` and
keep `central` as the back-compat default when no users exist.

### 3.3 Hot-path provider resolution (performance-critical)

Today `Registry.Resolve(model)` returns a boot-time provider. We introduce a
**per-caller provider resolver** without rebuilding providers on every request:

- New interface `CredentialResolver`:
  ```go
  // Resolve returns the upstream secret (+ base URL) for a provider on behalf
  // of a caller, honoring the configured key mode. Returns ErrNoCredential when
  // BYOK mode finds nothing.
  type CredentialResolver interface {
      Resolve(ctx context.Context, orgID, userID, provider string) (Credential, error)
  }
  type Credential struct { Secret, BaseURL string; Source string /* user|org|env */ }
  ```
- The `Registry` keeps doing `model → provider *type*` mapping (which provider
  serves a model), but provider **adapters become key-less**: the secret is passed
  per call.
  - Refactor `Provider.ChatCompletion(ctx, req)` → the provider reads the secret
    from `ctx` (injected by the handler) **or** we change the constructor pattern
    to a lightweight `provider.WithKey(secret, baseURL)` that returns a cheap
    request-scoped client. Adapters already hold an `*http.Client` (reusable) — we
    only swap the `Authorization` header per request, so **no new connections**.
- **Decryption cache:** decrypting a credential per request is wasteful. Add an
  in-memory LRU cache keyed by `credentialID@version` (or `userID:provider`) with
  TTL + invalidation on rotate/delete. AES-GCM decrypt is fast, but the cache
  avoids a Postgres round-trip on the hot path. Cache holds plaintext secrets in
  memory only (never logged, never serialized).

Latency budget: 1 Redis/PG lookup on cold cache, then memory hits. Comparable to
Bifrost's "in-memory configuration cache" approach.

### 3.4 Provider adapter refactor

Current adapters (`internal/providers/openai.go`, `gemini.go`, `anthropic.go`)
bake the key into the struct at `New*()`. Change to:

- Keep a shared, reusable transport/client per provider *type* (connection reuse).
- Accept the secret + base URL at call time (request-scoped header injection).

This is the largest code change. It must preserve streaming, timeouts, and the
existing retry/fallback chain in `handler.go:handleUnary` / `handleStream`.

### 3.5 Auth changes

`AuthResult` (in `middleware.go`) gains `UserID`. `makeAuthenticator` in
`main.go` fills it from the looked-up virtual key. New context key `ctxKeyUserID`.

---

## 4. Management API (console)

All under the existing console server (`internal/console/`). RBAC: a member can
only manage their *own* users-scoped resources; an admin manages the org.

### Auth (session login)
- `POST   /api/auth/login`       — `{email, password}` → sets session cookie
- `POST   /api/auth/logout`      — clears session
- `GET    /api/me`               — current user `{id, email, role, enforce_limits}`
- `PATCH  /api/me`               — update own settings, e.g. `{enforce_limits}`
  (the per-user budget/RPM on-off toggle)

### Users
- `POST   /api/users`            — create user (admin) `{email, role}`
- `GET    /api/users`            — list users in org (admin)
- `DELETE /api/users/{id}`       — remove user (admin)

### Virtual keys (extended)
- `POST   /api/keys`             — now accepts `user_id` (admin) or implies caller (self)
- `GET    /api/keys`             — scoped: members see own, admins see org

### Per-user provider credentials (BYOK)
- `POST   /api/me/credentials`   — register **my** provider key
  `{provider, name, base_url?, secret}`
- `GET    /api/me/credentials`   — list my credentials (last4 only)
- `POST   /api/me/credentials/{id}/rotate`
- `DELETE /api/me/credentials/{id}`

(Existing org-level `/api/credentials*` stays for admin/central mode.)

"Who am I" for `/api/me/*` is established by the presented virtual key (same
bearer auth as the gateway) or a console session — see Open Questions §7.

---

## 5. Dashboard UI (user chose "with UI")

Add to the React console (`web/src/`):

- **Settings → API Keys (mine):** register/rotate/delete my provider keys, show
  provider + last4 + status + rotated_at. Clear "your key, your bill" copy.
- **Admin → Users:** list/create/disable users, see per-user spend.
- **Usage:** per-user spend/RPM already partially exists via traces — add a
  per-user filter and a "credential source" column (user/org/env) so it's
  obvious which key served a request.

---

## 6. Backward compatibility & rollout

1. Migration adds only nullable columns + new table → safe on a running DB.
2. With no `users` and no per-user credentials, resolution falls through to
   org/env exactly as today (`central` default). **No behavior change for current
   single-tenant deployment.**
3. Flip `NEXUS_KEY_MODE=byok` only after at least one user has registered a key.
4. Provider adapter refactor is internal; external API (`/v1/chat/completions`)
   is unchanged.

---

## 7. Decisions (confirmed)

1. **Console identity = email + password login.** Real user accounts with
   sessions. Console gets a `/api/auth/login` (+ logout) issuing a session token
   (HTTP-only cookie). `/api/me/*` resolves the user from the session. Passwords
   are bcrypt-hashed. SSO/OIDC is a later phase.
2. **Per-user budget/RPM = user-toggleable.** Each user can turn their own
   Nexus-side monthly budget / RPM enforcement on or off (a per-user setting),
   even in BYOK. Off = no Nexus cap (provider's own limits apply); On = Nexus
   enforces the configured cap as a safety guardrail.
3. **Provider scope:** start with OpenAI + Gemini + Anthropic (current adapters),
   including routing-alias / load-balancer interaction with per-user keys.
4. **Secret exposure:** never return plaintext after creation (existing rule);
   decryption cache holds plaintext in memory only (never logged/serialized).

---

## 8. Implementation plan (phased, once design approved)

- **P1 — schema + resolver (no UI):**
  migration 002, `users` store methods, `user_id` on vkey/credential,
  `CredentialResolver` + decryption cache, wire `UserID` through auth.
- **P2 — provider adapter refactor:** key-less adapters + request-scoped secret;
  keep streaming/timeouts/fallback green (unit + e2e tests).
- **P3 — hot path:** handler resolves per-caller credential, `NEXUS_KEY_MODE`,
  error path for "no credential" + trace `credential_source`.
- **P4 — console API:** `/api/users`, `/api/me/credentials*`, RBAC scoping.
- **P5 — dashboard UI:** my-keys settings, admin users, usage credential-source.
- **P6 — docs + e2e:** runbook update, `scripts/test_byok.sh`, CI.

Each phase is independently shippable behind the nullable schema + key mode flag.

---

## 9. Competitive positioning (why BYOK must tie into eval/observability)

Direct review of Bifrost (`github.com/maximhq/bifrost`) confirms our strategy:

- Bifrost sells **raw speed** (11 µs overhead @ 5k RPS) and infra **observability**
  (Prometheus metrics, distributed tracing, logging). Its governance gives
  per-key *usage/budget* tracking.
- Crucially, **Bifrost has no in-gateway LLM quality eval** — quality evaluation
  is pushed to its parent SaaS (the `maxim` plugin). Many advanced features are
  enterprise-only.

Nexus's moat is the opposite: **eval + observability are first-class, built into
the OSS gateway** (Python sidecar with DeepEval/RAGAS, async scoring, ClickHouse
analytics, dashboard, CI regression gate). BYOK is table-stakes catch-up; to keep
our edge, BYOK must be designed *through* the eval/observability lens, not bolted
on:

1. **Per-user quality, not just per-user spend.** ✅ *Implemented.* Bifrost
   tracks "who spent what"; we additionally track **"what is this user's rolling
   quality score"**. `eval_scores.user_id` is denormalized from the trace
   (migration `005_eval_user.sql`, stamped by the eval worker), and
   `Reader.UserQualitySummary` aggregates quality + pass-rate joined with
   per-user spend. Surfaced at admin `GET /api/users/quality` and the console
   "Per-user quality" panel.
2. **BYOK × quality-SLA routing.** `virtual_keys.min_quality_score` already
   exists. With per-user keys, a user brings their own key *and* their own quality
   gate — the router (`ModelRouter.Rank`) honors it per caller. This is a combo
   Bifrost cannot offer without Maxim.
3. **`credential_source` as an observability signal.** Surface user/org/env in
   the trace and dashboard (already noted in §5), so operators can see BYOK
   adoption and isolate quality/cost per credential source.

Design rule: every new BYOK surface (API, trace field, dashboard panel) should
also carry the eval/quality dimension, reinforcing the differentiator rather than
just matching Bifrost's governance.

## 10. Risks

- **Hot-path regression:** per-request key resolution must not add meaningful
  latency → mitigated by in-memory decryption cache + reusable transports.
- **Adapter refactor blast radius:** touches all providers + streaming → cover
  with the existing e2e suite before/after.
- **Security:** more plaintext secrets in memory (cache) and more write APIs
  (per-user) → strict RBAC, audit every credential op (audit_log already exists).
