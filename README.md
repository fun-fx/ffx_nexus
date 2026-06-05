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

# 4. (dev) run the dashboard
cd web && npm install && npm run dev   # http://localhost:5173
```

The gateway boots even with no API keys or ClickHouse configured (traces are
then live-only). Set keys/URL to enable providers and persistence.

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

## Configuration

| Env var | Default | Purpose |
| --- | --- | --- |
| `NEXUS_GATEWAY_ADDR` | `:8080` | Gateway proxy listen address |
| `NEXUS_CONSOLE_ADDR` | `:8081` | Console API / dashboard listen address |
| `NEXUS_CLICKHOUSE_URL` | _(empty)_ | Native DSN; empty disables persistence |
| `NEXUS_POSTGRES_URL` | _(empty)_ | Control plane DSN; empty disables key auth & credential store |
| `NEXUS_REDIS_URL` | _(empty)_ | Shared rate limits + budgets across replicas; empty = in-memory |
| `NEXUS_MASTER_KEY` | _(empty)_ | 32-byte (base64/hex) KEK for provider-secret encryption |
| `OPENAI_API_KEY` / `OPENAI_BASE_URL` | — | OpenAI provider |
| `ANTHROPIC_API_KEY` | — | Anthropic provider |
| `GEMINI_API_KEY` | — | Google Gemini provider |
| `NEXUS_JUDGE_BASE_URL` / `NEXUS_JUDGE_MODEL` | — / `qwen2.5:7b` | Local SLM judge (Phase 3) |
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
```

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
| `test_phase2.sh` | Virtual keys, 401/403, encrypted credentials, audit log, DB reload on restart, revoke/delete |
| `test_phase234.sh` | Rate limits (429), budgets (402), async evals, streaming, quality-aware routing |
| `test_eval_routing.sh` | `min_quality_score`, `eff_quality` stats, provider fallback |
| `test_zero_dep.sh` | Gateway without Postgres/ClickHouse/Redis (env keys only) |
| `test_guardrails.sh` | Inline guardrails: PII/deny-pattern/length input blocking |
| `test_schema_guardrails.sh` | Schema/JSON output guardrail: wiring + live JSON roundtrip |
| `test_eval_service.sh` | External Python eval service: contract, wiring, failure isolation |
| `test_eval_batch.sh` | Offline regression eval batch: aggregation + baseline regression gate |
| `test_eval_persistence.sh` | Live completion → remote eval → ClickHouse (skips without provider key) |
| `test_rag_eval.sh` | RAG `nexus_eval` context → eval sidecar contract |

Run a single phase: `./scripts/test_phase2.sh`, `./scripts/test_phase234.sh`, etc.

Upstream completion tests need `GEMINI_API_KEY` or `OPENAI_API_KEY` in `.env`.
If the provider quota is exhausted, those cases are **skipped** (not failed) so
local runs stay green; re-run after quota resets for full coverage.

### Control plane API

- `GET/POST /api/keys`, `DELETE /api/keys/{id}` — virtual keys
- `GET/POST /api/credentials`, `DELETE /api/credentials/{id}` — provider secrets

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

When ClickHouse is configured, completed traces are evaluated **out-of-band** by
a background worker — never on the request hot path. Results land in the
`eval_scores` table and feed quality-aware routing.

- **Heuristics (always on, cheap):** `heuristic_pii` (flags emails/SSN/phone/card
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

## CI/CD

GitHub Actions workflows live in [`.github/workflows/`](.github/workflows/).

| Workflow | Trigger | What it does |
| --- | --- | --- |
| **CI** | push / PR to `main` | `gofmt`, `go vet`, `go test -race`, Go build, `web/` TypeScript + Vite build |
| **Integration** | push / PR to `main`, manual | Docker Compose (Postgres, ClickHouse, Redis) + `./scripts/test_all.sh` |
| **Release** | tag `v*` (e.g. `v0.1.0`) | Build & push image to `ghcr.io/fun-fx/ffx_nexus` |

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
