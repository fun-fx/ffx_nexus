# Changelog

All notable changes to Nexus are documented in this file. The format is
loosely based on [Keep a Changelog](https://keepachangelog.com), and the
project adheres to [Semantic Versioning](https://semver.org/) for the
Go gateway binary.

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
