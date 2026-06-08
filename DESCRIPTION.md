# Nexus — Project Description

**Nexus** is an open-core LLM gateway and router designed to be **observability-first**, with **eval-driven quality-aware routing** as its core differentiator. It provides a single Go binary that exposes an OpenAI-compatible API, unifies access to multiple LLM providers, and ships built-in tracing, evaluation, and intelligent model selection — without vendor lock-in.

---

## Positioning

Nexus sits in the same category as [Bifrost](https://www.getmaxim.ai/bifrost) and [LiteLLM](https://github.com/BerriAI/litellm), but differentiates on:

| Capability | Nexus approach |
| --- | --- |
| **Observability** | First-class, OpenTelemetry GenAI semantic conventions (`gen_ai.*`), ClickHouse persistence, live WebSocket feed |
| **Evaluations** | Async out-of-band workers (never on the hot path); heuristics + optional local SLM judge |
| **Routing** | Quality-aware: blends rolling eval scores, cost, and latency — not just failover or round-robin |
| **Key management** | Virtual keys (downstream) + encrypted provider credentials (upstream), tied to policy |
| **Deployment** | Zero-dependency mode (env keys only) or full control plane; stateless, horizontally scalable |

---

## Architecture

```
                    ┌─────────────────────────────────────────┐
                    │              Nexus (Go binary)           │
                    │  ┌─────────────┐    ┌─────────────────┐ │
  Client ──────────►│  │  Gateway    │    │  Console API    │ │
  (OpenAI SDK)      │  │  :8080      │    │  :8081          │ │
                    │  └──────┬──────┘    └────────┬────────┘ │
                    │         │                      │          │
                    │  ┌──────▼──────────────────────▼──────┐  │
                    │  │  Observability + Evals + Router    │  │
                    │  └──────┬──────────────────────┬──────┘  │
                    └─────────┼──────────────────────┼─────────┘
                              │                      │
         ┌────────────────────┼──────────────────────┼──────────────┐
         ▼                    ▼                      ▼              ▼
   OpenAI /            ClickHouse              Postgres         Redis
   Anthropic /         (traces +               (virtual keys,   (rate limits +
   Gemini               eval_scores)            credentials)     budgets)
```

- **Language**: Go single stack (performance, operational simplicity).
- **Data plane**: Stateless; safe for horizontal autoscaling behind a load balancer.
- **Control plane**: Optional Postgres-backed store for virtual keys and encrypted provider credentials.
- **Observability store**: ClickHouse for high-volume trace and eval score ingestion.
- **Dashboard**: React/TypeScript SPA (`web/`) consuming the console API.

---

## Implemented Features (Phases 1–4)

### Phase 1 — Gateway + Observability + Dashboard

- **OpenAI-compatible API**: `/v1/chat/completions` (streaming + non-streaming), `/v1/models`.
- **Provider adapters**: OpenAI, Anthropic, Gemini with prefix routing (`provider/model`).
- **Tracing**: Every request recorded with OpenTelemetry GenAI-aligned fields (`gen_ai.*`).
- **ClickHouse persistence**: Buffered batch inserts; non-blocking on the request path.
- **Live dashboard**: WebSocket hub for real-time traces; REST API for stats and history.
- **Zero-dependency mode**: Boots without Postgres, ClickHouse, or Redis.

### Phase 2 — Control Plane (Keys & Credentials)

- **Virtual keys** (client → gateway):
  - SHA-256 hashed at rest; plaintext shown once at creation.
  - Per-key policies: `allowed_models`, `rpm_limit`, `monthly_budget_usd`, `min_quality_score`.
  - Bearer token authentication on all gateway routes.
- **Provider credentials** (gateway → upstream):
  - AES-256-GCM envelope encryption under `NEXUS_MASTER_KEY`.
  - Stored in Postgres; loaded and decrypted at startup.
  - Console CRUD API; audit log for all control-plane actions.
- **Rate limits & budgets**:
  - RPM enforcement → `429 Too Many Requests`.
  - Monthly budget enforcement → `402 Payment Required`.
  - Redis-backed counters (shared across replicas) or in-memory fallback (single-node).

### Phase 3 — Async Evaluations

- **Worker architecture**: Implements `observability.Recorder`; enqueues completed traces on background goroutines.
- **Heuristic evaluators** (always on, sub-ms):
  - `heuristic_pii` — detects email, SSN, phone, card patterns in model output.
  - `heuristic_completeness` — flags empty responses and `finish_reason=length` truncation.
- **SLM judge** (sampled, optional):
  - OpenAI-compatible local inference (Ollama / vLLM).
  - Scores response quality 0..1; configurable via `NEXUS_EVAL_SAMPLE_RATE`.
  - Keeps trace content on-prem for data privacy.
- **External eval service** (sampled, optional):
  - Python sidecar (`eval-service/`) running DeepEval + RAGAS via FastAPI/async.
  - Go `RemoteEvaluator` calls it over HTTP from the worker — out-of-band, sample-gated.
  - Metrics: answer relevancy, toxicity, bias (trace-only); hallucination, faithfulness (when contexts supplied via `nexus_eval` on the request).
  - **RAG context**: clients send `nexus_eval: { contexts, reference }` on chat completions; stored on traces (ClickHouse) and forwarded to the sidecar. Not sent upstream.
  - **Failure isolation**: a slow/down sidecar skips metrics and degrades to Go heuristics; the gateway hot path is unaffected.
  - Reuses the same local judge (Ollama/vLLM) by default; embeddings endpoint optional for RAGAS.
- **Worker concurrency**: configurable via `NEXUS_EVAL_WORKERS` (default 4).
- **Storage**: Results written to ClickHouse `eval_scores` table (`evaluator` = `deepeval`/`ragas` for remote scores).

### Phase 4 — Quality-Aware Routing

- **Routing aliases**: Send `model: "auto"` or named groups (`fast`, `smart`, etc.).
- **Selection algorithm**: Weighted blend of rolling eval quality, average cost, and latency (min-max normalized).
- **Quality signal**: Combines LLM-as-judge quality and heuristic safety pass rate (PII/completeness), so routing reacts to evals even when the judge is disabled.
- **Exploration**: Models without stats receive optimistic quality scores to ensure traffic for cold-start measurement.
- **Policy enforcement**: Virtual key `allowed_models` filters candidates; `min_quality_score` drops models below the threshold (request rejected with `503 no_model_meets_quality` if none qualify).
- **Observability**: `GET /api/routing` exposes current per-model stats (incl. blended `eff_quality`) used for decisions.

### CI/CD

- **CI** (every push/PR): `gofmt`, `go vet`, `go test -race`, Go build, web dashboard build.
- **Integration** (push/PR/manual): Docker Compose + `./scripts/test_phase234.sh` (12 E2E cases).
- **Release** (tag `v*`): Multi-stage Docker image pushed to `ghcr.io/fun-fx/ffx_nexus`.

---

## Repository Layout

```
cmd/nexus/                 Entry point — orchestrates all subsystems
internal/gateway/          HTTP handlers, middleware, provider registry
internal/gateway/providers OpenAI, Anthropic, Gemini adapters
internal/observability/    Trace model, ClickHouse recorder/reader, live hub
internal/core/             Postgres store, virtual keys, credentials
internal/core/crypto/      AES-GCM encryption, SHA-256 key hashing
internal/limiter/          Redis + in-memory rate limiter and spend tracker
internal/evals/            Async eval worker, heuristics, SLM judge, remote eval client, CH sink
internal/router/           Quality-aware model selection, ClickHouse stats
internal/console/          Dashboard REST API + WebSocket + admin endpoints
internal/config/           Environment-based configuration (.env loader for dev)
eval-service/              Optional Python sidecar: DeepEval + RAGAS (FastAPI, async)
web/                       React/TypeScript dashboard (Vite)
migrations/                Postgres + ClickHouse SQL schemas (embedded via go:embed)
deploy/                    docker-compose.yml (ClickHouse, Postgres, Redis, Ollama, eval-service)
scripts/                   E2E test scripts
.github/workflows/         CI, Integration, Release GitHub Actions
Dockerfile                 Production container image
```

---

## API Surface

### Gateway (`:8080`)

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/healthz` | Health check |
| `POST` | `/v1/chat/completions` | Chat completion (stream + unary) |
| `GET` | `/v1/models` | List available models |

Authentication: `Authorization: Bearer <virtual_key>` (when Postgres is configured).

### Console (`:8081`)

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/healthz` | Health check |
| `GET` | `/api/stats?window=1h` | Aggregate metrics |
| `GET` | `/api/traces?limit=100` | Recent traces |
| `GET` | `/api/routing` | Per-model routing stats |
| `GET` | `/api/live` | WebSocket live trace feed |
| `GET/POST` | `/api/keys` | Virtual key management |
| `DELETE` | `/api/keys/{id}` | Revoke virtual key |
| `GET/POST` | `/api/credentials` | Provider credential management |
| `POST` | `/api/credentials/{id}/rotate` | Rotate a provider secret (re-encrypt, hot-reload) |
| `DELETE` | `/api/credentials/{id}` | Delete credential |

---

## Configuration

All settings are environment variables. See [`.env.example`](.env.example) and the [README configuration table](README.md#configuration).

Key variables:

| Variable | Purpose |
| --- | --- |
| `NEXUS_GATEWAY_ADDR` / `NEXUS_CONSOLE_ADDR` | Listen addresses |
| `NEXUS_POSTGRES_URL` | Control plane (keys + credentials) |
| `NEXUS_CLICKHOUSE_URL` | Trace + eval persistence |
| `NEXUS_REDIS_URL` | Shared rate limits + budgets |
| `NEXUS_MASTER_KEY` | KEK for credential encryption |
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` / `GEMINI_API_KEY` | Provider keys (env fallback) |
| `NEXUS_JUDGE_BASE_URL` / `NEXUS_JUDGE_MODEL` | Local SLM judge |
| `NEXUS_ROUTE_GROUPS` | Named routing aliases |
| `NEXUS_ROUTE_W_QUALITY` / `_W_COST` / `_W_LATENCY` | Routing weights |

---

## Testing

### Unit tests

```bash
go test -race ./...
```

Covers: limiter (RPM + spend), gateway enforcement middleware (429/402), eval heuristics, router selection logic.

### E2E integration

```bash
docker compose -f deploy/docker-compose.yml up -d postgres clickhouse redis
./scripts/test_phase234.sh
```

12 test cases across Phases 2b–4:

- Auth (401), RPM limit (429), budget exhaustion (402)
- Upstream completion, heuristic eval scores in ClickHouse
- Routing API, `model: auto`, `model: fast` group alias

Provider API keys are optional for enforcement tests; set `GEMINI_API_KEY` for full upstream coverage.

---

## Roadmap (Not Yet Implemented)

- Open-core packaging: Helm chart, OSS vs commercial feature split, single-command self-hosting (Phase 5)

### Recently completed

- Credential rotation API: `POST /api/credentials/{id}/rotate` re-encrypts a new secret in place (same credential id/provider/name), records `rotated_at` and a `credential.rotate` audit event, and hot-reloads the affected provider so the new key takes effect without a gateway restart.

- Eval-driven routing loop: heuristic safety pass rate now feeds model selection alongside judge quality.
- `min_quality_score` enforcement on routing aliases.
- Provider fallback: routing aliases try candidates best-first and fail over on upstream errors.
- Inline guardrails (hot path): PII/regex/length input blocking (pre-upstream) and PII output redaction, synchronous and datastore-free.
- External Python eval service: DeepEval + RAGAS sidecar wired via an async, sample-gated, failure-isolated `RemoteEvaluator` — richer metrics without touching the Go hot path.
- RAG eval context: clients pass `nexus_eval.contexts` / `reference` on chat completions; stored on traces and forwarded to the eval sidecar for online hallucination/faithfulness metrics.
- Schema/JSON output guardrail (hot path): when a request sets a JSON `response_format`, the output is validated as JSON and against the supplied JSON Schema; non-streaming violations are blocked with `422 schema_validation_failed`.
- Offline regression eval batch (`cmd/nexus-evalbatch`): runs a JSONL dataset through the Python eval service (no sampling), aggregates per-metric scores, and fails CI when scores regress beyond a tolerance versus a stored baseline. Optionally generates missing outputs via any OpenAI-compatible endpoint.
- Structured-output self-correction (hot path, non-streaming): when the schema guardrail rejects a JSON response, the gateway first attempts a free local repair (strip code fences / surrounding prose), then optionally retries the model with a correction prompt up to N times before falling back to `422`. Outcomes are traced as `json_repaired` and/or `self_corrected:N`.
- Route load balancing: rank-weighted (smooth WRR) primary selection among quality-qualified models in a routing alias (`NEXUS_ROUTE_LOAD_BALANCE=true`); better models get proportionally more primary traffic, failover order is preserved.
- Semantic cache: Redis-backed embedding-similarity cache returns stored completions for near-duplicate prompts without an upstream call (`NEXUS_SEMANTIC_CACHE_ENABLED`); tenant-isolated per org/virtual key, alias-aware keying (survives load-balancer rotation), deterministic requests only (temperature unset or 0), hot-path embedding bounded by a timeout with graceful degrade; hits are traced as `cache_hit`.

---

## License & Model

**Open-core**: The gateway, observability, evals, and routing core are open source. Commercial features planned: SSO/RBAC, multi-tenancy, managed cloud service.

---

## Quick Links

- [README](README.md) — Quick start, usage, configuration
- [`.env.example`](.env.example) — Local development environment template
- [CI/CD workflows](.github/workflows/) — GitHub Actions definitions
