# First-byte failover

Fallback is possible **only before the first byte is written** to the
downstream client. After `HTTP/1.1 200` + `Content-Type:
text/event-stream` and any body bytes, the chain is committed.

## Unary (`/v1/chat/completions` without `stream`)

Walk the candidate chain. On provider error, if another candidate
remains:

- record the attempt with `error_type = upstream_error_failover`
- try the next model

If the last candidate fails: HTTP 502, OpenAI error shape
`type = upstream_error` (see `errors/v1_openai.json`).

## Stream

Same chain walk, but the “connect” that wins is the first
`ChatCompletionStream` that returns a channel without error. Bytes
from that channel are copied raw. A later upstream stall is **not**
a failover; it is a mid-stream error comment
(`errors/sse_comment.sse`).

TTFT (`ttft_ms` on `gateway_traces`) is measured from stream start
to first downstream byte of the **winning** candidate.

## What Elixir must match

- No double TTFT from Go-proxy-to-Elixir. Canary split is at the LB.
- Do not JSON-re-encode SSE frames while failing over or while
  copying the winner.
- Failover traces stay in ClickHouse as separate attempts on the
  evidence graph (`attempts`, `policy_reasons` on `gateway_traces`).
