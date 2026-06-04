// API client + types for the Nexus console.

export interface Stats {
  total_requests: number;
  error_rate: number;
  avg_latency_ms: number;
  p95_latency_ms: number;
  total_tokens: number;
  total_cost_usd: number;
}

export interface TraceSummary {
  trace_id: string;
  timestamp: string;
  provider_name: string;
  request_model: string;
  input_tokens: number;
  output_tokens: number;
  latency_ms: number;
  ttft_ms: number;
  cost_usd: number;
  status_code: number;
  streamed: number;
  finish_reason: string;
}

export async function fetchStats(window = "1h"): Promise<Stats> {
  const res = await fetch(`/api/stats?window=${window}`);
  return res.json();
}

export async function fetchTraces(limit = 100): Promise<TraceSummary[]> {
  const res = await fetch(`/api/traces?limit=${limit}`);
  const data = await res.json();
  return Array.isArray(data) ? data : [];
}

// connectLive opens the live trace WebSocket. The backend pushes a full Trace
// object per gateway request; we map it to the summary shape used by the table.
export function connectLive(onTrace: (t: TraceSummary) => void): WebSocket {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const ws = new WebSocket(`${proto}://${location.host}/api/live`);
  ws.onmessage = (ev) => {
    try {
      const raw = JSON.parse(ev.data);
      onTrace({
        trace_id: raw.trace_id,
        timestamp: raw.timestamp,
        provider_name: raw["gen_ai.provider.name"] ?? raw.provider_name ?? "",
        request_model: raw["gen_ai.request.model"] ?? raw.request_model ?? "",
        input_tokens: raw["gen_ai.usage.input_tokens"] ?? 0,
        output_tokens: raw["gen_ai.usage.output_tokens"] ?? 0,
        latency_ms: raw.latency_ms ?? 0,
        ttft_ms: raw.ttft_ms ?? 0,
        cost_usd: raw.cost_usd ?? 0,
        status_code: raw.status_code ?? 0,
        streamed: raw.streamed ? 1 : 0,
        finish_reason: raw["gen_ai.response.finish_reasons"] ?? "",
      });
    } catch {
      /* ignore malformed frames */
    }
  };
  return ws;
}
