# Changelog

All notable changes to Nexus are documented in this file. The format is
loosely based on [Keep a Changelog](https://keepachangelog.com), and the
project adheres to [Semantic Versioning](https://semver.org/) for the
Go gateway binary.

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
- New recorded demo (`docs/demo-script.md`, `docs/demo-script.ko.md`)
  walks through signup → virtual key → cache badge → PII guardrail →
  auto routing in under five minutes.
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
