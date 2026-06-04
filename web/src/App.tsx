import { useEffect, useState } from "react";
import {
  connectLive,
  fetchStats,
  fetchTraces,
  type Stats,
  type TraceSummary,
} from "./api";

const EMPTY_STATS: Stats = {
  total_requests: 0,
  error_rate: 0,
  avg_latency_ms: 0,
  p95_latency_ms: 0,
  total_tokens: 0,
  total_cost_usd: 0,
};

export function App() {
  const [stats, setStats] = useState<Stats>(EMPTY_STATS);
  const [traces, setTraces] = useState<TraceSummary[]>([]);
  const [live, setLive] = useState(false);

  useEffect(() => {
    const load = () => {
      fetchStats().then(setStats).catch(() => {});
      fetchTraces().then(setTraces).catch(() => {});
    };
    load();
    const interval = setInterval(() => fetchStats().then(setStats).catch(() => {}), 5000);

    const ws = connectLive((t) => {
      setTraces((prev) => [t, ...prev].slice(0, 200));
    });
    ws.onopen = () => setLive(true);
    ws.onclose = () => setLive(false);

    return () => {
      clearInterval(interval);
      ws.close();
    };
  }, []);

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">
          <span className="logo">◆</span> Nexus <span className="sub">LLM Gateway</span>
        </div>
        <div className={`live ${live ? "on" : "off"}`}>
          <span className="dot" /> {live ? "LIVE" : "OFFLINE"}
        </div>
      </header>

      <section className="cards">
        <Card label="Requests (1h)" value={stats.total_requests.toLocaleString()} />
        <Card label="Error rate" value={`${(stats.error_rate * 100).toFixed(1)}%`} />
        <Card label="Avg latency" value={`${Math.round(stats.avg_latency_ms)} ms`} />
        <Card label="P95 latency" value={`${Math.round(stats.p95_latency_ms)} ms`} />
        <Card label="Tokens" value={stats.total_tokens.toLocaleString()} />
        <Card label="Cost" value={`$${stats.total_cost_usd.toFixed(4)}`} />
      </section>

      <section className="panel">
        <h2>Recent traces</h2>
        <table>
          <thead>
            <tr>
              <th>Time</th>
              <th>Provider</th>
              <th>Model</th>
              <th>Tokens (in/out)</th>
              <th>TTFT</th>
              <th>Latency</th>
              <th>Cost</th>
              <th>Status</th>
            </tr>
          </thead>
          <tbody>
            {traces.length === 0 && (
              <tr>
                <td colSpan={8} className="empty">
                  No traces yet. Send a request to the gateway on :8080.
                </td>
              </tr>
            )}
            {traces.map((t) => (
              <tr key={t.trace_id}>
                <td>{new Date(t.timestamp).toLocaleTimeString()}</td>
                <td><span className="tag">{t.provider_name}</span></td>
                <td>{t.request_model}</td>
                <td>{t.input_tokens}/{t.output_tokens}</td>
                <td>{t.ttft_ms ? `${t.ttft_ms} ms` : "-"}</td>
                <td>{t.latency_ms} ms</td>
                <td>${t.cost_usd.toFixed(5)}</td>
                <td>
                  <span className={`status ${t.status_code >= 400 ? "err" : "ok"}`}>
                    {t.status_code}
                  </span>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>
    </div>
  );
}

function Card({ label, value }: { label: string; value: string }) {
  return (
    <div className="card">
      <div className="card-label">{label}</div>
      <div className="card-value">{value}</div>
    </div>
  );
}
