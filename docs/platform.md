<!--
category: concepts
title: What Nexus is built from
summary: Inventory of the product layers, core technology, security controls, and infrastructure that actually ship — and which deeper docs to read next.
order: 5
status: stable
-->
# What Nexus is built from

This page is an inventory of what the repository actually ships: the
product layers, the core technology, the security controls, and the
infrastructure that runs them. It is not a runbook. Install, upgrade,
and Helm recipes live in the linked operations docs.

Nexus is an open-core **LLM gateway**: one OpenAI-compatible API in
front of many providers, traces on the way out, and a console that
configures keys, evals, and routing. The binary is Go. The dashboard
is a React SPA embedded in that binary. Datastores are optional; the
process starts without them and grows capabilities as they are wired.

## Product layers

Four layers share one process. Each answers a different question.

| Layer | Question | Where it lives |
| --- | --- | --- |
| Gateway | Did the call reach a model, and what did we record as cost? | `internal/gateway` on `:8080` |
| Router | Which model should this call use? | `internal/router` |
| Evaluator | Was the answer worth the spend? | `internal/evals`, `internal/evalplugin` |
| Benchmark | Is this still the model we want? | `internal/benchmark`, PrimeIntellect hosted eval |

They compose: quality-aware routing reads rolling `eval_scores` and
`benchmark_runs`. Heuristics can run without ClickHouse; **routing
stats and durable traces need ClickHouse**. Virtual keys, BYOK, SSO,
and the console need **Postgres**. Cross-replica RPM and budgets need
**Redis**.

Deeper: [Quickstart](quickstart.md), [Gateway API](gateway-api.md),
[Evals and routing](evals-and-routing.md),
[Model benchmarks](model-benchmarks.md).

## Core technology

### Runtime

- **One Go binary** (`cmd/nexus`): gateway `:8080` and console `:8081`.
  No required sidecar. `nexus serve --local` starts a private Postgres
  under `~/.nexus` so `npx` / `docker run` get a working console.
- **React/TypeScript console** (`web/`), MIT-licensed, embedded via
  `web/embed.go`. Docs in `/docs` are the same Markdown the console
  Docs tab and `GET /api/docs` serve.
- **Optional Python eval sidecar** (`eval-service/`): DeepEval / RAGAS.
  Not seeded in plugin-only clusters; kept for back-compat.

### Data plane (stateless)

The request path does not keep authoritative state in the process.
Replicas sit behind a load balancer. Rate limits and budgets go to
Redis (or in-memory on a laptop). Traces and scores go to ClickHouse.
Credentials and keys go to Postgres.

### Wire formats

The gateway speaks stock SDK shapes so callers do not take a Nexus
SDK:

- OpenAI chat, Responses, embeddings, moderations, images
- Anthropic Messages (`POST /v1/messages`)
- Cursor Agent hybrid bodies rewritten to chat
- Raw SSE passthrough so non-standard stream fields survive
- MCP proxy: register servers, `tools/list` / `tools/call`, dedicated
  ClickHouse `mcp_tool_logs`

All of those share the same auth, RPM/budget, and BYOK chain.
Errors on `/v1` stay OpenAI-shaped; console `/api` uses a Nexus
error envelope plus `X-Request-Id`. See
[Error response contract](error-response-contract.md).

### Providers

Built-in catalogs: OpenAI, Anthropic, Gemini, Groq, Mistral, The Grid,
and any OpenAI-compatible base URL the console registers. Default
mode is **`strict_byok`**: the caller’s credential, not an org-wide
env key. Server-side keys (`OPENAI_API_KEY`, `GRID_API_KEY`, …) are
opt-in fallbacks. Grid 307 redirects strip `Authorization` on
cross-origin hops. See [Providers](providers.md).

### Cost

Each completed call writes `cost_usd` on the ClickHouse
`gateway_traces` row. That is what Spend and Overview sum.

- **Most vendors** have no per-call dollar on the wire. Nexus uses
  the embedded table in `internal/gateway/pricing.go` (tokens × list
  rates, with family aliases for versioned ids).
- **The Grid** is a spot market. When `usage.estimated_cost` is
  positive, that value wins. When it is missing, the static
  `grid/code-prime` (and sibling) rates apply — which can diverge
  from what Grid actually invoiced.
- **LiteLLM’s public JSON** is polled at boot and every 6 hours
  (`NEXUS_PRICING_CHECK`, default on). A mismatch is a log,
  Prometheus gauge, admin API, and console banner. It does **not**
  rewrite the table or historical rows.

Prompt and completion **bodies** are stripped before durable storage
unless `NEXUS_CAPTURE_TRACE_CONTENT=true`. Cost, tokens, and latency
are still stored.

### Evals and routing

Two modes, not a zoo of evaluator kinds:

1. **`heuristic`** — in-process, zero egress. Closed names:
   `contains`, `pii`, `exact_match`, `rouge_l`, `hf_evaluate`,
   `lighteval`, `ragas` (the last of those may shell out to Python).
2. **`external`** — YAML plugin, OTLP send, webhook or poll collect.
   Closed adapters: Langfuse, LangSmith, Datadog, Braintrust, Arize,
   Confident AI, Arize Phoenix, OTEL collector, webhook.

`NEXUS_EVAL_PLUGIN_ONLY` skips seeding built-in PII/completeness
profiles. Pair with `NEXUS_EVAL_PURGE_LEGACY_PROFILES_ON_BOOT` only
when an external plugin covers PII. Guardrails (PII, deny lists,
JSON schema, optional self-correction) and a semantic cache (Redis +
embeddings) sit on the request path; heavy scoring does not.

## Security

The security review contract is
[Self-hosted security](customer-self-hosted-security.md) and
[Tenancy model](tenancy-model.md). Short version of what is enforced:

| Control | What it does |
| --- | --- |
| Virtual keys | Callers hold `nxs_live_…`. Provider secrets never go to the client or into logs. |
| Encryption at rest | Provider credentials: AES-256-GCM under `NEXUS_MASTER_KEY`. Virtual keys stored as SHA-256. |
| Tenancy | One customer = one install. Orgs inside an install are an **authorization** boundary in SQL, not a separate process or key. Need infra isolation → two installs. Session org always wins over `X-Org-Id`. |
| Roles | Admin vs member. Audit log is append-only; action constants are closed. |
| Browser | Cookie session, CSRF, CSP `connect-src` from `NEXUS_PUBLIC_WEB_ORIGINS`. Same-origin needs no allowlist. Cookie `Secure` is **not** taken from `X-Forwarded-Proto`. |
| Source IP | Socket `RemoteAddr` is canonical. `X-Forwarded-For` only with `trustedProxyCIDRs`, walk right-to-left, hop cap. |
| Capture | Bodies off by default on the ClickHouse writer. In-process evals still see the live trace. |
| Egress | Chart default-deny NetworkPolicy in `profile=enterprise`. Install refuses until every peer is named and `enforcementAcknowledged` is set. |
| Fail-closed eval | Plugin-only mode will not silently re-seed heuristic PII. |

What it does **not** claim: Nexus is not a WAF, not a secret manager,
and an attacker with code execution in the pod is outside the org
boundary. See also [IP exposure](ip-exposure.md),
[Audit fail-stop](audit-failstop-policy.md),
[NetworkPolicy prerequisites](network-policy-prerequisites.md).

## Infrastructure

### How you run it

| Path | What you get |
| --- | --- |
| `npx -y @ffxnexus/nexus` / `docker run` / install.sh | Local Postgres, console, no ClickHouse/Redis. Traces live-only. |
| `deploy/docker-compose.yml` | Dev/full profiles: Postgres, ClickHouse, Redis, optional Ollama, Grafana, Metabase. |
| Helm `deploy/helm/nexus` | One container, probes, non-root, optional Ingress/HPA/PDB. **Does not install databases.** |
| `fun-fx/ffx_nexus_ops` (private) | Maintainer prod: Talos + Cozystack, in-cluster Kaniko, Harbor, `tenant-nexus`. |

Images: `ghcr.io/fun-fx/ffx_nexus` on `v*` tags, plus GitHub Release
binaries. Chart `image.tag` should be pinned. Migrations:
`nexus migrate` (Postgres and/or ClickHouse); the Helm hook applies
them on upgrade. Additive SQL only.

### Cluster shape (typical production)

```
clients  →  ingress / tunnel
              →  nexus pod  :8080 gateway  :8081 console
                    →  Postgres   keys, users, plugins, audit
                    →  ClickHouse traces, eval_scores, mcp_tool_logs
                    →  Redis      RPM, budgets, semantic cache
                    →  upstream   OpenAI / Anthropic / Grid / …
                    →  optional   OTLP collector, Grafana, Metabase
```

The data plane stays horizontally scalable because the pod is
stateless. ClickHouse memory, not the language runtime, is what
fails first on wide 30-day dashboard aggregates.

### Observability plumbing

Independent sinks behind `observability.Recorder`: ClickHouse,
live WebSocket hub, Prometheus (`NEXUS_METRICS_ADDR`), OTLP/HTTP
(`gen_ai.*`, plus `gen_ai.evaluation.result` when scoring), Metabase
bootstrap. One sink dying does not disable the others.
[Observability](observability.md).

### Network policy (D-2b)

Enterprise profile is fail-closed: default-deny, named ingress
(Prometheus, ingress controller) and egress (Postgres, ClickHouse,
Redis, DNS, named LLM/OIDC peers). Enforcement is only as real as
the CNI; the chart cannot prove Cilium/Calico for you. Reports and
scenario gates live under `docs/phase-d2b-*` and
`docs/d2b-final-report.md`.

## Package map

```
cmd/nexus               serve, migrate, local Postgres supervisor
internal/gateway        providers, streaming, cost, MCP proxy, guardrails
internal/router         quality-aware pick, failover, alert sinks
internal/evals          worker, heuristics, plugin dispatch
internal/evalplugin     YAML decode, closed vendor enum
internal/observability  traces, spend readers, dashboard series, capture gate
internal/core           users, virtual keys, credentials
internal/limiter        Redis / memory RPM and budgets
internal/console        /api, auth, SSO, audit, spend, eval UI
internal/mcp            MCP client + registry
internal/benchmark      hosted PrimeIntellect runs
internal/localdb        embedded Postgres for --local
internal/egress         outbound HTTP (pricing catalog, plugins)
migrations/             postgres/ + clickhouse/
deploy/helm/nexus       chart + NetworkPolicy + migration job
web/                    console SPA
npx/                    release-binary wrapper
```

## Where to read next

| If you need | Read |
| --- | --- |
| Install in a customer cluster | [Self-hosted install](customer-self-hosted-install.md) |
| What a security review expects | [Security](customer-self-hosted-security.md), [Tenancy](tenancy-model.md) |
| Every env var | [Configuration](configuration.md) |
| Upgrade / rollback | [Upgrade and rollback](customer-self-hosted-upgrade-rollback.md) |
| Helm trial | [Kubernetes](kubernetes.md) |
| What “enterprise” means (no licence key) | [Enterprise](enterprise.md) |
| Console Eval / Benchmark tabs | [Eval tab](eval-tab.md), [Benchmark tab](benchmark-tab.md) |
| Cutting a release | [Development](development.md) |
| Go vs Elixir cutover (Manus briefing) | [Elixir cutover inventory](elixir-cutover-inventory.md) |
