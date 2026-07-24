# Changelog

All notable changes to Nexus are documented in this file. The format is
loosely based on [Keep a Changelog](https://keepachangelog.com), and the
project adheres to [Semantic Versioning](https://semver.org/) for the
Go gateway binary.

## [v0.6.3] — EvalRules switcher parity (PR #140)

Console-only follow-up to [v0.6.2](#v062--evalprofiles-switcher--nexus-favicon).

### Highlights

- **Heuristic rows use the same switch cell as profiles.** The PII / Completeness rows
  inside the Heuristics card now use the shared `ToggleCell` (extracted in
  PR #138/#139). Drop the previous `StatusPill on/off` + `Disable/Enable`
  button pair so the heuristics card and the profiles card share one
  visual affordance for the same underlying state. Keyboard (Space / Enter)
  works the same as on profile rows.
- **Shared `ToggleCell` component.** Extracted from the inline copy in
  `EvalProfiles.tsx` into `web/src/components/ToggleCell.tsx` so future
  in-row enable flags (routing groups, persona flags, etc.) drop in the
  same primitive.
- **SLM judge / Remote eval rows intentionally still use `StatusPill`.**
  Their `enabled` flag is env-driven and intentionally has no in-UI
  affordance, so the pill stays.

### Developer notes

- UI only. No backend change, no hot-path impact.
- Build / TSC / Vitest still clean.

## [v0.6.2] — EvalProfiles switcher + Nexus favicon

Console-only follow-up to [v0.6.1](#v061--evalprofiles-console-ux-consistency).

### Highlights

- **Switch cell.** Replace the old two-element "StatusPill on/off +
  Disable/Enable button" arrangement inside each profile row with a
  single `role="switch"` cell. Same shape and size regardless of
  state — off keeps the muted panel tone, on slides the thumb across
  the accent gradient. Space / Enter work too.
- **Nexus favicon.** A simple two-node / connector motif rendered as a
  32×32 SVG on the existing gradient, declared via
  `<link rel="icon" type="image/svg+xml" href="/favicon.svg">`.
  Browser tabs and bookmarks for `https://nexus.ffx.ai` show this
  instead of the default globe.

### Changed

- `web/src/pages/EvalProfiles.tsx`: introduces `<ToggleCell>`
  replacing the StatusPill + Disable/Enable pair; `busyToggle`
  state threaded through `<Group>` (mirroring `busyDelete`).
- `web/src/styles.css`: new `.toggle-cell` / `.toggle-cell-track` /
  `.toggle-cell-thumb` rules with `.toggle-cell-on` modifier.
- `web/index.html`: added `<link rel="icon">` tag.
- `web/public/favicon.svg`: new asset.

### Performance / hot path

No backend changes. `/v1/chat/completions` and the eval worker stay
byte-identical with v0.6.0 / v0.6.1.

### Upgrade notes

Existing deployments pick this up on the next `v*` tag push. Helm
chart version bumps to `0.6.2`, `appVersion` to `0.6.2`.

## [v0.6.1] — EvalProfiles console UX consistency

Visual follow-up to [v0.6.0](#v060--profile-driven-evals-go--python-sidecar--console-ui)
that aligns the new `Eval profiles` card and its drawer with the rest
of the console (Login / Playground / Keys / Credentials / Routing /
Eval / Audit / Overview).

### Highlights

- **Shared drawer.** The profile create / edit drawer now uses the
  same `<Drawer>` component (with the existing focus-trap, ESC-close,
  and overlay dismiss) as every other tab. No more bespoke
  `drawer-overlay / drawer-head / drawer-foot` markup.
- **Shared button palette.** Primary CTAs (`+ New profile`,
  `Create profile`, `Save changes`) are `btn-neon`; secondary actions
  (`Cancel`, `Disable`, `Enable`, `Edit`, `Delete`) are `btn-ghost`.
  The danger tone for `Delete` is a single `.row-action-danger`
  modifier on top of `btn-ghost`, no more raw `.btn.danger`.
- **Shared panel shell.** `Eval profiles` now lives in a `.panel`
  (matching `Audit`, `Credentials`, `Routing`, and the existing eval
  cards) instead of a one-off `.card` variant. Old `.card-head` /
  `.card-title` are now `.panel-head` / `.panel-title`.
- **Shared field rows.** Form rows in the drawer use the global
  `.field-row` class that `Login`, `Playground`, `Keys`, and
  `Credentials` already use. The local `.field / .field-label /
  .field-control` trio is gone.
- **CSS diet.** `styles.css` sheds the duplicate `.drawer-foot`
  block, the unused `.field / .field-control`, and the standalone
  `.btn.danger` rule. Bundle loses 0.35 kB of dead CSS.

### Changed

- `web/src/components/Drawer.tsx`: optional `testId` prop forwarded
  to the dialog `div` so the existing `data-testid="profile-drawer"`
  test query keeps working without coupling the tests to internal
  structure.
- `web/src/pages/EvalProfiles.tsx`:
  - Section wrapper: `section card evals-card` →
    `section panel profiles-card`.
  - Header: `card-head / card-title (h3)` → `panel-head /
    panel-title (h2)`.
  - Action buttons: `btn btn-primary` → `btn-neon`;
    `btn small` → `btn-ghost btn-small`; `btn small danger` →
    `btn-ghost btn-small row-action-danger`.
  - Field rows: `Field` component renamed to `FieldRow` and uses
    the global `.field-row` wrapper.
  - Drawer shell: replaced with the shared `<Drawer>` component.
- `web/src/styles.css`: removed `.field`, `.field-control`,
  `.btn.danger`, and the duplicate `.drawer-foot` block; added
  `.row-action-danger` for the delete button tone.

### Performance / hot path

No gateway / eval sidecar changes. This release is purely a console
UI alignment; `/v1/chat/completions`, eval worker tick, and the
secret resolver all stay byte-identical with v0.6.0.

### Upgrade notes

Existing `v0.6.0` deployments need no configuration changes — the
console rebuilds from the existing Helm value
(`image.repository: ghcr.io/fun-fx/ffx_nexus`, tag pinned by the
Helm release). Helm chart version bumps to `0.6.1`, `appVersion`
to `0.6.1`.

## [v0.6.0] — Profile-driven evals (Go + Python sidecar + Console UI)

Replaces the global, env-only eval configuration with first-class,
per-evaluation **profiles** that admins can author, toggle, and scope
from the Console without redeploying the gateway.

### Highlights

- **One profile = one evaluator spec.** `EvalProfile` (`internal/evals/profiles.go`)
  carries the metric kind (`heuristic_pii` / `heuristic_completeness` /
  `slm_judge` / `remote_eval`), scope (`org` vs `user`), endpoint,
  `key_source` (`org` / `user` / `inline` / `builtin`), threshold,
  sample rate, and a metric-specific config blob. `Worker.ReplaceProfiles`
  swaps the active profile set on the next eval tick; profiles that are
  disabled in the UI are skipped at dispatch.
- **UI-driven secrets.** No more `OPENAI_API_KEY` / `JUDGE_URL` env vars
  required for evals. `SecretResolver` (`internal/evals/secret_resolver.go`)
  fetches keys from org credentials, the calling user's BYOK store, or
  an inline registered secret, matching the same precedence the gateway
  uses for normal traffic.
- **Dynamic per-request overrides.** `EvalOverride`
  (`internal/evals/override.go`) carries judge URL, judge model, and
  threshold from the active profile to the Python sidecar on every
  batch. The Python service honors request fields over env config
  (`eval-service/app/judge.py`), so changing a profile in the Console
  flows through without a restart.
- **Console CRUD UI.** A new *Profiles* card under the Eval page lets
  admins create / edit / enable / disable / delete profiles and groups
  them by scope (`web/src/pages/EvalProfiles.tsx`). `ProfileDrawer`
  encodes the key-source ↔ kind invariants client-side (heuristics are
  pinned to `builtin`), and the React Query cache invalidates on
  every mutation for instant feedback.

### Added

- `internal/evals/profiles.go`, `internal/evals/profile_store.go`,
  `internal/evals/profile_store_helpers.go` — `EvalProfile` schema,
  `ProfileStore` interface, and a persistent store wrapper.
- `internal/evals/secret_resolver.go`,
  `internal/evals/store_secret_lookup.go` — `SecretResolver` + a
  `core.Store`-backed lookup that mirrors gateway BYOK precedence.
- `internal/evals/batcher.go` — `Batcher` collects traces and flushes
  them to the Python sidecar in size/time-windowed batches
  (configurable `BatchConfig`).
- `internal/evals/override.go` — `EvalOverride` request envelope.
- `internal/console/eval_profiles.go` — `/api/eval/profiles` CRUD
  handlers (`profileCallerCanSee` / `profileCallerCanWrite`).
- `eval-service/app/schemas.py` — `EvalBatchRequest` /
  `EvalBatchResponse` and override fields on `EvaluateRequest`.
- `eval-service/app/main.py` — `/evaluate/batch` endpoint with
  concurrent metric dispatch (`asyncio.gather`).
- `web/src/pages/EvalProfiles.tsx` + `EvalProfilesCard` /
  `ProfileDrawer` — full CRUD UI.
- `cmd/nexus/profile_store.go`, `cmd/nexus/eval_runtime.go` — runtime
  wiring (`SeedProfilesFromConfig`, `SetSecretResolver`,
  `SetEvalProfiles`).

### Changed

- `internal/evals/worker.go` — eval execution now fans out across
  metrics (`scoreBag` + `runEvaluators`) so a request's eval latency is
  bounded by the slowest metric, not their sum. Sequential execution
  paths were removed.
- `internal/evals/remote.go` — `RemoteEvaluator` now accepts
  `EvalOverride`, supports `EvaluateBatch`, and runs on an
  `http.Transport` with keep-alive (`MaxIdleConns=64`,
  `MaxIdleConnsPerHost=16`).
- `eval-service/app/judge.py`, `eval-service/app/metrics.py` —
  LLM / embeddings / threshold construction prefers request fields
  over `settings`.
- `cmd/nexus/main.go` — startup seeds default profiles from the
  current env (if set) and registers a `SecretResolver` so existing
  configs keep working without editing the deploy.
- `web/src/pages/Eval.tsx` — renders the new `EvalProfilesCard` and
  forwards `isAdmin` to `EvalRules`.
- `web/src/api.ts` — `EvalProfile` types and CRUD helpers.

### Performance guardrails (hot path zero-impact)

- Profile resolution runs only on the eval worker tick, **never** in
  the gateway hot path. Gateway middleware, `ResolveCredential`,
  routing, and tracing remain unchanged for a `/v1/chat/completions`
  call.
- Eval dispatch uses fan-out + batched HTTP to the Python sidecar, so
  per-request eval overhead drops from `O(metrics)` to `O(1)` HTTP
  round trips and parallel metric latency.
- Remote evaluator HTTP client pools keep-alive connections
  (`IdleConnTimeout=90s`).
- Batcher caps in-flight queues (`MaxQueue`); on overflow it drops
  with a counter so a stuck sidecar can never back-pressure live
  traffic.
- `secretResolver` is invoked inside the worker goroutine; org/user
  lookups reuse the existing credential pool — no new
  database connections per request.

### Security / boundary

- Inline secrets are stored in-memory only (process-local); they are
  scoped to the registering user and never persisted.
- Profile mutations are gated by `profileCallerCanWrite`: only admins
  can edit org-scope profiles; users can only edit their own user-scope
  profiles.
- Eval override fields never leak credentials: the sidecar only sees
  the resolved key, not the source.

### Upgrade notes

- Existing deployments using `OPENAI_API_KEY` / `JUDGE_URL` env vars
  will continue to work: `SeedProfilesFromConfig` materialises a
  default profile from those vars on first boot.
- To go fully env-free, run `GET /api/eval/profiles` to find the
  seeded profile and `PATCH /api/eval/profiles/{id}` to attach an
  inline key (or switch `key_source` to `user` / `org`).
- Helm chart version bumped to `0.6.0`, `appVersion` to `0.6.0`.

## [v0.4.0] — user-defined OpenAI-compatible providers

Splits production deployment out of the public repo into a private
[`fun-fx/ffx_nexus_ops`](https://github.com/fun-fx/ffx_nexus_ops) ops repo
and lets any tenant plug an OpenAI-shaped upstream into the gateway without
us shipping a per-vendor Go adapter.

### Highlights

- **Own your provider.** From *Account → My provider keys (BYOK)* pick
  "Custom (OpenAI-compatible)…", give it a name (e.g. `openrouter`,
  `together`, `fireworks`, `mycorp-llm`), a base URL, and optional
  chat / embed model inventories. Nexus auto-registers a wrapper adapter
  on the next boot and exposes your models at `/v1/models` under
  `user/<provider>/<model>`. The Playground picker uses a live datalist
  bound to `/v1/models` so autocompletion discovers your entries on the
  fly.
- **On-prem repo separation.** The production pipeline (Talos + Cozystack
  + Kaniko + LAN Harbor) now lives in `fun-fx/ffx_nexus_ops` so internal
  identifiers never reach the public release. Verified by end-to-end
  green deploys from the ops repo while and after the public repo was
  made PUBLIC (`v0.3.6` → `main` rewrite → public visibility flip →
  tag-anchored release history).
- **Public repo hygiene.** `git filter-repo` rewrote history to scrub
  private node IP, Tailscale tailnet name, and LAN Harbor host (all
  replaced with `<node-ip>`, `<tailnet>`, and `harbor.<node-ip>.nip.io`
  placeholders or fully removed). 37 stale merged branches deleted,
  `v0.3.6` tag re-pointed at the cleaned commit, Dependabot baseline
  carried forward. **fun-fx/ffx_nexus** is now `visibility: PUBLIC`.

### Added

- `core.CredentialModels{Chat, Embed}` persisted as Postgres JSONB on
  `provider_credentials.provider_credentials` via new migration
  `005_credential_models.sql`. Additive — existing rows stay valid.
- `providers.UserCompat` wrapper around `OpenAICompat` that namespaces
  dynamic model ids under `user/<provider>/<model>` and strips the
  prefix on outbound calls so the upstream sees the raw model id.
- Console: Custom provider field group in *Account → My provider keys
  (BYOK)* (base URL + chat/embed model inventories), credentials table
  shows base url + "N chat / M embed" summary.
- `web/src/api.ts`: `fetchGatewayModels()` returning the `/v1/models`
  catalog grouped by `chat`, `embed`, and `user/<namespace>`.

### Changed

- `cmd/nexus/main.go: registerStoredCredentials`: any credential whose
  `provider` is not one of `openai|anthropic|gemini|groq|mistral|grid`
  falls through to `UserCompat`. base_url is required (logged + skip
  when missing).
- Console model picker (Playground prompt) gains a `<datalist>` backed
  by `/v1/models` for autocompletion.

### Security / boundary

- 1st-party providers and their catalog are unchanged.
- Built-in `provider == "openai"` credentials still go through the
  existing `OpenAI` adapter so third-party OpenAI-compatible endpoints
  do not piggyback on the builtin's catalog without an opt-in model
  inventory.
- BYOK precedence is preserved (`ResolveCredential` still wins
  user-owned over org-level).
- Backward compatibility: `CredentialModels{}` (empty) means "use the
  built-in default catalog", so existing 1st-party credentials behave
  identically.

## [v0.5.0] — Audit log + Onboarding + V1 observability + dev container

Closes the v1.1 design workstream (audit log WS-A, onboarding WS-B) and
ships the V1 observability stack introduced in `docs/scalability-plan.md`
(dev container, OTLP, Prometheus, replica_id). Plus the housekeeping
gaps that were blocking a "fresh clone → reproducible dev setup" path.

### Highlights

- **Audit log coverage** (WS-A of v1.1 design). Every state-changing
  admin/member action now writes to `audit_log` with a canonical action
  enum (`internal/core/audit.go`). The owner-deduped `target_id`,
  detail, and `actor_id` flow through `GET /api/audit` (admin-only)
  with `?action=`, `?user_id=`, `?limit=`, `?since=` filters and an
  RFC3339+duration-flexible parser.
- **Onboarding flow** (WS-B of v1.1 design). A new `users.onboarded_at`
  column (`migrations/postgres/008_onboarded_at.sql`) is stamped the
  first time a member successfully creates a provider credential. The
  React `Account.tsx` surfaces a 3-step `OnboardingChecklist` banner for
  members with `onboarded_at IS NULL`, auto-hiding on first credential
  create. Inline `?` tooltips on credential / virtual-key sections, and
  a copy-pasteable curl snippet rendered after virtual-key creation.
- **V1 observability stack** (scalability V1). `docker compose
  --profile dev` now brings up Grafana, Prometheus, OpenTelemetry
  collector, and (optionally) Metabase in one command. Grafana is
  pre-loaded with eight panels matching the published Prometheus
  queries (latency p50/p95/p99, RPS by model, semantic cache hit rate,
  cost/hour, failover events, BYOK adoption, quality judge score, error
  rate). OTLP collector is wired but **silent** — it requires
  `NEXUS_OTLP_ENABLED=true` and `NEXUS_OTLP_ENDPOINT=...` to forward.
- **ReplicaID on failover alerts** (V4). The router now stamps a
  per-pod `replica_id` (env `NEXUS_REPLICA_ID`, default
  `<hostname>-<randid>`) on every failover event. The Grafana
  `Failover events / hour` panel still sums across all replicas, but a
  px-quick drill-down filter by `replica` now makes flaps attributable.
- **Eval judge → Prometheus gauge** (PR #89). After this PR closing
  the wiring, `nexus_eval_quality_score{model="…"}` is now fed whenever
  the SLM judge (Qwen2.5:7b via Ollama, or any OpenAI-compatible
  endpoint) fires — see Grafana panel 7. The dev compose file ships
  the 4 judge env vars (`NEXUS_JUDGE_BASE_URL`, `MODEL`, `API_KEY`,
  `EVAL_SAMPLE_RATE`) so a fresh `docker compose --profile dev up -d`
  immediately lights up the metric.
- **Metabase Pattern B takeover** (Pattern B production scenario). The
  Metabase BI adapter leaves pre-existing customer-registered
  datasources and collections alone unless it sees a `nexus-managed`
  ownership marker. The `scripts/takeover_metabase.sh`-style
  operator
  workflow updated to stamp the marker idempotently, so adopting a
  customer's existing Metabase instance in production is a one-shot
  PR that does not destroy their dashboards.
- **V5 single-pod ceiling measurement** (PRs #84, #85). The
  `scripts/test_v5_ceiling.sh` script quantifies single-pod capacity
  via `wrk + GOMEMLIMIT=768MiB + GODEBUG=gctrace=1`. Results written
  up in `docs/v5_stress_ceiling_results.md` (p99 80–115 ms at 1000
  concurrent; 23–29 k req/s throughput plateau; linear RSS; no STW
  cycle observed). Deployment-version tuning knobs (`GOMEMLIMIT`,
  `GOGC`, per-vkey concurrency cap) are exposed in `values.yaml`
  under `config.runtime.*` and `config.maxConcurrentPerKey`.
- **Dev container one-command setup** (PR #82). `.devcontainer/`
  brings the `docker compose --profile dev up -d` stack at the
  root of any clone plus a matching Vite dev server for the React SPA.
  Replaces the "zip a tunnel of scripts and README pointers" pattern
  with a single VS Code "Reopen in Container" flow.
- **Repo hygiene cleanup** (PR #90). 9 orphan scripts and
  `cmd/loadgen` removed (no caller anywhere in the tree, no doc
  references, not in CI). The CI integration suite
  (`scripts/test_all.sh` + 13 sub-scripts) is intact and remains the
  authoritative E2E.
- **Helper charts** (`deploy/helm/nexus/Chart.yaml`) bumped to
  `version: 0.5.0` / `appVersion: "0.5.0"` so a default `helm
  install` pulls a current image in lock-step with what the dev container
  runs (previously chart's default was `0.3.3`, more than a major's
  worth behind the binary).

### Backward compatibility

- Audit log: `users.audit_log` table unchanged; new columns are nullable
  so older rows render in `/api/audit` with empty actor_id.
- Onboarding: a fresh `onboarded_at` column added at boot via the
  008 migration — idempotent and nullable for legacy users.
- Observability: existing /metrics surface alias kept; OTLP collector
  stays silent until opted in.
- Helm: chart version `0.3.3 → 0.5.0`. values.yaml keys and structure
  are unchanged — only the *default image tag* moves forward, so an
  existing `helm template | kubectl apply` followed by image pinning
  continues to work.

### Commits & PRs in this release

- PR #79  feat(observability): V1 dev container + OTLP + Prometheus + replica_id
- PR #80  feat(test-tools): loadgen + bound mock upstream + multi-node/stress bench
- PR #81  feat(metabase): Pattern B takeover script + e2e harness + seed dashboards
- PR #82  feat(devcontainer): one-command dev environment
- PR #83  feat(router): stamp ReplicaID on failover alerts
- PR #84  feat(stress): V5 single-pod ceiling measurement + first-pass results
- PR #85  feat(stress): extend V5 ceiling script with RSS + GC sampling
- PR #86  Audit log constants + filter parse + tests
- PR #87  feat(onboarding): mark onboarded_at after first credential create
- PR #88  feat(onboarding): first-run banner + help tooltips + curl code-snippet
- PR #89  feat(evals): propagate quality scores into Prometheus nexus_eval_quality_score
- PR #90  chore(cleanup): drop orphaned scripts and cmd/loadgen
- PR #91  chore(dev): wire SLM judge env by default in dev profile

## [v0.5.1] — Cursor Agent compatibility, raw SSE passthrough, Responses SSE shell

Targets the **Cursor Agent / Cursor Composer** traffic shape that drove the
v0.5.0 pilot. The pilot handoff letter
([`docs/release-notes/v0.1.0.md`](docs/release-notes/v0.1.0.md)) already flagged
that "the gateway must look like a first-class OpenAI + Responses endpoint from
both the public hostname and the API hostname" — this release closes the gap.

### Highlights

- **Cursor Agent "hybrid" bodies.** `POST /v1/chat/completions` now accepts
  Responses-shaped payloads out of the box: top-level `input` (string or
  array), flat Responses function tools, custom-type tools (e.g. `ApplyPatch`),
  `reasoning.effort`, `max_output_tokens`, Responses-only `tool_choice`
  shapes, and the full Responses extras (`store`, `include`,
  `prompt_cache_key`, `metadata`, …). `IsCursorHybridRequest` detects the
  shape by a cheap top-level key scan (no full decode on the hot path), and
  `TransformCursorHybrid` rewrites it to governance-aware
  `ChatCompletionRequest` so virtual-key limits, BYOK, guardrails,
  routing, eval, and quality routing all keep working.
- **Raw SSE passthrough for OpenAI-compatible providers.** OpenAI,
  the OpenAI-compat wrapper, and The Grid now stream the upstream
  Server-Sent Events byte-for-byte when the call is a passthrough-eligible
  model, instead of unmarshal-then-remarshal. Non-OpenAI-standard fields
  (`reasoning_content`, `thinking_blocks`, vendor-specific metadata) survive
  the trip end-to-end. The handler still parses one cheap copy per chunk
  locally for trace metrics, so the dashboard / cost / latency record is
  unaffected.
- **Responses SSE `response.completed` event.** `POST /v1/responses`
  streaming now emits a complete `response.completed` envelope per the
  OpenAI spec — `{id, object:"response", status, model, output[],
  usage, parallel_tool_calls, instructions, tools}` — closes open items
  as `status:incomplete` on truncated streams and emits `response.failed`
  with `trace.error_type=stream_error` when the upstream errors before the
  first chunk. Tool delta `call_id`s round-trip; a stable `call_<uuid>` is
  minted when the upstream never sets one, so parallel tool calls never
  collide on the cumulative `output[]`.
- **Public console vs API hostnames.** When `NEXUS_PUBLIC_GATEWAY_URL` is
  set (e.g. `https://api.nexus.ffx.ai`), the console renders that URL in
  the onboarding curl snippets and PlayGround SDK panel instead of the
  in-process listen address. The console also reverse-proxies `/v1/*` to
  the co-located gateway so the Playground and `/v1/models` discovery stay
  same-origin on the public console hostname; Cursor — which only trusts
  the API hostname — connects to that `NEXUS_PUBLIC_GATEWAY_URL` directly.
- **`/v1/chat/completions` array message content.** Cursor Agent arrays
  its `messages[].content` (text + file parts); the gateway now accepts
  both string and array content shapes per OpenAI's Chat Completions spec.
- **Inline guardrails: `maxInputChars` default raised.** The full-profile
  default was lifted from `20_000` to `200_000` bytes (≈ 50k tokens) so a
  single non-ASCII (Korean/emoji) or long-context request from Cursor
  Agent passes the inline guardrail without `403 guardrail_blocked`. The
  MaxInput in `.env.example` now mirrors the full-profile default.

### Added

- `internal/gateway/cursor_compat.go` — `IsCursorHybridRequest` +
  `TransformCursorHybrid` + `parseInputToMessages` + `normaliseTool` +
  `normaliseHybridToolChoice` + `wrapApplyPatchGrammar` +
  `pickResponsesExtras`. Preserves the Responses `tool_choice` hybrid
  shape, keeps `format`/`grammar` keys on `function.parameters.format`
  so ApplyPatch round-trips, and lets promoted Chat keys
  (`parallel_tool_calls`, `tool_choice`, …) not double-publish.
- `internal/gateway/providers/openai.go` — `scanOpenAISSERaw`,
  `parseOpenAISSEWithRaw`, and `sseEvent` buffer that emits `Raw`
  bytes; the handler selects between `parseOpenAISSE` (strict OpenAI)
  and the raw line when the Provider advertises a passthrough-eligible
  model set.
- `internal/console/gateway_proxy.go: SetGatewayProxy` + `loopbackGatewayURL`
  — the console listens for `/v1/*` and proxies to the in-process gateway
  on `127.0.0.1`, so the public console URL can serve `/v1/models` and the
  Playground without an extra hop.
- `internal/console/gateway_proxy.go: SetPublicGatewayURL` —
  env-driven `NEXUS_PUBLIC_GATEWAY_URL` plumbed to the React onboarding
  curl snippet, CSP `connect-src` allowlist, and PlayGround SDK panel.
- `internal/config/config.go: PublicGatewayURL` — new field wired through
  Helm chart's `configMap` via `deploy/helm/nexus/templates/configmap.yaml`.
- `internal/gateway/handler.go` — Cursor-hybrid detection path on
  `/v1/chat/completions`; array message-content shapes on
  `/v1/chat/completions`; Responses streaming `response.completed`
  shape with `instructions`, `tools`, `parallel_tool_calls`,
  `usage`, and OpenAI-spec output items.

### Changed

- `deploy/helm/nexus/Chart.yaml`: `version` bumped to `0.5.1`,
  `appVersion` to `"0.5.1"` so a default `helm install` pulls a current
  image in lock-step with what the dev container runs.
- `deploy/helm/nexus/values-full.yaml`:
  `NEXUS_GUARDRAILS_MAX_INPUT_CHARS` raised from `20000` to `200000`.
- `.env.example`: `NEXUS_GUARDRAILS_MAX_INPUT_CHARS` example value
  switched to `200000`.
- `internal/console/security.go` — CSP `connect-src` allowlists
  `api.<nexus-domain>` so the frontend can fetch the public gateway.
- `web/src/api.ts` — `api.nexus.ffx.ai` is the public-facing gateway
  base for the onboarding curl snippet.

### Fixed

- `internal/gateway/handler.go` — `/v1/chat/completions` no longer 400s
  on Responses-style bodies; Cursor Agent "hybrid" requests succeed.
- `internal/gateway/handler.go` — array message content (`[{"type":"text",…}]`)
  is parsed correctly (was 400'ing from Cursor Composer's file parts).
- `internal/gateway/responses.go` — streaming `response.completed` no
  longer drops `instructions` / `tools` / `parallel_tool_calls`
  fields; truncated streams close items as `incomplete` instead of
  pretending success; tool delta IDs round-trip through the cumulative
  output list.
- `cmd/nexus/main.go` — `PublicGatewayURL` is now sourced from config so
  the Helm chart ConfigMap key is honoured at boot.

### Security / boundary

- Cursor-hybrid detection runs **before** auth, so a malformed body still
  gets the standard 401-vs-400 split the rest of the API surface uses;
  Note: regression-tested against the existing
  `scripts/test_phase2.sh` and `test_phase234.sh` chains — no new tests
  are required to keep them green.
- The raw-SSE passthrough preserves comment / `id:` / `event:` lines
  verbatim so security headers (e.g. `:x-trace-id` style comments) on the
  upstream still surface to the client; no codepath actively forbids them.

### Upgrade notes

None. v0.5.1 is additive — existing clients that already speak
chat-completions or Responses keep working unchanged, and the Cursor
Agent compat is gated on detection (a true Chat body never enters the
hybrid path).

### Commits & PRs in this release

- PR #109 fix(gateway): accept array message content from Cursor Agent
- PR #110 chore(guardrails): raise maxInputChars to 200000 in full profile
- PR #112 feat(streaming, gateway): raw- SSE passthrough agent mode
- PR #113 fix(cursor): bridge Responses-shaped payloads onto Chat Completions
- PR #114 chore(release): bump chart to 0.5.1 for PR #113 gateway fix

## [v0.1.0] — initial strict-byok pilot release

First publicly consumable release. Grid team pilot.

### Highlights

- **Strict-BYOK by default.** Every gateway call uses the calling user's
  own stored provider key. Operator env keys remain loaded for
  visibility but never reach the data path unless the operator opts in
  via `NEXUS_ALLOW_SHARED_KEYS=true`. The "operator pays the bill"
  behavior from v0 is preserved as an explicit, documented escape hatch.
- **Welcome-first dashboard.** Fresh visitors land on a Sign-in panel,
  not on an empty Admin Console. Logged-in users get four tabs:
  *Overview · Playground · Audit (admin) · Account*. Demo data and
  per-user provider keys are wired into the *Playground* lane for one-
  shot chat completion testing without leaving the browser.
- **Byok-strict-byom path** for `the_grid` (The Grid spot-market) is now
  first-class in the dashboard — register a key under
  *Account → My provider keys (BYOK)* like any other provider.
- **Eval-driven model routing** (`model: "auto"` or named groups like
  `fast`/`smart`) is the new default story in docs and the demo.

### Added

- Dual-license: Apache-2.0 for the Go binary and infra, MIT for the
  React/TypeScript dashboard. `LICENSE` and `LICENSE-MIT` are committed
  at the repo root.
- New `scripts/test_strict_byok.sh` covers strict-byok gating and the
  `NEXUS_ALLOW_SHARED_KEYS` escape hatch.
- New dashboard tab: *Playground* — a one-pane chat-completion test
  surface modeled on LiteLLM's in-console playground.
- `docs/release-notes/v0.1.0.md` — pilot-handoff letter for the Grid team.

### Changed

- `NEXUS_KEY_MODE` default flipped from `shared` to `strict_byok`.
  Out-of-the-box, the operator never pays for user usage.
- `scripts/install.sh` no longer hard-codes `NEXUS_KEY_MODE`; it relies
  on the new default.
- README "BYOK & multi-tenancy" subsection: documents the new default,
  adds an opt-in shared-key fallback section.

### Fixed

- `web/src/api.ts`: `fetchMyStats`/`fetchMyTraces`/`fetchMyQuality` no
  longer throw `Unexpected end of JSON input` when the session expires
  between polls.
- `scripts/demo_reset.sh`: brings up the fake embeddings stub and the
  semantic-cache / guardrail env vars so the steps 7–9 of the demo
  walkthrough work on a fresh install.

### Upgrade notes

Existing self-hosters who relied on env-configured provider keys
running *for everyone* should set `NEXUS_ALLOW_SHARED_KEYS=true` before
upgrading to preserve old behavior. New deployments can leave the
default unchanged.

### Known limitations

- Strict-byok requires Postgres + `NEXUS_MASTER_KEY`. Without storage,
  the gateway falls back to `shared` and logs a warning — same as
  before this release.
- Playground uses `sessionStorage` to keep the user's virtual key warm
  across requests within a single tab; closing the tab prompts again.
- The Grid provider (`the_grid`) is not registered by default; it
  enters the registry only after a user registers a BYOK key for it.
