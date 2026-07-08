# Nexus

Open-core LLM gateway built **observability-first**, with eval-driven
**quality-aware routing** as its differentiator. Single Go binary, OpenAI-compatible
API, OpenTelemetry GenAI-aligned tracing, and a live dashboard.

> **Full project description**: [DESCRIPTION.md](DESCRIPTION.md)

> Status: Phases 1–4 implemented — gateway + observability + dashboard,
> control plane (keys/credentials), rate limits & budgets, async evals, and
> quality-aware routing. See
> [`.cursor/plans/llm_gateway_nexus_*.plan.md`](.cursor/plans) for the full roadmap.

## Architecture

- **Language**: Go single stack. Stateless data plane designed for horizontal
  autoscaling; the core boots with zero dependencies.
- **Standard**: traces use OpenTelemetry GenAI semantic conventions (`gen_ai.*`)
  so they export to any OTLP backend without remapping — no lock-in.
- **Stores**: ClickHouse (traces/scores), Postgres (meta, Phase 2),
  Redis (cache/limits, Phase 2). Managed ClickHouse Cloud in production.
- **Evals**: heavy eval (LLM-as-judge) runs async, off the request hot path,
  against a local SLM judge (Ollama/vLLM). Phase 3.

```
cmd/nexus            single binary (gateway :8080 + console :8081)
internal/gateway     OpenAI-compatible API, provider adapters, streaming, middleware
internal/observability  gen_ai.* traces -> ClickHouse + live hub
internal/core        control plane: virtual keys + encrypted credentials (Postgres)
internal/limiter     per-key RPM rate limits + monthly budgets (Redis / in-memory)
internal/evals       async eval worker: heuristics + SLM judge + remote eval client
internal/router      quality-aware model selection (eval quality + cost + latency)
internal/console     dashboard API + WebSocket live feed
eval-service/        optional Python sidecar: DeepEval + RAGAS (async, out-of-band)
web/                 React/TS dashboard
migrations/          SQL (ClickHouse + Postgres schema embedded & applied on startup)
deploy/              docker-compose (ClickHouse/Postgres/Redis/Ollama/eval-service)
```

## Quick start

> **TL;DR** — one line, zero prompts:
> ```bash
> curl -fsSL https://raw.githubusercontent.com/fun-fx/ffx_nexus/main/scripts/install.sh | bash
> ```
> Or with the friendly alias (once DNS is wired up): `curl -fsSL install.nexus.ffx.ai | bash`.
> The installer boots Postgres + Redis + ClickHouse + Ollama, builds the
> Go binary, and starts the gateway on `:8090` / console on `:8091`. See
> [`docs/quickstart.md`](docs/quickstart.md) for the full step-by-step.

| Path | Gateway | Console | Notes |
| --- | --- | --- | --- |
| One-line `install.sh` | `:8090` | `:8091` | Ports chosen to avoid clashes on a fresh machine; overridden by `NEXUS_GATEWAY_PORT` / `NEXUS_CONSOLE_PORT`. |
| `go run ./cmd/nexus` (source) | `:8080` | `:8081` | The Go binary defaults. Override with `NEXUS_GATEWAY_ADDR` / `NEXUS_CONSOLE_ADDR`. |
| Docker (`docker run ghcr.io/fun-fx/ffx_nexus`) | `:8080` | `:8081` | Same defaults; map with `-p 8080:8080 -p 8081:8081` or pass the `*_ADDR` envs. |
| Helm chart | `:8080` | `:8081` | Container defaults; expose via `service.port` / Ingress. |

Pick any row — every path is fully supported. The rest of this README uses
`:8080` / `:8081` everywhere (the binary default); the `install.sh` row uses
`:8090` / `:8091` to dodge a port already bound by another tool on most
laptops.

```bash
# 1. (optional) start local datastores
docker compose -f deploy/docker-compose.yml up -d clickhouse

# 2. configure providers + trace store
export OPENAI_API_KEY=sk-...
export ANTHROPIC_API_KEY=sk-ant-...
export GEMINI_API_KEY=...
export NEXUS_CLICKHOUSE_URL="clickhouse://nexus:nexus@localhost:9000/nexus"

# 3. run the gateway + console
go run ./cmd/nexus
# → gateway on :8080  •  console on :8081

# 4. (dev) run the dashboard
cd web && npm install && npm run dev   # http://localhost:5173
```

> **Heads-up:** the dashboard dev server (`npm run dev`) serves a hot-reloading
> SPA on `:5173` and proxies its `/api` calls to the console on `:8081`. If you'd
> rather skip the dev server, the console on `:8081` already serves a built SPA
> embedded into the Go binary. Both URLs fully functional.

The gateway boots even with no API keys or ClickHouse configured (traces are
then live-only). Set keys/URL to enable providers and persistence.

## Deploy to Kubernetes (Helm)

A first-party Helm chart lives in `deploy/helm/nexus`. It deploys the gateway
(`:8080`) and console (`:8081`) from a single container, with liveness/readiness
probes on `/healthz`, a non-root hardened pod, and optional Ingress / HPA /
PodDisruptionBudget. The chart does **not** run databases itself — it connects
to external/managed Postgres, ClickHouse, and Redis (toggle each on).

```bash
# Zero-dependency: just the gateway + console (point a provider key at it).
helm install nexus deploy/helm/nexus \
  --namespace nexus --create-namespace \
  --set secrets.openaiApiKey=sk-...

# Port-forward and try it
kubectl -n nexus port-forward svc/nexus 8080:8080 8081:8081
curl -s localhost:8080/healthz
```

Enable the control plane, persistence, and cache by wiring external datastores:

```bash
helm install nexus deploy/helm/nexus -n nexus --create-namespace \
  --set existingSecret=nexus-secrets \
  --set dependencies.postgres.enabled=true \
  --set dependencies.clickhouse.enabled=true \
  --set dependencies.redis.enabled=true
```

For production, create a Secret out-of-band and reference it with
`existingSecret` (keys: `OPENAI_API_KEY`, `NEXUS_MASTER_KEY`,
`NEXUS_POSTGRES_URL`, `NEXUS_CLICKHOUSE_URL`, `NEXUS_REDIS_URL`, …) instead of
putting secrets in `values.yaml`. All non-secret settings map to `config.*` in
`values.yaml` (routing, guardrails, semantic cache, self-correction).

Container images are published to `ghcr.io/fun-fx/ffx_nexus` on every `v*` tag.

## Provider catalog opt-in

By default Nexus runs in `strict_byok`: every caller registers their own
upstream key (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, …) via
`POST /api/credentials`. For admin-only flows — dogfooding, demos, internal
assistants — you can opt in to a *server-side* provider fallback by setting
the matching env var / Kubernetes Secret key. The keys are opt-in
independent of each other, so you can enable The Grid for production without
adding Groq or Mistral.

| Server-side key | Toggle location |
| --- | --- |
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` / `GEMINI_API_KEY` | env / Secret entry; both modes (`byok` / `strict_byok`) honoured |
| `GROQ_API_KEY` | env / Secret entry; chat-model ids auto-listed from the catalog |
| `MISTRAL_API_KEY` | env / Secret entry; chat + embedding ids auto-listed |
| `GRID_API_KEY` | env / Secret entry; 9 instrument ids auto-listed. `Authorization` is auto-stripped on cross-origin 307 redirects (security). |

### Opt-in on Kubernetes (Cozystack)

The prod values file uses `existingSecret: nexus`, so the chart's Secret
template is a no-op and changes via `helm upgrade -f` do **not** touch live
credentials. Patch the cluster Secret out-of-band to add a provider key:

```bash
# Login to the prod cluster (Tailscale + kubectl already set up).
kubectl -n tenant-nexus get secret nexus -o jsonpath='{.data}' | jq 'keys'
# → shows current keys (BASE64-encoded). Add one more:

kubectl -n tenant-nexus patch secret nexus --type merge \
  -p '{"stringData":{"GRID_API_KEY":"<grid-api-key-from-app.thegrid.ai>"}}'

# Trigger a rolling restart so the pod picks up the new env:
kubectl -n tenant-nexus rollout restart deployment/nexus

# Verify the provider is registered:
kubectl -n tenant-nexus exec deploy/nexus -- \
  curl -s http://localhost:8080/v1/models | jq '.data[] | select(.id | startswith("grid/"))'
# → lists grid/text-prime, grid/text-max, … once the pod has restarted
```

### Opt-in locally

```bash
# 1. Grab a Grid API key from https://app.thegrid.ai/profile/api-keys
export GRID_API_KEY=...
go run ./cmd/nexus   # nexus boots and registers grid/* in the registry

# 2. Use it via any OpenAI-compatible client:
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer nxs_live_..." \
  -d '{"model":"grid/text-prime","messages":[{"role":"user","content":"hi"}]}'
```

The Grid responds with a `307 Temporary Redirect` to an actual supplier, and
Nexus follows it with the Grid key (Authorization is stripped automatically
if the supplier's host differs from `api.thegrid.ai`).

## Usage

OpenAI-compatible — point any OpenAI SDK at `http://localhost:8080/v1`:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": true
  }'
```

Use a `provider/model` prefix to force a backend, e.g. `anthropic/claude-sonnet-4-5`.

### Supported endpoints

| Endpoint | Notes |
| --- | --- |
| `POST /v1/chat/completions` | OpenAI-compatible chat (streaming + non-streaming, tools with `tool_choice` + `parallel_tool_calls`, structured output) |
| `POST /v1/responses` | OpenAI Responses API (string or array `input`, tool calls surfaced as `function_call` items). Implemented as a thin shim over `/v1/chat/completions`. |
| `POST /v1/embeddings` | OpenAI-compatible embeddings for providers that implement the `EmbeddingsProvider` interface (OpenAI / Mistral today; Anthropic / Gemini / Groq to follow). Supports string and string-array `input`. |
| `POST /v1/moderations` | OpenAI-compatible content moderation. Omitted `model` defaults to `omni-moderation-latest`. Same `Auth`+`Enforce`+`BYOK` chain as chat. |
| `POST /v1/images/generations` | OpenAI-compatible image generation (`dall-e-3` and friends). Omitted `model` defaults to `dall-e-3`. |
| `GET  /v1/models` | Union of registered chat / embedding / moderation / image model ids across all installed providers |

All six endpoints go through the same `Auth` + `Enforce` middleware chain, so
virtual-key RPM/budget limits and BYOK credential resolution apply uniformly.

```bash
# Embeddings
curl http://localhost:8080/v1/embeddings \
  -H "Authorization: Bearer nxs_live_..." \
  -H "Content-Type: application/json" \
  -d '{"model":"text-embedding-3-small","input":["hello","world"]}'

# Responses API (multi-message + tool call)
curl http://localhost:8080/v1/responses \
  -H "Authorization: Bearer nxs_live_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "instructions": "Reply concisely.",
    "input": [
      {"role":"user","content":"What is the capital of France?"},
      {"role":"assistant","content":"Paris."},
      {"role":"user","content":"And of Italy?"}
    ]
  }'

# Moderation
curl http://localhost:8080/v1/moderations \
  -H "Authorization: Bearer nxs_live_..." \
  -H "Content-Type: application/json" \
  -d '{"model":"omni-moderation-latest","input":"I want to hurt myself."}'

# Image generation (dall-e-3 default)
curl http://localhost:8080/v1/images/generations \
  -H "Authorization: Bearer nxs_live_..." \
  -H "Content-Type: application/json" \
  -d '{"model":"dall-e-3","prompt":"a watercolor of a ship in a storm","size":"1024x1024"}'
```

## Configuration

| Env var | Default | Purpose |
| --- | --- | --- |
| `NEXUS_GATEWAY_ADDR` | `:8080` | Gateway proxy listen address (override when `:8080` clashes; e.g. `install.sh` uses `:8090` via `NEXUS_GATEWAY_PORT`) |
| `NEXUS_CONSOLE_ADDR` | `:8081` | Console API / dashboard listen address (override similarly; e.g. `install.sh` uses `:8091` via `NEXUS_CONSOLE_PORT`) |
| `NEXUS_CLICKHOUSE_URL` | _(empty)_ | Native DSN; empty disables persistence |
| `NEXUS_POSTGRES_URL` | _(empty)_ | Control plane DSN; empty disables key auth & credential store |
| `NEXUS_REDIS_URL` | _(empty)_ | Shared rate limits + budgets across replicas; empty = in-memory |
| `NEXUS_MASTER_KEY` | _(empty)_ | 32-byte (base64/hex) KEK for provider-secret encryption |
| `NEXUS_KEY_MODE` | `shared` | Upstream key resolution: `shared` / `byok` / `strict_byok` |
| `NEXUS_ADMIN_EMAIL` / `NEXUS_ADMIN_PASSWORD` | — | Bootstrap the first console admin (only when no users exist) |
| `NEXUS_ALLOW_SIGNUP` | `false` | Enable public `POST /api/auth/register` (member role only) |
| `NEXUS_SSO_ISSUER` / `NEXUS_SSO_CLIENT_ID` / `NEXUS_SSO_CLIENT_SECRET` / `NEXUS_SSO_REDIRECT_URL` | — | OIDC SSO; when all four are set, `/api/auth/sso/login` is enabled. See [SSO (OIDC)](#sso-oidc-optional). |
| `NEXUS_SSO_LABEL` | `SSO` | UI label for the SSO button (e.g. `Keycloak`) |
| `OPENAI_API_KEY` / `OPENAI_BASE_URL` | — | OpenAI provider |
| `ANTHROPIC_API_KEY` | — | Anthropic provider |
| `GEMINI_API_KEY` | — | Google Gemini provider |
| `GROQ_API_KEY` | — | Groq OpenAI-compatible endpoint (Llama 3.x, Mixtral, Gemma, Whisper, llama-guard; chat model ids auto-listed) |
| `MISTRAL_API_KEY` | — | Mistral OpenAI-compatible endpoint (mistral-large/small, codestral, mixtral, pixtral) |
| `GRID_API_KEY` | — | The Grid (thegrid.ai) OpenAI-compatible endpoint — instruments: text-{standard,prime,max}, code-{standard,prime,max}, agent-{standard,prime,max}. On 307 supplier redirect, `Authorization` is auto-stripped when the new host is not `api.thegrid.ai` (security). See [Provider catalog opt-in](#provider-catalog-opt-in). |
| `NEXUS_JUDGE_BASE_URL` / `NEXUS_JUDGE_MODEL` | — / `qwen2.5:7b` | Local SLM judge (Phase 3) |
| `NEXUS_EVAL_ENABLED` | `true` | Async eval worker (heuristics + optional judges) |
| `NEXUS_JUDGE_API_KEY` / `NEXUS_EVAL_SAMPLE_RATE` | — / `1.0` | Judge auth + judge sampling fraction |
| `NEXUS_EVAL_SERVICE_URL` / `_METRICS` | — / `answer_relevancy,toxicity,bias` | Python eval sidecar (DeepEval/RAGAS) |
| `NEXUS_EVAL_WORKERS` / `NEXUS_EVAL_SERVICE_TIMEOUT` | `4` / `30s` | Eval worker concurrency + sidecar timeout |
| `NEXUS_ROUTE_GROUPS` | _(empty)_ | Routing aliases, `alias=m1,m2;...` (Phase 4) |
| `NEXUS_ROUTE_W_QUALITY` / `_W_COST` / `_W_LATENCY` | `0.6` / `0.2` / `0.2` | Routing weights |
| `NEXUS_ROUTE_WINDOW` / `NEXUS_ROUTE_REFRESH` | `1h` / `30s` | Routing stats window & refresh |
| `NEXUS_UPSTREAM_TIMEOUT` | `120s` | Upstream provider timeout |

## Control plane (Phase 2): keys & credentials

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

## Rate limits & budgets (Phase 2)

Each virtual key carries an `rpm_limit` (requests/min) and `monthly_budget_usd`.
The gateway enforces both per key:

- Over the RPM limit → `429 Too Many Requests` (with `Retry-After`).
- Monthly spend ≥ budget → `402 Payment Required`. Spend is accumulated from
  each request's computed cost.

With `NEXUS_REDIS_URL` set, counters are shared across all gateway replicas
(fixed per-minute window for RPM, monthly bucket for spend). Without Redis, an
in-memory limiter is used (correct for single-node only). `0` means unlimited.

## Evals (Phase 3)

When enabled (`NEXUS_EVAL_ENABLED=true`, default), completed traces are evaluated
**out-of-band** by a background worker — never on the request hot path. Heuristics
and judges run without ClickHouse; **score persistence** and **quality-aware routing**
require `NEXUS_CLICKHOUSE_URL` (scores land in the `eval_scores` table).

- **Heuristics (always on when eval is enabled, cheap):** `heuristic_pii` (flags emails/SSN/phone/card
  patterns in output) and `heuristic_completeness` (empty or truncated answers).
- **LLM-as-judge (sampled):** a local SLM (Ollama/vLLM, OpenAI-compatible API)
  scores response `quality` 0..1. Runs on `NEXUS_EVAL_SAMPLE_RATE` of traces and
  stays local for data privacy. Enable with `NEXUS_JUDGE_BASE_URL`.
- **External eval service (sampled):** an optional Python sidecar running mature
  eval libraries (**DeepEval** + **RAGAS**) for richer metrics
  (answer relevancy, toxicity, bias, and — when retrieval contexts are supplied —
  hallucination / faithfulness). Enable with `NEXUS_EVAL_SERVICE_URL`. See below.

### External Python eval service

The Go gateway stays the hot path; deep eval (which benefits from Python's
ecosystem) runs in a separate async sidecar under `eval-service/`. The eval
worker calls it over HTTP **only on sampled traces**, off the request path.

- **Why a sidecar:** DeepEval/RAGAS are best-in-class but Python- and LLM-bound.
  Isolating them keeps the Go gateway's per-request overhead unchanged while
  giving you the full metric catalog.
- **Failure isolation:** if the sidecar is slow or down, the requested metrics
  are simply skipped and evaluation degrades to the Go heuristics. The gateway
  response and routing availability are never affected.
- **Judge reuse:** by default it points at the same local Ollama/vLLM judge.
  Set `EMBEDDINGS_BASE_URL` on the service to unlock RAGAS metrics.

```bash
# Start the sidecar (reuses the compose Ollama judge):
docker compose -f deploy/docker-compose.yml --profile eval up -d eval-service

# Point the gateway at it:
export NEXUS_EVAL_SERVICE_URL=http://localhost:8200
export NEXUS_EVAL_SERVICE_METRICS=answer_relevancy,toxicity,bias
```

Scores returned by the service land in the same `eval_scores` table (with
`evaluator` = `deepeval`/`ragas`) and feed quality-aware routing like any other
evaluator.

### RAG eval context (`nexus_eval`)

Pass optional retrieval data on `POST /v1/chat/completions`. The block is **never
forwarded upstream** — it is stored on the trace and passed to the async eval
worker only. When `contexts` are present, the worker automatically adds
`hallucination` and `ragas_faithfulness` to the remote eval request.

```bash
curl -s localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer nxs_live_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-2.5-flash",
    "messages": [{"role": "user", "content": "What is the capital of France?"}],
    "nexus_eval": {
      "contexts": ["Paris is the capital of France."],
      "reference": "Paris"
    }
  }'
```

### Offline regression eval (`nexus-evalbatch`)

A standalone CLI runs a fixed dataset through the eval service and aggregates the
scores, so you can catch quality regressions when you change a prompt, model, or
config. Unlike the online worker it does **no sampling** — every case is scored —
and it can fail CI when scores drop versus a stored baseline.

The dataset is JSON Lines, one case per line:

```json
{"id":"q1","model":"gpt-4o-mini","input":"Capital of France?","output":"Paris.","reference":"Paris"}
{"id":"rag1","input":"When was the Eiffel Tower completed?","output":"1889.","contexts":["Completed in 1889."]}
```

- `output` present → the recorded answer is evaluated directly.
- `output` omitted + `-gateway-url` → the answer is generated first (any
  OpenAI-compatible endpoint), then evaluated.
- `contexts` present → RAG metrics (`hallucination`, `ragas_faithfulness`) are
  added automatically.

```bash
go build -o bin/nexus-evalbatch ./cmd/nexus-evalbatch

# Score recorded outputs and save a baseline
./bin/nexus-evalbatch \
  -dataset datasets/regression_example.jsonl \
  -service-url http://localhost:8200 \
  -out baseline.json

# Later: fail (exit 1) if any metric's mean dropped > tolerance vs the baseline
./bin/nexus-evalbatch \
  -dataset datasets/regression_example.jsonl \
  -service-url http://localhost:8200 \
  -baseline baseline.json -tolerance 0.05
```

**Evaluators**: `-evaluator remote` (default) scores via the Python eval service
(DeepEval/RAGAS, needs a judge LLM). `-evaluator heuristic` scores locally with
the built-in LLM-free heuristics (`pii_leak`, `completeness`) — fully
deterministic and dependency-free, so it runs **hermetically in CI**:

```bash
./bin/nexus-evalbatch \
  -dataset datasets/regression_example.jsonl \
  -evaluator heuristic \
  -baseline datasets/regression_baseline.json -tolerance 0.05
```

The CI **eval regression gate** (`.github/workflows/ci.yml`) runs exactly this
on every PR against the committed `datasets/regression_baseline.json`, failing
the build on any quality regression — no provider keys or eval service required.
Regenerate the baseline with `-out datasets/regression_baseline.json` when an
intended change shifts scores.

Key flags: `-metrics` (comma-separated ids), `-gateway-url`/`-api-key`/`-gen-model`
(generate missing outputs), `-concurrency`, `-timeout`, `-detail` (per-case scores
in the JSON report). `-service-url` defaults to `NEXUS_EVAL_SERVICE_URL`.

## Quality-aware routing (Phase 4)

Send a request to a routing alias instead of a concrete model and the gateway
picks the best candidate using rolling stats (eval quality + cost + latency,
weighted and min-max normalized). Candidates with no stats yet get optimistic
exploration traffic. A virtual key's `allowed_models` still constrains the set.

The **quality signal** blends both eval sources, so routing reacts to evals even
when the SLM judge is disabled:

- **Judge quality** (`metric=quality`, 0..1) and **heuristic safety pass rate**
  (PII/completeness) are combined: `0.7·quality + 0.3·safety` when both exist,
  otherwise whichever is available, or an exploration value when neither is.

A virtual key's **`min_quality_score`** is enforced here: candidate models whose
blended quality is below the threshold are dropped. If no allowed model clears
the bar, the request is rejected with `503 no_model_meets_quality`. `0` disables
the gate.

- Built-in alias `auto` routes across **all** registered models.
- Named groups via `NEXUS_ROUTE_GROUPS=fast=gpt-4o-mini,gemini-2.5-flash;smart=gpt-4o,...`.

### Provider fallback

For routing aliases the candidates are tried **best-first**: if an upstream
provider errors, the gateway automatically fails over to the next-ranked model
(failover attempts are traced as `upstream_error_failover`). A request to a
**concrete** model is not failed over — only the requested model is attempted.
Streaming requests fail over only before the first byte is sent.

```bash
curl -s localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer nxs_live_..." \
  -d '{"model":"auto","messages":[{"role":"user","content":"hi"}]}'
```

Inspect current routing stats: `GET /api/routing`.

### Load balancing within routing tiers

Quality-aware routing ranks candidates by eval quality, cost, and latency, but
without load balancing the top-ranked model absorbs all primary traffic. When
`NEXUS_ROUTE_LOAD_BALANCE=true`, the gateway **rotates the primary model with
rank-weighted round-robin** among all quality-qualified candidates in a routing
alias (`auto` or a named group): the best-ranked model still gets proportionally
more primary traffic, while lower-ranked qualified models get a fair share.
Selection is deterministic and smooth (nginx-style SWRR), so traffic stays
balanced without thundering-herd spikes. Failover order for the remaining models
is unchanged.

Requires ClickHouse (for the quality router). Composes with
`NEXUS_ROUTE_GROUPS` and virtual-key `min_quality_score`.

### Semantic cache

Near-duplicate prompts can skip the upstream LLM entirely. When
`NEXUS_SEMANTIC_CACHE_ENABLED=true`, the gateway embeds the prompt (via an
OpenAI-compatible `/v1/embeddings` endpoint), searches a **Redis-backed cache**
for a stored completion above a cosine-similarity threshold, and returns it on
hit. Misses are stored after a successful upstream call.

- Requires `NEXUS_REDIS_URL` and `NEXUS_EMBEDDINGS_URL`.
- Non-streaming only; skips tool calls, sampled requests (any non-zero
  `temperature`), and `nexus_eval` requests. Only deterministic requests
  (temperature unset or `0`) are cached, so a single sampled answer is never
  replayed as if canonical.
- **Tenant-isolated**: cache entries are namespaced per org / virtual key
  (`nexus:sem:{scope}:{model}`), so one tenant never receives another tenant's
  cached response.
- **Alias-aware**: keyed by the client-requested model. When the request targets
  a routing alias, the cache key is the alias (not the concrete model), so
  load-balancer rotation across quality-interchangeable members does not
  fragment the cache.
- **Bounded hot path**: the lookup embedding is capped by
  `NEXUS_EMBEDDINGS_TIMEOUT` (default 5s). A slow or unhealthy embeddings
  endpoint degrades to a normal upstream call instead of stalling the request,
  and lookup/store errors are logged.
- Hits are traced as `cache_hit: true` (zero upstream cost on the trace).
- Tunables: `NEXUS_SEMANTIC_CACHE_TTL` (default 24h),
  `NEXUS_SEMANTIC_CACHE_THRESHOLD` (default 0.92),
  `NEXUS_SEMANTIC_CACHE_MAX_ENTRIES` per model (default 500).

```bash
NEXUS_SEMANTIC_CACHE_ENABLED=true \
NEXUS_REDIS_URL=redis://localhost:6379/0 \
NEXUS_EMBEDDINGS_URL=http://localhost:11434/v1 \
NEXUS_EMBEDDINGS_MODEL=nomic-embed-text \
  ./bin/nexus
```

## Inline guardrails

Unlike the async eval workers (which observe completed traces out-of-band),
**guardrails run synchronously on the request hot path** and can block a request
or redact a response. They are intentionally cheap — regex and length checks
only, no network calls — so they add negligible latency.

- **Input guardrails** run *before* any upstream call, so blocked content costs
  zero tokens. A rejected request returns `403 guardrail_blocked`.
  - `NEXUS_GUARDRAILS_BLOCK_PII_INPUT` — reject prompts containing PII (email,
    SSN, phone, card patterns).
  - `NEXUS_GUARDRAILS_MAX_INPUT_CHARS` — reject prompts over N characters.
  - `NEXUS_GUARDRAILS_DENY_PATTERNS` — semicolon-separated regexes (e.g. prompt
    injection phrases); any match rejects the request.
- **Output guardrails** run on the response:
  - `NEXUS_GUARDRAILS_REDACT_PII_OUTPUT` — replace PII in non-streaming
    responses with `[REDACTED]`. (Streaming responses are not redacted.)
  - `NEXUS_GUARDRAILS_VALIDATE_JSON_OUTPUT` — when a request sets a JSON
    `response_format`, enforce that the output is valid JSON (see below).

Enable with `NEXUS_GUARDRAILS_ENABLED=true` plus at least one rule. Guardrail
decisions are surfaced on the live trace feed via `guardrail_action`.

```bash
NEXUS_GUARDRAILS_ENABLED=true \
NEXUS_GUARDRAILS_BLOCK_PII_INPUT=true \
NEXUS_GUARDRAILS_DENY_PATTERNS='(?i)ignore previous instructions' \
  ./bin/nexus
```

### Schema / JSON output guardrail

Providers don't always honor JSON mode reliably. When
`NEXUS_GUARDRAILS_VALIDATE_JSON_OUTPUT=true` and a request carries an OpenAI
`response_format`, Nexus validates the model output:

- `response_format: { "type": "json_object" }` — output must be parseable JSON.
- `response_format: { "type": "json_schema", "json_schema": { "schema": {...} } }`
  — output must also conform to the supplied JSON Schema (draft 2020-12).

Non-streaming violations are blocked on the hot path with
`422 schema_validation_failed`; the `response_format` is still forwarded upstream
so native JSON modes keep working. Streaming responses can't be blocked after
bytes are sent, so violations are recorded on the trace
(`guardrail_action=output_schema_violation`) instead.

```bash
curl -s localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer nxs_live_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "messages": [{"role": "user", "content": "Extract name and age."}],
    "response_format": {
      "type": "json_schema",
      "json_schema": {
        "name": "person",
        "schema": {
          "type": "object",
          "properties": {"name": {"type": "string"}, "age": {"type": "integer"}},
          "required": ["name", "age"]
        }
      }
    }
  }'
```

### Structured-output self-correction

Rather than failing a malformed JSON response outright, the gateway repairs it.
When the schema guardrail rejects a non-streaming JSON response, Nexus runs a
two-stage recovery:

1. **Free local repair (always on):** strip a markdown ```` ```json ```` fence or
   surrounding prose ("Sure, here you go: {...}") and re-validate — no extra
   upstream call. This handles the most common failure modes at zero cost.
2. **Paid self-correction (opt-in):** if local repair isn't enough and
   `NEXUS_SELF_CORRECTION_ENABLED=true`, append the rejected output plus a
   correction instruction and retry the **same** model up to
   `NEXUS_SELF_CORRECTION_MAX_RETRIES` times (default 1).

If a stage passes validation the response is returned with `200`; otherwise it
falls back to `422 schema_validation_failed`.

- Non-streaming only (a streamed response can't be retried after bytes are sent).
- Requires `NEXUS_GUARDRAILS_VALIDATE_JSON_OUTPUT=true` to supply the rejection
  signal. Token usage from every paid attempt is summed into the trace cost.
- Outcomes are surfaced on the trace as `guardrail_action`: `json_repaired`,
  `self_corrected:N`, or both (`json_repaired,self_corrected:N`).

```bash
NEXUS_GUARDRAILS_ENABLED=true \
NEXUS_GUARDRAILS_VALIDATE_JSON_OUTPUT=true \
NEXUS_SELF_CORRECTION_ENABLED=true \
NEXUS_SELF_CORRECTION_MAX_RETRIES=2 \
  ./bin/nexus
```

## CI/CD

GitHub Actions workflows live in [`.github/workflows/`](.github/workflows/).

| Workflow | Trigger | What it does |
| --- | --- | --- |
| **CI** | push / PR to `main` | `gofmt`, `go vet`, `go test -race`, Go build, `web/` TypeScript + Vite build |
| **Integration** | push / PR to `main`, manual | Docker Compose (Postgres, ClickHouse, Redis) + `./scripts/test_all.sh` |
| **Release** | tag `v*` (e.g. `v0.1.0`) | Build & push image to `ghcr.io/fun-fx/ffx_nexus` |

### Deploying

This repo publishes a container image to `ghcr.io/fun-fx/ffx_nexus` and ships a
generic Helm chart under [`deploy/helm/nexus`](deploy/helm/nexus). Point the
chart at your own cluster, datastores, and provider policy — see the chart
`values.yaml` for the full surface. A minimal deploy:

```bash
helm upgrade --install nexus deploy/helm/nexus \
  --namespace nexus --create-namespace \
  --set image.tag=v0.1.0
```

> The specific on-prem production pipeline for the maintainers' cluster
> (Talos + Cozystack, in-cluster image build, prod values) lives in a separate
> private operations repo and is intentionally not part of this public release.

### Local parity

```bash
# Same checks as CI
gofmt -l .          # should print nothing
go vet ./...
go test -race ./...
go build ./cmd/nexus
cd web && npm ci && npm run build

# Same as Integration workflow
./scripts/test_all.sh
```

### Optional: full upstream tests in CI

Integration tests for rate limits (`429`) and budgets (`402`) need **no** provider keys.
For real Gemini/OpenAI completion, eval, and routing tests, add a repository secret:

- GitHub → **Settings → Secrets and variables → Actions**
- `GEMINI_API_KEY` (or `OPENAI_API_KEY`)

### Release a version

See [`CHANGELOG.md`](CHANGELOG.md) for what changed in each release and
[`docs/release-notes/v0.1.0.md`](docs/release-notes/v0.1.0.md) for the
current pilot handoff letter.

```bash
git tag v0.1.0
git push origin v0.1.0
# → ghcr.io/fun-fx/ffx_nexus:0.1.0
```

Run the image locally (datastores must be reachable separately):

```bash
docker run --rm -p 8080:8080 -p 8081:8081 \
  -e NEXUS_POSTGRES_URL=postgres://nexus:nexus@host.docker.internal:5433/nexus?sslmode=disable \
  -e NEXUS_CLICKHOUSE_URL=clickhouse://nexus:nexus@host.docker.internal:9000/nexus \
  -e NEXUS_REDIS_URL=redis://host.docker.internal:6379/0 \
  -e NEXUS_MASTER_KEY="$(openssl rand -hex 32)" \
  ghcr.io/fun-fx/ffx_nexus:0.1.0
```

## Console API

- `GET /api/stats?window=1h` — aggregate metrics
- `GET /api/traces?limit=100` — recent traces
- `GET /api/routing` — per-model rolling quality/cost/latency used for routing
- `GET /api/live` — WebSocket live trace feed
- `POST /api/auth/login`, `POST /api/auth/logout`, `GET/PATCH /api/me` — session auth + self settings
- `GET /api/auth/sso/login`, `GET /api/auth/sso/callback` — OIDC SSO (only when SSO env vars are set)
- `GET/POST /api/me/keys`, `GET/POST /api/me/credentials` — BYOK self-service
- `GET/POST /api/users`, `DELETE /api/users/{id}` — admin user management
- `GET /api/users/quality` — per-user rolling quality + spend (admin)

## License

Nexus is dual-licensed:

- The **Go gateway, console, and bundled binaries** in this repository
  (everything under `cmd/`, `internal/`, `migrations/`, `eval-service/`,
  `scripts/`, `deploy/`, `Dockerfile`, plus CLI tooling) are released under
  the Apache License 2.0. See [`LICENSE`](LICENSE).
- The **React/TypeScript dashboard** under `web/` (and the corresponding
  embedded SPA assets) is released under the MIT License. See
  [`LICENSE-MIT`](LICENSE-MIT).

By contributing, you agree that new contributions fall under the same terms
as the file they touch — Apache-2.0 for backend / infra files, MIT for
dashboard files. The full license texts are the authoritative source; the
table above is a summary.
