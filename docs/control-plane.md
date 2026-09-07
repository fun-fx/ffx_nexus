# Control plane: keys, credentials and tenancy

All of this needs Postgres. `nexus serve --local` runs one for you; a
cluster points `NEXUS_POSTGRES_URL` at a managed instance.

## Keys and credentials

When `NEXUS_POSTGRES_URL` is set, Nexus enables the control plane. Two key types,
managed separately:

- **Virtual keys** (apps -> gateway): stored as SHA-256 hashes, shown once at
  creation. They are the tenancy axis that observability, evals, and routing
  policy bind to (allowed models, RPM limit, monthly budget, quality SLA).
- **Provider credentials** (gateway -> OpenAI/Anthropic/Gemini): encrypted at
  rest with AES-256-GCM under `NEXUS_MASTER_KEY`. Plaintext is never returned
  after creation (only `last4`). Inject the master key from a secret manager in
  production; rotate to re-wrap.

```bash
# enable control plane
docker compose -f deploy/docker-compose.yml up -d postgres   # host port 5433
export NEXUS_POSTGRES_URL="postgres://nexus:nexus@localhost:5433/nexus?sslmode=disable"
export NEXUS_MASTER_KEY="$(openssl rand -hex 32)"   # persist this; needed to decrypt
go run ./cmd/nexus

# create a virtual key (secret returned once)
curl -s -X POST localhost:8081/api/keys \
  -d '{"name":"my-app","allowed_models":["gemini-2.5-flash"],"rpm_limit":100}'

# call the gateway with the virtual key
curl -s localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer nxs_live_..." \
  -d '{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}'

# register an upstream provider key (encrypted at rest)
curl -s -X POST localhost:8081/api/credentials \
  -d '{"provider":"openai","name":"prod","secret":"sk-..."}'

# rotate a provider key in place (re-encrypted; same credential id)
curl -s -X POST localhost:8081/api/credentials/<id>/rotate \
  -d '{"secret":"sk-new-..."}'
```

**Credential rotation** swaps the stored secret without recreating the
credential: the new secret is re-encrypted under `NEXUS_MASTER_KEY`, the
credential keeps its id/provider/name (so references stay valid), `rotated_at`
is recorded, and the audit log captures a `credential.rotate` event. The gateway
**hot-reloads** the affected provider so the new key takes effect without a
restart. Returns the updated metadata only (never the plaintext).

Without Postgres, the gateway runs in zero-dependency mode: no key enforcement,
provider keys read from env.

### Automated integration tests

```bash
docker compose -f deploy/docker-compose.yml up -d postgres clickhouse redis
go build -o bin/nexus ./cmd/nexus
./scripts/test_all.sh
```

The full suite runs four scripts (~40+ cases):

| Script | Coverage |
| --- | --- |
| `test_phase2.sh` | Virtual keys, 401/403, encrypted credentials, audit log, DB reload on restart, rotation (hot-reload), revoke/delete |
| `test_phase234.sh` | Rate limits (429), budgets (402), async evals, streaming, quality-aware routing |
| `test_eval_routing.sh` | `min_quality_score`, `eff_quality` stats, provider fallback |
| `test_zero_dep.sh` | Gateway without Postgres/ClickHouse/Redis (env keys only) |
| `test_guardrails.sh` | Inline guardrails: PII/deny-pattern/length input blocking |
| `test_schema_guardrails.sh` | Schema/JSON output guardrail: wiring + live JSON roundtrip |
| `test_self_correction.sh` | Structured-output self-correction: startup wiring |
| `test_lb_cache.sh` | Route load balancing + semantic cache wiring and cache hit |
| `test_eval_service.sh` | External Python eval service: contract, wiring, failure isolation |
| `test_eval_batch.sh` | Offline regression eval batch: aggregation + baseline regression gate |
| `test_eval_persistence.sh` | Live completion → remote eval → ClickHouse (skips without provider key) |
| `test_rag_eval.sh` | RAG `nexus_eval` context → eval sidecar contract |
| `test_byok.sh` | BYOK + multi-tenancy: login/session, self-service keys/credentials, budget toggle, admin user management, RBAC |

Run a single phase: `./scripts/test_phase2.sh`, `./scripts/test_phase234.sh`, etc.

Upstream completion tests need `GEMINI_API_KEY` or `OPENAI_API_KEY` in `.env`.
If the provider quota is exhausted, those cases are **skipped** (not failed) so
local runs stay green; re-run after quota resets for full coverage.

### Control plane API

- `GET/POST /api/keys`, `DELETE /api/keys/{id}` — virtual keys
- `GET/POST /api/credentials`, `POST /api/credentials/{id}/rotate`, `DELETE /api/credentials/{id}` — provider secrets

## BYOK & multi-tenancy

Nexus supports a **Bring-Your-Own-Key** model: each user signs in to the console,
registers their *own* OpenAI/Anthropic/Gemini key, and gateway calls go out on
that key — so every user pays their own provider bill, while Nexus still owns the
parts that are its moat: **per-user observability, quality evals, routing, and
guardrails**. This is the key difference from Bifrost/LiteLLM, which track per-key
*spend* but push LLM quality eval to an external SaaS.

### How key resolution works (`NEXUS_KEY_MODE`)

Upstream provider keys are resolved per request, in precedence order:

1. the **caller's** own stored credential (BYOK), then
2. the **org-level** credential, then
3. the process **env** key.

| `NEXUS_KEY_MODE` | Behavior |
| --- | --- |
| `strict_byok` *(default since v0.1.0)* | Require a per-user key; reject callers without one. The operator never pays for user usage. |
| `byok` | Prefer the caller's own key; fall back to org → env. |
| `shared` | Legacy: everyone uses the org/env key. No per-user keys. |

BYOK modes need Postgres + `NEXUS_MASTER_KEY`; otherwise Nexus falls back to
`shared`. The resolved key never touches logs; the trace records only its
**source** (`user` / `org` / `env`) so operators can see BYOK adoption and isolate
quality/cost per credential source.

### Opt-in shared-key fallback (`NEXUS_ALLOW_SHARED_KEYS`)

By default in v0.1.0+, the **env-provided** provider keys (`OPENAI_API_KEY`,
`ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `GRID_API_KEY`, …) are loaded into the
process for visibility but never reach the data path — every gateway call goes
out on the caller's own stored credential. To re-enable the legacy "process owners
the bill" behavior, set:

```
NEXUS_ALLOW_SHARED_KEYS=true
```

When set, the env keys are registered as a fallback in any `NEXUS_KEY_MODE`. When
unset (the default), Nexus logs a single `env provider key present but unused under
strict-byok default` warning per provider at startup so operators can see exactly
which keys are present but inert, and route statistics are kept free of shadow
env-key traffic.

### Dynamic model catalog sync (`NEXUS_DYNAMIC_MODEL_SYNC`)

Off by default. When enabled, a per-provider background worker periodically calls
that provider's upstream `/v1/models` endpoint (`OPENAI_BASE_URL/models`,
`ANTHROPIC_BASE_URL/models`, `https://generativelanguage.googleapis.com/v1beta/models?key=…`)
and rewrites the registry's `byModel` index with the response. The mock experiment
at the start of this README (`/v1/models`) keeps reflecting new OpenAI / Gemini /
Anthropic releases without a NexUS redeploy.

- **Latency impact**: zero on the hot path. The worker is a single goroutine per
  provider that takes the registry lock only for a slice-swap, while requests
  take the read lock and copy the slice.
- **Failure handling**: failures use exponential backoff with jitter (max 60s)
  and keep the previously cached catalog so a flaky upstream never blanks
  `/v1/models`. Counters are exposed via `internal/gateway/DynamicSyncRegistry`
  for future Prometheus integration.
- **Toggles**:
  - `NEXUS_DYNAMIC_MODEL_SYNC=true` — opt-in.
  - `NEXUS_DYNAMIC_MODEL_INTERVAL=30m` — refresh cadence (Go duration string).
  - `NEXUS_DYNAMIC_MODEL_MAX_RETRY=3` — retry budget per refresh.

### Console identity & sessions

- **Email + password login** (passwords are bcrypt-hashed). A login issues an
  HTTP-only session cookie; `/api/me/*` resolves the user from the session.
- Bootstrap the first admin with `NEXUS_ADMIN_EMAIL` / `NEXUS_ADMIN_PASSWORD`
  (created on startup only when the org has no users yet).
- Roles: `admin` (manages users) and `member` (self-service only). RBAC is
  enforced server-side (`requireUser` / `requireAdmin`).

### Per-user budget toggle

Each user can turn their **own** Nexus-side monthly budget / RPM enforcement on or
off. Off = only the provider's own limits apply (the user's bill is their own);
On = Nexus enforces the configured cap as a safety guardrail. The dashboard
**Account** tab exposes this toggle; the trace flags column shows a `byok` badge
when a request used a user's own key.

### SSO (OIDC, optional)

When SSO is enabled, the console shows a **Sign in with {label}** button above
the email/password forms. Nexus uses the standard OIDC Authorization Code
flow against a configurable IdP (Keycloak, Authentik, Zitadel, ...). The
browser is redirected to the IdP, the IdP authenticates the user, and Nexus
exchanges the code for tokens, verifies the ID token's signature + claims,
and then either links the verified identity to an existing user (by email)
or JIT-provisions a new `member` account.

#### Enable SSO

Set these environment variables on the gateway/console pod (via the Helm
chart's `config`/`secrets` values if you use it):

| Variable | Required | Example | Notes |
|----------|----------|---------|-------|
| `NEXUS_SSO_ISSUER` | yes | `https://keycloak.example.com/realms/cozy` | OIDC issuer URL; Nexus uses OIDC discovery against `<issuer>/.well-known/openid-configuration` |
| `NEXUS_SSO_CLIENT_ID` | yes | `nexus-console` | Must match a client in the IdP |
| `NEXUS_SSO_CLIENT_SECRET` | yes | (from IdP) | Confidential client; the secret is sent in the token-exchange body (HTTPS only) |
| `NEXUS_SSO_REDIRECT_URL` | yes | `https://console.example.com/api/auth/sso/callback` | Must be registered as a valid redirect URI on the IdP client |
| `NEXUS_SSO_LABEL` | no | `Keycloak` | UI label for the button; defaults to `SSO` |

When all four required values are present, `SSOConfig.Enabled()` returns
true, `GET /api/auth/config` reports `sso_enabled: true`, and the routes
`/api/auth/sso/login` and `/api/auth/sso/callback` are wired up. If any
value is missing, SSO is silently disabled and the existing email/password
flow is the only sign-in path.

#### Keycloak client setup (one-time)

In the realm that should be allowed to sign in (e.g. `cozy`):

1. **Realm → Clients → Create client**
   - **Client type**: OpenID Connect
   - **Client ID**: `nexus-console` (must match `NEXUS_SSO_CLIENT_ID`)
2. **Capability config**:
   - **Client authentication**: ON (this is a confidential client)
   - **Authentication flow**: Standard flow (Authorization Code)
   - **Direct access grants**: OFF
3. **Login settings**:
   - **Root URL**: `https://console.example.com`
   - **Valid redirect URIs**: `https://console.example.com/api/auth/sso/callback`
   - **Web origins**: `https://console.example.com` (or `*` for dev)
4. Copy the **Client secret** into `NEXUS_SSO_CLIENT_SECRET`.
5. Make sure every user that should be able to sign in has **Email verified**
   checked (otherwise Nexus refuses to link/JIT the account — see security
   notes below).

#### How linking works

When the IdP callback fires, Nexus:

1. Verifies the ID token signature, issuer, and expiry.
2. Requires `email` and `sub` claims, and `email_verified=true`.
3. Looks up the user by `(org_id, sso_provider, sso_subject)` — a hit means
   this identity has signed in before, reuse it.
4. Falls back to `email` lookup — if a user with the same email already
   exists, records the `(sso_provider, sso_subject, sso_issuer)` triple on
   that row so subsequent logins skip the email lookup.
5. Otherwise JIT-provisions a new `member` user with a random
   unguessable placeholder password (password login is therefore
   impossible for SSO-only users; the only way back in is via the IdP).

#### Security notes

- The OIDC `state` is a 32-byte random value stored in an `HttpOnly` cookie
  scoped to `/api/auth/sso`; the callback compares cookie vs. query param
  and rejects mismatches.
- ID token signature, issuer, and audience (client_id) are all validated
  by the upstream `coreos/go-oidc` library.
- `email_verified` must be `true`; unverified emails are rejected to
  prevent account takeover via IdP-side spoofing.
- The `(org_id, sso_provider, sso_subject)` tuple is unique — re-binding a
  user to a different IdP subject requires a manual DB update, so a
  Keycloak user cannot be silently re-mapped to another Keycloak user.

### BYOK API

- `POST /api/auth/login`, `POST /api/auth/logout`
- `GET /api/me`, `PATCH /api/me` *(toggle `enforce_limits`)*
- `GET/POST /api/me/keys`, `DELETE /api/me/keys/{id}` — self-service virtual keys
- `GET/POST /api/me/credentials`, `POST /api/me/credentials/{id}/rotate`,
  `DELETE /api/me/credentials/{id}` — self-service BYOK provider keys
- `GET/POST /api/users`, `DELETE /api/users/{id}` — admin user management
- `GET /api/users/quality?window=24h` — **per-user rolling quality + spend** (admin)

### Eval differentiator: per-user quality

Unlike spend-only gateways (Bifrost/LiteLLM track *who spent what*), Nexus also
tracks **what each user's rolling quality score is**. Async eval scores carry the
caller's `user_id` (denormalized onto `eval_scores`), so the console's **Per-user
quality** panel shows average judge quality, pass rate, eval sample count, request
volume, and spend per user — quality and cost on one screen, per credential owner.

```bash
# enable BYOK with a bootstrap admin
export NEXUS_POSTGRES_URL="postgres://nexus:nexus@localhost:5433/nexus?sslmode=disable"
export NEXUS_MASTER_KEY="$(openssl rand -hex 32)"
export NEXUS_KEY_MODE=byok
export NEXUS_ADMIN_EMAIL=admin@example.com
export NEXUS_ADMIN_PASSWORD='change-me'
go run ./cmd/nexus

# log in (stores the session cookie), register your own provider key, mint a vkey
curl -sc /tmp/cj -X POST localhost:8081/api/auth/login \
  -d '{"email":"admin@example.com","password":"change-me"}'
curl -sb /tmp/cj -X POST localhost:8081/api/me/credentials \
  -d '{"provider":"openai","name":"mine","secret":"sk-..."}'
curl -sb /tmp/cj -X POST localhost:8081/api/me/keys -d '{"name":"my-app"}'
# → calls made with that nxs_live_... key now go out on YOUR OpenAI key
```

## Rate limits and budgets

Each virtual key carries an `rpm_limit` (requests/min) and `monthly_budget_usd`.
The gateway enforces both per key:

- Over the RPM limit → `429 Too Many Requests` (with `Retry-After`).
- Monthly spend ≥ budget → `402 Payment Required`. Spend is accumulated from
  each request's computed cost.

With `NEXUS_REDIS_URL` set, counters are shared across all gateway replicas
(fixed per-minute window for RPM, monthly bucket for spend). Without Redis, an
in-memory limiter is used (correct for single-node only). `0` means unlimited.
