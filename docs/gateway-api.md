# Gateway API

The gateway speaks the OpenAI wire format on `:8080`. Point any
OpenAI-compatible client at it and authenticate with a Nexus virtual key.

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

### Supported endpoints

| Endpoint | Notes |
| --- | --- |
| `POST /v1/chat/completions` | OpenAI-compatible chat (streaming + non-streaming, tools with `tool_choice` + `parallel_tool_calls`, structured output). Also accepts the **Cursor Agent "hybrid" body** (Responses-shaped `input`, flat functions, custom tools, `reasoning.effort`) and rewrites it to canonical chat shape — see [Cursor Agent compatibility](#cursor-agent-compatibility). |
| `POST /v1/responses` | OpenAI Responses API (string or array `input`, tool calls surfaced as `function_call` items). Streaming emits a full **OpenAI-spec `response.completed`** envelope (with `instructions`, `tools`, `parallel_tool_calls`, `usage`), a `response.failed` event on stream errors, and an `incomplete` close when the upstream truncates; `ApplyPatch` tool grammar is preserved end-to-end. Implemented as a shim over `/v1/chat/completions`. |
| `POST /v1/messages` | Anthropic Messages API (top-level `system`, content blocks, `max_tokens`, `tool_use` / `tool_result` exchanges). Streaming emits `message_start` → `content_block_*` → `message_delta` → `message_stop` per the Anthropic spec. Stock Anthropic Python and TypeScript SDKs point at the gateway by configuring their base URL to `https://<nexus>/v1` — no inline adapter required. Implemented as a shim over `/v1/chat/completions` so every backend the gateway already supports (OpenAI / Anthropic / Gemini / Groq / Mistral / Grid / user BYOK) is reachable behind the same wire shape. |
| `POST /v1/embeddings` | OpenAI-compatible embeddings for providers that implement the `EmbeddingsProvider` interface (OpenAI / Mistral today; Anthropic / Gemini / Groq to follow). Supports string and string-array `input`. |
| `POST /v1/moderations` | OpenAI-compatible content moderation. Omitted `model` defaults to `omni-moderation-latest`. Same `Auth`+`Enforce`+`BYOK` chain as chat. |
| `POST /v1/images/generations` | OpenAI-compatible image generation (`dall-e-3` and friends). Omitted `model` defaults to `dall-e-3`. |
| `GET  /v1/models` | Union of registered chat / embedding / moderation / image model ids across all installed providers |

All seven endpoints go through the same `Auth` + `Enforce` middleware chain, so
virtual-key RPM/budget limits and BYOK credential resolution apply uniformly.
The chat and `/v1/messages` paths share the same canonical pipeline (quality-
aware routing, fail-over, eval trace, semantic cache, guardrails) without
duplication.

> **Auth note for `/v1/messages`**: stock Anthropic SDKs default to an
> `x-api-key` header. Nexus auth reads `Authorization: Bearer <vkey>` (and
> `X-Nexus-Key`). Configure the SDK to set `auth_token = "Bearer <vkey>"`
> (`@anthropic-ai/sdk` `authToken` option) for the call to reach the
> virtual-key middleware.

### Cursor Agent compatibility

[Cursor Agent](https://cursor.com) and Cursor Composer sometimes send
**Responses-shaped payloads to `/v1/chat/completions`** — top-level
`input`, flat function tools, custom-type tools (notably
`type:"custom"` with an `ApplyPatch` grammar), `reasoning.effort`,
`max_output_tokens`, and Responses-only extras like `store`,
`include`, `prompt_cache_key`, `metadata`. Rather than 400 the call, the
gateway detects the hybrid shape and rewrites it to a canonical
`ChatCompletionRequest` so the rest of the pipeline (auth, BYOK,
guardrails, quality-aware routing, evals, semantic cache, **all** of
it) keeps working exactly as it does for plain Chat traffic:

| Cursor field | How Nexus handles it |
| --- | --- |
| top-level `input` (string or array) | translated to Chat `messages[]` |
| Responses flat function tools (`{type:"function", name:…, parameters:…}`) | nested to Chat `{type:"function", function:{name:…, parameters:…}}` |
| `type:"custom"` tools (ApplyPatch et al.) | preserved; `format` / `grammar` keys are kept on `function.parameters.format` so ApplyPatch survives round-trip |
| `tool_choice` hybrid shape (`{type:"function", name:X}` vs nested) | flattened / unnested to the Chat wire shape |
| `reasoning.effort` | promoted to `reasoning_effort` |
| `max_output_tokens` | promoted to `max_tokens` |
| Responses-only extras (`store`, `include`, `prompt_cache_key`, `metadata`, …) | forwarded as wire `extra` on the Chat request, then stripped so promoted keys never double-publish |
| array `messages[].content` (text + file parts) | parsed as a content-part list per OpenAI's Chat spec |

Detection runs **before** full JSON decode on the hot path so the
gateway doesn't allocate a `CursorHybridReq` for normal Chat traffic.
A true Chat body never enters the rewrite path.

A separate `/v1/responses` endpoint is also exposed for clients that
already speak Responses natively (and so that Cursor Agent's "hybrid"
traffic — when it eventually targets `/v1/responses` directly — works
without a bridge layer). Streaming on `/v1/responses` emits a
fully-formed `response.completed` envelope per the OpenAI spec, so
non-Cursor SDKs that already implement the Responses surface get the
same semantics for free.

```bash
# Embeddings
curl http://localhost:8080/v1/embeddings \
  -H "Authorization: Bearer nxs_live_..." \
  -H "Content-Type: application/json" \
  -d '{"model":"text-embedding-3-small","input":["hello","world"]}'

# Responses API (multi-message + tool call)
curl http://localhost:8080/v1/responses \
  -H "Authorization: Bearer nxs_live_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "instructions": "Reply concisely.",
    "input": [
      {"role":"user","content":"What is the capital of France?"},
      {"role":"assistant","content":"Paris."},
      {"role":"user","content":"And of Italy?"}
    ]
  }'

# Responses API streaming — wire ends with response.completed carrying
# {id, object:"response", status, model, output[], usage,
#  parallel_tool_calls, instructions, tools}. Pass "stream": true.
curl -N http://localhost:8080/v1/responses \
  -H "Authorization: Bearer nxs_live_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "stream": true,
    "input": [{"role":"user","content":"Three short bullet points about SLIs."}]
  }'

# Moderation
curl http://localhost:8080/v1/moderations \
  -H "Authorization: Bearer nxs_live_..." \
  -H "Content-Type: application/json" \
  -d '{"model":"omni-moderation-latest","input":"I want to hurt myself."}'

# Image generation (dall-e-3 default)
curl http://localhost:8080/v1/images/generations \
  -H "Authorization: Bearer nxs_live_..." \
  -H "Content-Type: application/json" \
  -d '{"model":"dall-e-3","prompt":"a watercolor of a ship in a storm","size":"1024x1024"}'
```

## Raw SSE passthrough

OpenAI-compatible providers increasingly carry non-OpenAI-standard
fields on their SSE event payloads — `reasoning_content`,
`thinking_blocks`, vendor-specific metadata — that the strict
unmarshal-then-remarshal path would silently drop. When the provider
advertises a passthrough-eligible model set (OpenAI itself, the
OpenAI-compat wrapper behind `OPENAI_BASE_URL`, The Grid behind
`GRID_API_KEY`), the gateway streams the upstream SSE byte-for-byte
instead of round-tripping through `ChatCompletionChunk`. The handler
still parses one cheap copy per chunk locally for trace metrics, so the
dashboard / per-model cost / latency / failover trace record is
unchanged.

What this buys you:

| Field on the wire | Stricter path (≤ v0.5.0) | Raw passthrough (v0.5.1+) |
| --- | --- | --- |
| `delta.content` | forwarded | forwarded |
| `delta.reasoning_content` (OpenAI o-series, Cursor-style) | dropped | forwarded |
| `delta.tool_calls[*]` (including `index` / `id`) | forwarded | forwarded byte-for-byte |
| `delta.thinking_blocks` (vendor-specific) | dropped | forwarded |
| Custom SSE `:comment`, `id:`, `event:` lines | kept if recognised | preserved verbatim |
| Trace metrics (per-chunk cost, model, latency) | yes | yes |
| First-byte latency tax | unmarshal+remarshal | \(\approx 0\) |

A failure to parse a chunk (malformed JSON, mid-stream truncation) is
recorded on the trace and falls back to the strict path; the connection
is not dropped. For Responses streaming (`POST /v1/responses`), the
gateway still emits a final OpenAI-spec `response.completed` envelope
- raw passthrough is data-plane only; the public Responses shape is
  emitted by the gateway, not the upstream.

## Console API

- `GET /api/stats?window=1h` — aggregate metrics
- `GET /api/traces?limit=100` — recent traces
- `GET /api/routing` — per-model rolling quality/cost/latency used for routing
- `GET /api/live` — WebSocket live trace feed
- `POST /api/auth/login`, `POST /api/auth/logout`, `GET/PATCH /api/me` — session auth + self settings
- `GET /api/auth/sso/login`, `GET /api/auth/sso/callback` — OIDC SSO (only when SSO env vars are set)
- `GET/POST /api/me/keys`, `GET/POST /api/me/credentials` — BYOK self-service
- `GET/POST /api/users`, `DELETE /api/users/{id}` — admin user management
- `GET /api/users/quality` — per-user rolling quality + spend (admin)
