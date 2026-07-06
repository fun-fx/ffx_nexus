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
