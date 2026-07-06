import { useEffect, useState } from "react";
import {
  connectLive,
  fetchEvals,
  fetchMe,
  fetchRouting,
  fetchStats,
  fetchTraces,
  type EvalMetric,
  type RoutingModel,
  type Stats,
  type TraceSummary,
  type User,
} from "./api";
import { Account } from "./Account";
import { Audit } from "./Audit";
import { Playground } from "./Playground";

const EMPTY_STATS: Stats = {
  total_requests: 0,
  error_rate: 0,
  avg_latency_ms: 0,
  p95_latency_ms: 0,
  total_tokens: 0,
  total_cost_usd: 0,
  cache_hits: 0,
  cache_hit_rate: 0,
  guardrail_events: 0,
};

type Tab = "overview" | "playground" | "audit" | "account";

export function App() {
  const [stats, setStats] = useState<Stats>(EMPTY_STATS);
  const [traces, setTraces] = useState<TraceSummary[]>([]);
  const [routing, setRouting] = useState<RoutingModel[]>([]);
  const [evals, setEvals] = useState<EvalMetric[]>([]);
  const [live, setLive] = useState(false);
  const [tab, setTab] = useState<Tab>("overview");
  const [user, setUser] = useState<User | null>(null);

  useEffect(() => {
    fetchMe().then((u) => {
      setUser(u);
      // Anonymous users see the Sign-in panel first. Logged-in users start
      // on Overview, but the PlayGround is always one click away.
      if (!u) setTab("account");
    }).catch(() => {});
  }, []);

  useEffect(() => {
    if (!user) {
      // Defer /api/* polling until we know there is a session. Avoids
      // spamming the gateway with 401s on the first paint.
      return;
    }
    const load = () => {
      fetchStats().then(setStats).catch(() => {});
      fetchTraces().then(setTraces).catch(() => {});
      fetchRouting().then(setRouting).catch(() => {});
      fetchEvals().then(setEvals).catch(() => {});
    };
    load();
    const interval = setInterval(load, 5000);

    const ws = connectLive((t) => {
      setTraces((prev) => [t, ...prev].slice(0, 200));
    });
    ws.onopen = () => setLive(true);
    ws.onclose = () => setLive(false);

    return () => {
      clearInterval(interval);
      ws.close();
    };
  }, [user]);

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">
          <span className="logo">◆</span> Nexus <span className="sub">LLM Gateway</span>
        </div>
        <nav className="tabs">
          {user && tab !== "account" && tab !== "audit" && (
            <>
              <button className={tab === "overview" ? "active" : ""} onClick={() => setTab("overview")}>
                Overview
              </button>
              <button className={tab === "playground" ? "active" : ""} onClick={() => setTab("playground")}>
                Playground
              </button>
            </>
          )}
          {user?.role === "admin" && (
            <button className={tab === "audit" ? "active" : ""} onClick={() => setTab("audit")}>
              Audit
            </button>
          )}
          <button className={tab === "account" ? "active" : ""} onClick={() => setTab("account")}>
            {user ? "Account" : "Sign in"}
          </button>
        </nav>
        <div className={`live ${live ? "on" : "off"}`}>
          <span className="dot" /> {live ? "LIVE" : "OFFLINE"}
        </div>
      </header>

      {tab === "account" ? (
        <Account user={user} onUser={setUser} />
      ) : tab === "playground" ? (
        <Playground />
      ) : tab === "audit" ? (
        <Audit />
      ) : (
        <>
      <section className="cards">
        <Card label="Requests (1h)" value={stats.total_requests.toLocaleString()} />
        <Card label="Error rate" value={`${(stats.error_rate * 100).toFixed(1)}%`} />
        <Card label="Avg latency" value={`${Math.round(stats.avg_latency_ms)} ms`} />
        <Card label="P95 latency" value={`${Math.round(stats.p95_latency_ms)} ms`} />
        <Card label="Cache hit rate" value={`${(stats.cache_hit_rate * 100).toFixed(1)}%`} />
        <Card label="Guardrail events" value={stats.guardrail_events.toLocaleString()} />
        <Card label="Tokens" value={stats.total_tokens.toLocaleString()} />
        <Card label="Cost" value={`$${stats.total_cost_usd.toFixed(4)}`} />
      </section>

      <div className="grid-2">
        <section className="panel">
          <h2>Model routing</h2>
          <table>
            <thead>
              <tr>
                <th>Model</th>
                <th>Eff. quality</th>
                <th>Quality</th>
                <th>Pass</th>
                <th>Safety</th>
                <th>Avg latency</th>
                <th>Avg cost</th>
                <th>Samples</th>
              </tr>
            </thead>
            <tbody>
              {routing.length === 0 && (
                <tr>
                  <td colSpan={8} className="empty">
                    No routing stats yet.
                  </td>
                </tr>
              )}
              {routing.map((m) => (
                <tr key={m.model}>
                  <td>{m.model}</td>
                  <td>
                    <Bar value={m.eff_quality} />
                  </td>
                  <td>{pct(m.quality)}</td>
                  <td>{pct(m.pass_rate)}</td>
                  <td>{pct(m.safety_pass_rate)}</td>
                  <td>{Math.round(m.avg_latency_ms)} ms</td>
                  <td>${m.avg_cost_usd.toFixed(5)}</td>
                  <td>{m.samples.toLocaleString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>

        <section className="panel">
          <h2>Eval scores (24h)</h2>
          <table>
            <thead>
              <tr>
                <th>Evaluator</th>
                <th>Metric</th>
                <th>Avg score</th>
                <th>Pass rate</th>
                <th>Samples</th>
              </tr>
            </thead>
            <tbody>
              {evals.length === 0 && (
                <tr>
                  <td colSpan={5} className="empty">
                    No eval scores yet.
                  </td>
                </tr>
              )}
              {evals.map((e) => (
                <tr key={`${e.evaluator}:${e.metric}`}>
                  <td><span className="tag">{e.evaluator}</span></td>
                  <td>{e.metric}</td>
                  <td>
                    <Bar value={e.avg_score} />
                  </td>
                  <td>{pct(e.pass_rate)}</td>
                  <td>{e.samples.toLocaleString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      </div>

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
              <th>Flags</th>
              <th>Status</th>
            </tr>
          </thead>
          <tbody>
            {traces.length === 0 && (
              <tr>
                <td colSpan={9} className="empty">
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
                  {t.cache_hit ? <span className="badge cache">cache</span> : null}
                  {t.guardrail_action ? (
                    <span className="badge guard" title={t.guardrail_action}>
                      {guardLabel(t.guardrail_action)}
                    </span>
                  ) : null}
                  {t.credential_source && t.credential_source !== "env" ? (
                    <span className={`badge key ${t.credential_source}`} title={`key: ${t.credential_source}`}>
                      {t.credential_source === "user" ? "byok" : t.credential_source}
                    </span>
                  ) : null}
                  {!t.cache_hit && !t.guardrail_action && (!t.credential_source || t.credential_source === "env") ? (
                    <span className="muted">-</span>
                  ) : null}
                </td>
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
        </>
      )}
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

// Bar renders a 0..1 value as a labeled progress bar.
function Bar({ value }: { value: number }) {
  const v = Math.max(0, Math.min(1, value || 0));
  return (
    <div className="bar">
      <div className="bar-fill" style={{ width: `${v * 100}%` }} />
      <span className="bar-label">{(v * 100).toFixed(0)}%</span>
    </div>
  );
}

function pct(v: number): string {
  return `${((v || 0) * 100).toFixed(0)}%`;
}

// guardLabel shortens a guardrail action ("input_blocked:deny" -> "blocked").
function guardLabel(action: string): string {
  if (action.startsWith("input_blocked")) return "blocked";
  if (action.startsWith("output_redacted")) return "redacted";
  if (action.startsWith("output_schema")) return "schema";
  return action.split(":")[0];
}
