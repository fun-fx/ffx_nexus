# ADR 0001: Preserve upstream-supplied model id in OpenAI-compatible responses

Status: **Accepted** (2026-07-01)
Author: Nexus team
Context: v1.1 gateway gap work, dogfood pass on `grid/text-prime` (PR #57).

## Context

Nexus exposes an OpenAI-compatible chat completions API. Callers send
`{"model":"<provider>/<instrument>"}` and get back a response whose top-level
`model` field is the provider's answer.

Three reasonable defaults are possible, and the open-source community has
**already judged** one of them as a regression:

| Default | Example after `grid/text-prime` is requested | Pros | Cons |
| --- | --- | --- | --- |
| (A) Pass upstream response through verbatim | `model: "minimax/minimax-m3"` (whatever The Grid routed to) | Standard OpenAI behaviour; real deployment id preserved; full traceability in traces | Caller who sent "grid/text-prime" does not see that name echoed |
| (B) Echo the caller-requested model id | `model: "grid/text-prime"` | Stable client log; alias preserved | Hides the actual supplier model; same regression that LiteLLM users reported in [issue #22709](https://github.com/BerriAI/litellm/issues/22709) |
| (C) Add a new `model_info` field with both | both | Maximum info | Breaks OpenAI contract for clients that don't know the field |

LiteLLM tried (B) by intent (strip internal LiteLLM prefixes from the
response), but the regression generalised to model-group aliases, breaking
clients. The fix [PR #21874](https://github.com/BerriAI/litellm/pull/21874)
rolled back to **preserve the upstream-resolved model name**, only stripping
known internal provider prefixes (`hosted_vllm/`, `bedrock/`, ...). This is
also the same default OpenAI itself uses: `model: "gpt-4.1"` →
`model: "gpt-4.1-2025-04-14"`.

Nexus today already implements (A) for every provider (the OpenAI,
OpenAICompat-derived adapters, Anthropic adapter, Gemini adapter). The
`grid/text-prime` dogfood call returned `minimax/minimax-m3` — exactly
what (A) prescribes. No code change is required.

## Decision

**Default to (A).** Nexus preserves the model id that the upstream provider
returns. Internal Nexus prefixes that may appear in upstream payloads
(`nexus_`, etc.) are stripped, mirroring LiteLLM's prefix-set. No
override knob in v1.1 — clients that need the originally-requested model
should keep it on their side.

## Consequences

- **Positives.** Trivial alignment with OpenAI native behaviour and with
  LiteLLM post-#21874. Zero regression risk. Supplier model id flows
  directly into ClickHouse traces, which means cost attribution, eval
  sampling, and routing decisions stay correct without any second lookup.
- **Negatives.** A caller sending `model: "grid/text-prime"` does not see
  that name in the response. If a UI wants to show "you used grid/text-prime,"
  it must use its own request-side log. This is identical to direct OpenAI
  usage (`gpt-4.1` request → `gpt-4.1-2025-04-14` response in OpenAI console).
- **Future toggle.** If a later user need surfaces (e.g. a developer wants
  their CLI to display the same model name they typed), revisit as a
  config flag in the OpenAICompat layer. Not in v1.1.

## Notes for implementers

- If a future provider adapter needs to surface `(B)` semantics for a
  specific integration (e.g. a hosted proprietary model whose id includes
  a tenant prefix the client shouldn't see), do so in that adapter — keep
  the global default `(A)`.
- Streaming chunks share the same `model` field on the first chunk.
  `internal/gateway/providers/openai.go::ChatCompletionStream` already lets
  the upstream value flow through. Verify by inspecting SSELines after a
  live call (see dogfood transcript 2026-07-01).
