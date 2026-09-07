# Evals, routing and guardrails

Nexus scores traffic off the hot path and feeds those scores back into
model selection. This is the part that is not a proxy.

## Evals

When enabled (`NEXUS_EVAL_ENABLED=true`, default), completed traces are evaluated
**out-of-band** by a background worker — never on the request hot path. Heuristics
and judges run without ClickHouse; **score persistence** uses ClickHouse when
`NEXUS_CLICKHOUSE_URL` is set, otherwise **Postgres** when `NEXUS_POSTGRES_URL`
is set. **Quality-aware routing** still requires ClickHouse for rolling stats.

In **v0.7** Nexus ships with three first-class evaluator kinds:

- **`heuristic_pii` (always on, cheap)** — flags emails/SSN/phone/card patterns
  in output. In-process regex; zero resource footprint.
- **`heuristic_completeness` (always on, cheap)** — empty or truncated answers.
  In-process regex.
- **`external` plugin (admin opt-in)** — a YAML manifest that forwards traces
  (and collects results) to an evaluation service you choose: LangSmith,
  Langfuse, Datadog LLM Observability, Braintrust, Arize Phoenix/AX, an OTel
  collector, or any HTTPS endpoint via the generic `webhook` adapter. See
  [`docs/eval-tab.md`](eval-tab.md) for the schema reference.

Alongside those, **model benchmarks** answer the other question — not "how
good was this trace" but "how good is this model at this task". An operator
picks a model and a dataset in `/eval/benchmarks`, and PrimeIntellect runs
the dataset and its scoring code on their own infrastructure; Nexus launches
the run, polls it, and stores the aggregate. Pointing the provider's inference
back through this gateway makes the score describe what Nexus actually serves,
routing and cache included. See
[`docs/model-benchmarks.md`](model-benchmarks.md).

Legacy `slm_judge` (Ollama-in-cluster) and `remote_eval` (Python sidecar with
DeepEval/RAGAS) evaluators are still supported for back-compat but are no
longer seeded automatically — opt in via the existing eval-profile endpoints
if you want to keep them running. A deprecation banner in `/eval` calls out
the migration path to plugins.

The Go gateway stays self-contained: nothing in the hot path depends on an
external service. The dispatcher reads the plugin registry, samples each
plugin's `spec.send.sampling`, and forwards the rendered payload off-cluster
on the worker goroutine.

### Results schema

All incoming eval scores are normalised to the OTel GenAI semantic
convention `gen_ai.evaluation.result`
(`name`, `score.value`, `score.label`, `explanation`, `response.id`). Per-vendor
flat-key mapping (`spec.collect.mapping`) handles the wire-shape
differences — each value is the *source key* on the vendor's wire format
(do NOT prefix with `$.`); JSONPath syntax was retired. Results
are written to `eval_scores` with `evaluator = "plugin:<name>"` so a
single SQL `GROUP BY evaluator` separates plugin scores from heuristic /
legacy ones.

### Legacy local backend

The Go gateway ships **two legacy evaluator profiles** for back-compat with
v0.5 / v0.6:

- `slm_judge` — local Ollama/vLLM LLM-as-judge; back-compat for the v0.6-era
  `NEXUS_JUDGE_*` env vars. Cells the worker through the same internal
  plugin interface (the plugin name resolves to `legacy-judge`).
- `remote_eval` — the Python sidecar ([`eval-service/`](../eval-service/)) with
  DeepEval/RAGAS; back-compat for `NEXUS_EVAL_SERVICE_*`. Cells through
  `legacy-sidecar`.

### External Python eval service (legacy)

> **Status (v0.7+)**: Pre-existing `NEXUS_EVAL_SERVICE_*` env vars still wire up
> the Python sidecar for tenants that depend on it, but new installations should
> use the Eval plugin system above instead. See
> [`docs/eval-tab.md`](eval-tab.md).

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

## Quality-aware routing

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

### Failover alert sinks (V4)

The metrics side of failover lives at `nexus_router_failover_total` (per
prometheus /metrics), but operators want pages, not metrics. Opt in to
external alerting by setting `NEXUS_FAILOVER_WEBHOOK` and/or
`NEXUS_FAILOVER_SLACK_WEBHOOK`. Both are independently optional; an empty
URL means *no goroutine, no DNS, no goroutine-assigned port* (the zero-dep
fast path stays clean).

The gateway emits one event per primary → secondary hop with this shape
(see `internal/router/notifier.go`):

```json
{
  "org_id": "default",
  "virtual_key_id": "vk-1",
  "alias": "smart",
  "tried": ["openai/gpt-4o", "anthropic/claude-3-5-sonnet-latest"],
  "primary": "openai/gpt-4o",
  "fallback": "anthropic/claude-3-5-sonnet-latest",
  "reason": "upstream_error_failover",
  "latency_ms": 412,
  "failed_at_unix_ms": 1752031234000
}
```

- **Generic webhook** (`NEXUS_FAILOVER_WEBHOOK`): `POST` of the envelope
  above, `Content-Type: application/json`. Forward into PagerDuty, OpsGenie,
  in-house alerting, or anywhere else that can take a JSON POST.
- **Slack** (`NEXUS_FAILOVER_SLACK_WEBHOOK`): a one-liner `{"text": ":warning:
  nexus failover · primary → fallback · reason=…"}` so it shows up in a Slack
  channel with all the usual formatting. Compatible with Slack, Mattermost,
  and Discord-via-webhook-proxy.

Both POSTs run on a buffered async worker so the gateway's hot path never
waits on a slow alert sink. A flapping primary is also coalesced by
optional `NEXUS_FAILOVER_ALERT_COOLDOWN` (e.g. `30s`) so the alert inbox
isn't melted — the metric counter still increments on every hop.

### High-concurrency tuning (V5)

Single-replica throughput is bounded by four knobs. Each is *opt-out*;
defaults keep the zero-dependency path lean.

- **Provider HTTP client pool.** Every provider adapter reuses a tuned
  `*http.Transport` (`MaxIdleConnsPerHost = max(32, 2*GOMAXPROCS)`, capped
  at 100, `IdleConnTimeout = 90s`). The stdlib default of 2 connects per
  host is the classic "first TCP+TLS handshake on every retry under
  load" failure. See `internal/gateway/providers/pool.go`.
- **Pooled SSE buffers.** `parseOpenAISSE` and the Anthropic/Gemini SSE
  parsers recycle their 64 KiB scanner buffer from a `sync.Pool`
  (`internal/gateway/providers/bufferpool.go`). A 24-stream burst no
  longer allocates 1.5 MiB of scratch.
- **Per-vkey in-flight cap.** Set `NEXUS_MAX_CONCURRENT_PER_KEY=16` (or
  any positive integer) to bound one virtual key's footprint in the
  upstream provider's queue. Independent of RPM: a key with a 1000-RPM
  plan and 24-stream bursts would still cap at 16 in-flight. Excess
  returns `429 concurrency_exceeded` with `Retry-After: 1`.
- **GOMEMLIMIT / GOGC (Kubernetes).** Helm `config.runtime.gomemlimit`
  (e.g. `768MiB`) ships a soft memory target so Go's GC biases earlier
  under pressure instead of letting RSS balloon until OOMKill. Set
  `config.runtime.gogc` if you want to trade CPU for GC aggressiveness.

Smoke under load:

```bash
go test -race -count=1 ./internal/gateway/providers/ -run 'Streaming|Pool'
go test -race -count=1 ./internal/limiter -run 'Concurrency'
```

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

While `NEXUS_PUBLIC_GATEWAY_URL` (above) wires the public-facing entry
points and the **raw SSE passthrough** keeps non-OpenAI-standard fields
alive on the wire, the actual content-policy enforcement lives in the
inline guardrails below. These run synchronously on the request hot path:
cheaper than async eval, and able to block or redact before bytes hit
the upstream or the client.

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
