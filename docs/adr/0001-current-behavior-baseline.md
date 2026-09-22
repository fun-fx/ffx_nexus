<!--
category: concepts
title: Current behavior baseline
summary: Pins the Go binary, chart, schema ledger, tenancy defaults, and golden HTTP/SSE contracts that any rewrite must match before it can serve traffic.
order: 91
status: stable
-->

# ADR 0001 — Current behavior baseline

**Status:** accepted (Phase 0 pin)
**Date:** 2026-09-18
**Deciders:** Nexus maintainers

## Context

Nexus production is a Go binary in this repository. An Elixir rewrite
lives in a **sibling** repository (`fun-fx/nexus_ex`) and is not
production until it has passed a canary. Until that cutover, this
repository is the executable spec: request path, schema, error shapes,
and SSE wire bytes.

This ADR records what “current behavior” means so a second agent can
implement the Elixir vertical slice **without reading Go packages as
the spec**.

## Decision

1. Keep `fun-fx/ffx_nexus` as the public production repo and contract
   oracle. Do not replace it in place.
2. Land language-neutral artifacts under `priv/fixtures/contracts/`
   and this ADR. The Go request path does not change in Phase 0.
3. Elixir consumes those fixtures by **pinned commit SHA** (vendored
   copy or submodule). Fixture drift is resolved here first, then
   re-pinned.
4. Stop the rewrite if the Elixir OpenAI chat SSE slice fails the
   P1-3 non-inferiority gate in `scripts/load_baseline`. Do not spend
   later phases on a slower proxy.

## Baseline pin (this tree)

| Item | Value |
| --- | --- |
| Go commit this ADR describes | see [`priv/fixtures/contracts/PIN.json`](../../priv/fixtures/contracts/PIN.json) |
| Helm chart | `deploy/helm/nexus` **0.8.4** (`appVersion` 0.8.4) |
| Default image | `ghcr.io/fun-fx/ffx_nexus` tagged with chart `appVersion` or git SHA (ops CD) |
| Schema ledger | Postgres + ClickHouse table `schema_migrations`; IDs `engine/NNN_name.sql` |
| Reconstruct empty DB | concatenate `migrations/{postgres,clickhouse}/*.sql` in ordinal order (already dumped to `priv/fixtures/contracts/schema/`) |
| `nexus migrate --check` | exit 0 = ledger current; exit 2 = outstanding; exit 1 = failure. Apply is `nexus migrate` (Helm pre-upgrade hook). |

Re-pin `PIN.json` whenever a migration file or a golden fixture
changes. Do not edit an already-applied `migrations/**/*.sql` file;
add a new ordinal.

## Live defaults that docs still fight

These are the values the **binary** uses today. Older docs or operator
muscle memory still argue the opposite. The rewrite must copy the
binary, not the folklore.

| Topic | Binary default | Folklore / trap |
| --- | --- | --- |
| Provider keys | `NEXUS_KEY_MODE=strict_byok`. Env/org keys never reach the data path unless `NEXUS_ALLOW_SHARED_KEYS=true`. | “Shared env keys work out of the box.” That was pre-v0.1.0. |
| Tenancy on console | Session `User.OrgID` wins. `X-Org-Id` is **ignored** once a session exists. The header is only for pre-login (`POST /api/auth/login`, `/api/auth/register`). Empty → `default`. | Sending `X-Org-Id` from a logged-in client to hop orgs. That was a cross-team authz bug and is closed. |
| Signup | `NEXUS_ALLOW_SIGNUP=false`. First local-mode user is admin; production expects bootstrap / invite. | Public register enabled. |
| Capture bodies | `NEXUS_CAPTURE_TRACE_CONTENT=false`, `NEXUS_CAPTURE_MCP_CONTENT=false`. | Traces contain prompts by default. |
| Auto-migrate | `NEXUS_AUTO_MIGRATE=false` (true only in `--local`). Production uses the migrate Job + boot `--check`. | Pods migrate themselves on boot. |
| Eval plugin-only | `NEXUS_EVAL_PLUGIN_ONLY=false`. Built-in heuristic PII/completeness seed unless this is set. Destructive companion `NEXUS_EVAL_PURGE_LEGACY_PROFILES_ON_BOOT` must be paired explicitly. | In-cluster Python eval is always on the critical path. Confirm the live cluster before scheduling a Python sidecar rewrite (Phase 6). |
| Pricing | `CostUSD` from upstream `usage.estimated_cost` when &gt; 0, else embedded `pricingTable`. LiteLLM JSON is **drift alert only** (`NEXUS_PRICING_CHECK`, default true). | “We bill LiteLLM prices.” |

## Contract surfaces (fixtures, not Go)

| Surface | Where the gold lives | Rule |
| --- | --- | --- |
| OpenAI chat SSE | `priv/fixtures/contracts/sse/` | Entity-body concat, SHA-256. Unknown JSON fields, comments, `event:`/`id:`, CRLF, truncation, `[DONE]` survive as **raw bytes**. No JSON re-encode. |
| `/v1` errors | `priv/fixtures/contracts/errors/v1_openai.json` | `{ "error": { "message", "type" } }`. `X-Request-Id` on the header, not in the body. |
| `/api` errors | `priv/fixtures/contracts/errors/api_nexus.json` | `apierr.Body`: `{ "error": { "code", "message", "request_id" } }`. |
| Mid-stream errors | `priv/fixtures/contracts/errors/sse_comment.sse` | `: stream error` + `: nexus-request-id=<id>` after `text/event-stream` has started. |
| First-byte failover | `priv/fixtures/contracts/failover/first_byte.md` | Fallback only **before** the first response byte. Failed candidate traces `upstream_error_failover`. |
| Redis limits | `priv/fixtures/contracts/schema/redis.md` | Key layout + INCR/EXPIRE semantics (Go uses a MULTI pipeline, not Lua today). |
| Postgres / ClickHouse | `priv/fixtures/contracts/schema/*.sql` | Existing SQL is the ledger. Do not invent a parallel Ecto schema as the public protocol. |

Normalization in goldens: timestamps `1720000000` / `1970-01-01T00:00:00Z`;
request ids `req_golden`. Compare SHA-256 of the body after substituting
those tokens, never wall-clock values.

## Ports and process

| Port | Role |
| --- | --- |
| `:8080` | Gateway (`/v1`, `/v1beta`, `/healthz`) |
| `:8081` | Console (`/api`, SPA, `/readyz`) |

One process, two listeners. Elixir Phase 1 must boot the same pair
even if `:8081` only serves health.

## What this ADR does not decide

- Elixir is not the serving binary.
- LiveView, Horde, libcluster, custom NIFs, and Python/React removal
  are later phases and are blocked on the P1-3 load gate.
- Dual-write of Redis or Postgres from Go and Elixir on the same
  command is forbidden.
- Cutover (Phase 8) is a load-balancer canary, **never** Go-proxy-to-Elixir.

## Consequences

- Security and feature work continues in this Go tree.
- `nexus_ex` CI must not be a required check on this repo’s `main`
  until Phase 1 exists and the fixture pin is wired.
- Changing a golden file is a breaking change for the rewrite and
  needs a PIN bump plus an Elixir fixture refresh.
