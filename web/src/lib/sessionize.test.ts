import { describe, expect, it } from "vitest";
import { sessionizeTraces } from "./sessionize";
import type { TraceSummary } from "../api";

// minimal TraceSummary factory — anything we do not list defaults to 0
// / "" / "ok". The sessionize helper only reads a handful of fields
// so this is enough surface area to drive the roll-up cases.
function row(opts: Partial<TraceSummary> & { ts: string; sid?: string }): TraceSummary {
  return {
    trace_id: opts.trace_id ?? crypto.randomUUID(),
    timestamp: opts.ts,
    provider_name: opts.provider_name ?? "grid",
    request_model: opts.request_model ?? "code-prime",
    response_model: opts.response_model,
    input_tokens: opts.input_tokens ?? 10,
    output_tokens: opts.output_tokens ?? 20,
    total_tokens: opts.total_tokens ?? (opts.input_tokens ?? 10) + (opts.output_tokens ?? 20),
    latency_ms: opts.latency_ms ?? 100,
    ttft_ms: 0,
    cost_usd: opts.cost_usd ?? 0.001,
    status_code: opts.status_code ?? 200,
    streamed: 0,
    finish_reason: "stop",
    cache_hit: 0,
    guardrail_action: "",
    credential_source: "env",
    user_id: opts.user_id,
    user_email: opts.user_email,
    session_id: opts.sid,
  };
}

describe("sessionizeTraces", () => {
  it("returns an empty list for an empty input", () => {
    expect(sessionizeTraces([])).toEqual([]);
  });

  it("merges traces sharing the same wire session_id into a single row", () => {
    const a = row({ ts: "2026-01-01T00:00:01Z", sid: "agent-1", cost_usd: 0.01 });
    const b = row({
      ts: "2026-01-01T00:00:02Z",
      sid: "agent-1",
      cost_usd: 0.02,
      trace_id: "trace-b",
    });
    const c = row({
      ts: "2026-01-01T00:00:03Z",
      sid: "agent-1",
      cost_usd: 0.03,
      trace_id: "trace-c",
    });

    const out = sessionizeTraces([a, b, c]);
    expect(out).toHaveLength(1);
    expect(out[0].session_key).toBe("agent-1");
    expect(out[0].from_wire).toBe(true);
    expect(out[0].trace_count).toBe(3);
    expect(+out[0].total_cost_usd.toFixed(4)).toBe(0.06);
    expect(out[0].trace_ids).toEqual([a.trace_id, b.trace_id, c.trace_id]);
    expect(out[0].last_at).toBe("2026-01-01T00:00:03Z");
    expect(out[0].first_at).toBe("2026-01-01T00:00:01Z");
  });

  it("keeps traces with different session_ids separate", () => {
    const a = row({ ts: "2026-01-01T00:00:01Z", sid: "A" });
    const b = row({ ts: "2026-01-01T00:00:02Z", sid: "B" });
    expect(sessionizeTraces([a, b])).toHaveLength(2);
  });

  it("merges synthetically by time window when session_id is empty", () => {
    // Two bursts of the same model+provider; the bursts are > 5 min
    // apart and within each burst the calls are < 5 min apart.
    const burst1 = [
      "2026-01-01T00:00:01Z",
      "2026-01-01T00:00:30Z",
      "2026-01-01T00:01:00Z",
    ];
    const burst2 = [
      "2026-01-01T00:10:00Z",
      "2026-01-01T00:10:30Z",
    ];

    const all = [...burst1, ...burst2].map((ts, i) =>
      row({
        ts,
        request_model: "code-prime",
        provider_name: "grid",
        trace_id: `t${i}`,
      }),
    );
    const out = sessionizeTraces([...all].reverse());

    expect(out).toHaveLength(2);
    const sizes = [out[0].trace_count, out[1].trace_count].sort();
    expect(sizes).toEqual([2, 3]);
    // Both rows are heuristic (no wire id).
    expect(out.map((r) => r.from_wire)).toEqual([false, false]);
  });

  it("does not merge across (model, provider) even within the window", () => {
    const a = row({
      ts: "2026-01-01T00:00:01Z",
      request_model: "code-prime",
      provider_name: "grid",
    });
    const b = row({
      ts: "2026-01-01T00:00:02Z",
      request_model: "code-prime",
      provider_name: "openai",
      trace_id: "trace-b",
    });
    expect(sessionizeTraces([a, b])).toHaveLength(2);
  });

  it("averages latency across the merged traces", () => {
    const a = row({ ts: "2026-01-01T00:00:01Z", sid: "S", latency_ms: 100 });
    const b = row({
      ts: "2026-01-01T00:00:02Z",
      sid: "S",
      latency_ms: 200,
      trace_id: "trace-b",
    });
    const c = row({
      ts: "2026-01-01T00:00:03Z",
      sid: "S",
      latency_ms: 300,
      trace_id: "trace-c",
    });
    const out = sessionizeTraces([a, b, c]);
    expect(out[0].avg_latency_ms).toBe(200);
  });

  it("records the first error if any merged trace fails", () => {
    const a = row({ ts: "2026-01-01T00:00:01Z", sid: "S" });
    const b = row({
      ts: "2026-01-01T00:00:02Z",
      sid: "S",
      status_code: 503,
      trace_id: "trace-b",
    });
    const c = row({
      ts: "2026-01-01T00:00:03Z",
      sid: "S",
      trace_id: "trace-c",
    });
    const out = sessionizeTraces([a, b, c]);
    expect(out[0].first_error).not.toBeNull();
    expect(out[0].first_error!.status).toBe(503);
  });

  it("honours the wire marker ahead of the heuristic fallback", () => {
    // a carries an explicit session_id; b does not. Even though b is
    // within 5 min of a on the same (model, provider), the heuristic
    // cannot fold a wire-marked row — the wire id is authoritative,
    // and a synthetic merge could mask an unrelated user that happens
    // to share the burst window.
    const a = row({
      ts: "2026-01-01T00:00:01Z",
      sid: "agent",
      request_model: "code-prime",
      provider_name: "grid",
    });
    const b = row({
      ts: "2026-01-01T00:00:02Z",
      sid: "",
      request_model: "code-prime",
      provider_name: "grid",
      trace_id: "trace-b",
    });
    const out = sessionizeTraces([a, b]);
    expect(out).toHaveLength(2);
    // Order doesn't matter for the assertion; one row is wire-anchored,
    // the other is heuristic.
    const wireRow = out.find((r) => r.from_wire);
    const heuRow = out.find((r) => !r.from_wire);
    expect(wireRow?.session_key).toBe("agent");
    expect(heuRow).toBeDefined();
  });

  it("sums input/output/total tokens across the merged traces", () => {
    // Three turns of a session — verify total_tokens / total_input_tokens
    // / total_output_tokens all roll up so the Tokens column on the
    // Recent-sessions panel reflects full conversation volume, not the
    // last trace only. We also feed each row a total_tokens that does
    // NOT equal input+output to make sure the helper prefers the
    // wire-disclosed total when present.
    const a = row({
      ts: "2026-01-01T00:00:01Z",
      sid: "agent",
      input_tokens: 100,
      output_tokens: 50,
      cost_usd: 0,
    });
    const b = row({
      ts: "2026-01-01T00:00:02Z",
      sid: "agent",
      input_tokens: 200,
      output_tokens: 80,
      cost_usd: 0,
      trace_id: "trace-b",
    });
    const c = row({
      ts: "2026-01-01T00:00:03Z",
      sid: "agent",
      input_tokens: 350,
      output_tokens: 175,
      cost_usd: 0,
      trace_id: "trace-c",
    });
    const out = sessionizeTraces([a, b, c]);
    expect(out[0].total_input_tokens).toBe(650);
    expect(out[0].total_output_tokens).toBe(305);
    expect(out[0].total_tokens).toBe(955);
  });

  it("tracks the latest response_model on the session row", () => {
    // claude-opus-latest requested, anthropic/claude-opus-5 served:
    // the grid-routed model should show in response_model so the
    // Recent-sessions row surfaces the multi-vendor fan-out that
    // provider_name="grid" alone would hide.
    const a = row({
      ts: "2026-01-01T00:00:01Z",
      sid: "agent",
      request_model: "claude-opus-latest",
      response_model: "anthropic/claude-opus-5",
    });
    const out = sessionizeTraces([a]);
    expect(out[0].request_model).toBe("claude-opus-latest");
    expect(out[0].response_model).toBe("anthropic/claude-opus-5");
  });
});
