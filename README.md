# Nexus

**An open-core LLM gateway that scores its own traffic and routes on the
result.** One OpenAI-compatible API in front of every provider, OpenTelemetry
GenAI traces out the back, and a live console you can actually configure
things in.

Single Go binary. No sidecars, no agent, no vendor SDK.

---

## Quick start

**Zero to a working gateway in under a minute.**

**Step 1:** Start Nexus

```bash
# Install and run locally
npx -y @ffxnexus/nexus

# Or use Docker
docker run -p 8080:8080 -p 8081:8081 -v "$PWD/data:/app/data" ghcr.io/fun-fx/ffx_nexus

# Or the curl installer
curl -fsSL install.nexus.ffx.ai | bash
```

Each of these downloads a single binary and starts it with its own database,
so nothing else needs to be installed or configured.

**Step 2:** Configure it in the console

```bash
open http://localhost:8081
```

Create an account — the first one on your machine is the admin — then add a
provider key and mint a virtual key.

**Step 3:** Make your first API call

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer nxs_live_..." \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "gpt-4o-mini",
    "messages": [{"role": "user", "content": "Hello, Nexus!"}]
  }'
```

Or drop it into an existing app with one line:

```diff
- base_url = "https://api.openai.com/v1"
+ base_url = "http://localhost:8080/v1"
```

**What local mode is and is not.** It runs a private Postgres next to the
gateway and keeps everything under `~/.nexus`, which is what makes the console
work without setup. Traces are live-only and rate limits are per-process,
because ClickHouse and Redis are deliberately not part of a laptop install.
For anything shared, see [Kubernetes](docs/kubernetes.md).

---

## Enterprise deployments

Nexus is built to run inside your perimeter. Self-hosted deployments add
strict-BYOK credential isolation, per-tenant boundaries, OIDC SSO, an
append-only audit log, and a fail-closed NetworkPolicy profile that refuses to
install until every egress peer is named.

Prompts and completions are stripped before durable storage unless you
explicitly turn capture on, and provider credentials are encrypted with a key
you hold.

**[Book a demo](https://nexus.ffx.ai/demo)** ·
[Enterprise capabilities](docs/enterprise.md) ·
[Self-hosted install](docs/customer-self-hosted-install.md)

---

## Key features

### Gateway

- **One API for every provider** — OpenAI, Anthropic, Gemini, Groq, Mistral,
  The Grid, and any OpenAI-compatible endpoint, behind one wire format
- **Drop-in replacement** — chat, embeddings, Responses, moderation, images,
  and streaming, including Cursor Agent's hybrid request shape
- **Raw SSE passthrough** — non-standard streaming fields survive the trip
  instead of being normalised away
- **Automatic fallback** — provider and model failover with alert sinks

### Observability

- **OpenTelemetry GenAI semantics** — `gen_ai.*` traces export to any OTLP
  backend without remapping, so there is nothing to migrate off
- **Pluggable sinks** — ClickHouse, Prometheus, OTLP, a live WebSocket feed
  and Metabase compose independently; one failing does not disable the others
- **Per-user quality and spend** — not just aggregate latency

### Evals and routing

- **Quality-aware routing** — model selection weighted by measured eval
  quality, cost and latency rather than a static preference list
- **Async evals** — heuristics in-process, LLM-as-judge and external scorers
  off the hot path
- **External eval plugins** — Langfuse, LangSmith, Datadog, Braintrust, Arize,
  Confident AI, Phoenix and OTLP collectors, declared as YAML
- **Semantic cache** and **inline guardrails** — PII, deny patterns, JSON
  schema validation with optional self-correction

### Control plane

- **Virtual keys** with per-key RPM limits and monthly budgets
- **BYOK** — tenants bring their own provider credentials, encrypted at rest
- **SSO (OIDC)**, role separation, invites, and an append-only audit log

---

## Documentation

| Guide | What is in it |
| --- | --- |
| [Quickstart](docs/quickstart.md) | How the gateway, router, evaluator and benchmark fit together |
| [Gateway API](docs/gateway-api.md) | Endpoints, streaming, SDK compatibility |
| [Configuration](docs/configuration.md) | Every environment variable |
| [Providers](docs/providers.md) | Adding and opting into provider catalogs |
| [Control plane](docs/control-plane.md) | Keys, BYOK, tenancy, SSO, budgets |
| [Evals and routing](docs/evals-and-routing.md) | Scoring, routing, guardrails |
| [Observability](docs/observability.md) | Trace sinks, Prometheus, Metabase, BI |
| [Kubernetes](docs/kubernetes.md) | Helm chart quick tour |
| [Self-hosted install](docs/customer-self-hosted-install.md) | Production install, end to end |
| [Enterprise](docs/enterprise.md) | What a supported deployment includes |
| [Development](docs/development.md) | CI, local parity, cutting a release |

---

## Architecture

- **Language**: Go, single stack. Stateless data plane designed for horizontal
  autoscaling; the core boots with zero dependencies.
- **Standard**: traces use OpenTelemetry GenAI semantic conventions (`gen_ai.*`)
  so they export to any OTLP backend without remapping — no lock-in.
- **Stores**: ClickHouse (traces/scores), Postgres (control plane),
  Redis (cache/limits). All three are optional; the gateway degrades rather
  than refusing to start.
- **Evals**: heavy eval (LLM-as-judge) runs async, off the request hot path.

```
cmd/nexus               single binary (gateway :8080 + console :8081)
internal/gateway        OpenAI-compatible API, provider adapters, streaming, middleware
internal/observability  gen_ai.* traces -> ClickHouse + live hub
internal/core           control plane: virtual keys + encrypted credentials (Postgres)
internal/localdb        the private Postgres behind `nexus serve --local`
internal/limiter        per-key RPM rate limits + monthly budgets (Redis / in-memory)
internal/evals          async eval worker: heuristics + SLM judge + remote eval client
internal/router         quality-aware model selection (eval quality + cost + latency)
internal/console        dashboard API + WebSocket live feed
eval-service/           optional Python sidecar: DeepEval + RAGAS (async, out-of-band)
web/                    React/TS dashboard, embedded into the binary
npx/                    the `npx @ffxnexus/nexus` wrapper
migrations/             SQL (ClickHouse + Postgres), applied by `nexus migrate`
deploy/                 Helm chart + local docker-compose
```

Container images are published to `ghcr.io/fun-fx/ffx_nexus` and binaries to
[GitHub Releases](https://github.com/fun-fx/ffx_nexus/releases) on every `v*`
tag. Full project description: [DESCRIPTION.md](DESCRIPTION.md).

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and
[docs/development.md](docs/development.md) for the development setup, the
checks CI runs, and how to run them locally.

---

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
summary above is not.
