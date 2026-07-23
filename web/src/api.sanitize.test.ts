import { describe, expect, it, vi } from "vitest";
import { fetchStats, fetchEvalConfig } from "./api";

describe("API sanitizers", () => {
  it("fetchStats returns zero-stats on 401 (does not propagate the error body)", async () => {
    vi.stubGlobal(
      "fetch",
      async () =>
        new Response(JSON.stringify({ error: "login required" }), { status: 401 }),
    );
    const s = await fetchStats();
    expect(s).toEqual({
      total_requests: 0,
      error_rate: 0,
      avg_latency_ms: 0,
      p95_latency_ms: 0,
      total_tokens: 0,
      total_cost_usd: 0,
      cache_hits: 0,
      cache_hit_rate: 0,
      guardrail_events: 0,
    });
    vi.unstubAllGlobals();
  });

  it("fetchStats clamps NaN/undefined numeric fields", async () => {
    vi.stubGlobal(
      "fetch",
      async () =>
        new Response(
          JSON.stringify({
            total_requests: Number.NaN,
            error_rate: undefined,
            avg_latency_ms: 12,
          }),
          { status: 200 },
        ),
    );
    const s = await fetchStats();
    expect(s.total_requests).toBe(0);
    expect(s.error_rate).toBe(0);
    expect(s.avg_latency_ms).toBe(12);
    vi.unstubAllGlobals();
  });

  it("fetchEvalConfig returns a zero-but-shaped snapshot on 401 (no throw)", async () => {
    vi.stubGlobal(
      "fetch",
      async () =>
        new Response(JSON.stringify({ error: "login required" }), { status: 401 }),
    );
    const c = await fetchEvalConfig();
    expect(c.eval_enabled).toBe(false);
    expect(c.routing.weights).toEqual({ quality: 0, cost: 0, latency: 0 });
    expect(c.eval.judge.api_key_set).toBe(false);
    vi.unstubAllGlobals();
  });

  it("fetchEvalConfig sanitizes incomplete bodies", async () => {
    vi.stubGlobal(
      "fetch",
      async () =>
        new Response(JSON.stringify({ eval_enabled: true }), { status: 200 }),
    );
    const c = await fetchEvalConfig();
    // Even though the server only sent `eval_enabled: true`, the snapshot is
    // complete and downstream optional chaining stays safe.
    expect(c.eval_enabled).toBe(true);
    expect(c.eval.judge.enabled).toBe(false);
    expect(Array.isArray(c.eval.remote.metrics)).toBe(true);
    expect(typeof c.routing.weights.quality).toBe("number");
    vi.unstubAllGlobals();
  });
});
