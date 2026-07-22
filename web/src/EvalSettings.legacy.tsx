import { useEffect, useState } from "react";
import {
  fetchEvalConfig,
  patchEvalConfig,
  type EvalConfigSnapshot,
} from "./api";

export function EvalSettings() {
  const [cfg, setCfg] = useState<EvalConfigSnapshot | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const [piiEnabled, setPiiEnabled] = useState(true);
  const [completenessEnabled, setCompletenessEnabled] = useState(true);
  const [sampleRate, setSampleRate] = useState("1");
  const [judgeBaseURL, setJudgeBaseURL] = useState("");
  const [judgeModel, setJudgeModel] = useState("");
  const [judgeAPIKey, setJudgeAPIKey] = useState("");
  const [evalServiceURL, setEvalServiceURL] = useState("");
  const [evalServiceMetrics, setEvalServiceMetrics] = useState("");
  const [routeWQuality, setRouteWQuality] = useState("0.6");
  const [routeWCost, setRouteWCost] = useState("0.2");
  const [routeWLatency, setRouteWLatency] = useState("0.2");
  const [routeWindow, setRouteWindow] = useState("1h");
  const [routeGroups, setRouteGroups] = useState("");

  const applyForm = (snap: EvalConfigSnapshot) => {
    setCfg(snap);
    setPiiEnabled(snap.eval.pii_enabled);
    setCompletenessEnabled(snap.eval.completeness_enabled);
    setSampleRate(String(snap.eval.sample_rate));
    setJudgeBaseURL(snap.eval.judge.base_url);
    setJudgeModel(snap.eval.judge.model);
    setEvalServiceURL(snap.eval.remote.url);
    setEvalServiceMetrics(snap.eval.remote.metrics.join(","));
    setRouteWQuality(String(snap.routing.weights.quality ?? 0.6));
    setRouteWCost(String(snap.routing.weights.cost ?? 0.2));
    setRouteWLatency(String(snap.routing.weights.latency ?? 0.2));
    setRouteWindow(snap.routing.window || "1h");
    setRouteGroups(snap.routing.groups_spec || "");
  };

  useEffect(() => {
    setLoading(true);
    setError("");
    fetchEvalConfig()
      .then((snap) => applyForm(snap))
      .catch((e) => setError(String(e?.message || e)))
      .finally(() => setLoading(false));
  }, []);

  const onSave = async () => {
    setSaving(true);
    setError("");
    setNotice("");
    try {
      const snap = await patchEvalConfig({
        pii_enabled: piiEnabled,
        completeness_enabled: completenessEnabled,
        sample_rate: Number(sampleRate),
        judge_base_url: judgeBaseURL,
        judge_model: judgeModel,
        judge_api_key: judgeAPIKey || undefined,
        eval_service_url: evalServiceURL,
        eval_service_metrics: evalServiceMetrics,
        route_w_quality: Number(routeWQuality),
        route_w_cost: Number(routeWCost),
        route_w_latency: Number(routeWLatency),
        route_window: routeWindow,
        route_groups: routeGroups,
      });
      applyForm(snap);
      setJudgeAPIKey("");
      setNotice("Settings applied at runtime. Worker count and refresh interval require a restart.");
    } catch (e) {
      setError(String((e as Error)?.message || e));
    } finally {
      setSaving(false);
    }
  };

  if (loading) {
    return (
      <section className="panel">
        <h2>Quality &amp; Routing</h2>
        <p className="muted">Loading...</p>
      </section>
    );
  }

  if (!cfg) {
    return (
      <section className="panel">
        <h2>Quality &amp; Routing</h2>
        {error && <div className="error">{error}</div>}
        <p className="empty">
          Eval worker is disabled. Set <code>NEXUS_EVAL_ENABLED=true</code> (default) and restart.
        </p>
      </section>
    );
  }

  return (
    <>
      <div className="grid-2">
        <section className="panel">
          <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 4 }}>
            <h2 style={{ margin: 0 }}>Heuristics</h2>
            <span className="badge cache">Free &middot; Instant</span>
          </div>
          <p className="muted" style={{ marginTop: 6, marginBottom: 14 }}>
            Runs automatically on every successful gateway response. Scores persist to ClickHouse or Postgres when configured.
          </p>

          {error && <div className="error" style={{ marginBottom: 10 }}>{error}</div>}
          {notice && <div className="notice" style={{ marginBottom: 10 }}>{notice}</div>}

          <div className="toggle-row">
            <input
              id="pii"
              type="checkbox"
              checked={piiEnabled}
              onChange={(e) => setPiiEnabled(e.target.checked)}
            />
            <label htmlFor="pii">
              <strong>PII leak check</strong>
              <small>Checks if the response contains sensitive data (SSN, card numbers, emails).</small>
            </label>
          </div>

          <div className="toggle-row">
            <input
              id="comp"
              type="checkbox"
              checked={completenessEnabled}
              onChange={(e) => setCompletenessEnabled(e.target.checked)}
            />
            <label htmlFor="comp">
              <strong>Completeness check</strong>
              <small>Flags truncated or empty responses (finish_reason != stop).</small>
            </label>
          </div>
        </section>

        <section className="panel">
          <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 4 }}>
            <h2 style={{ margin: 0 }}>SLM Judge</h2>
            <span className={`badge ${cfg.eval.judge.enabled ? "key" : "guard"}`}>
              {cfg.eval.judge.enabled ? "Active" : "Inactive"}
            </span>
          </div>
          <p className="muted" style={{ marginTop: 6, marginBottom: 14 }}>
            Scores response quality with a small LLM. Adjust sample rate to control cost.
          </p>

          <div className="field-row">
            <label>Sample rate (0-1)</label>
            <input
              type="number"
              min={0}
              max={1}
              step={0.1}
              value={sampleRate}
              onChange={(e) => setSampleRate(e.target.value)}
              style={{ width: 80 }}
            />
          </div>

          <div className="field-row">
            <label>Judge URL</label>
            <input
              value={judgeBaseURL}
              onChange={(e) => setJudgeBaseURL(e.target.value)}
              placeholder="http://localhost:11434/v1"
              style={{ flex: 1 }}
            />
          </div>

          <div className="field-row">
            <label>Model</label>
            <input
              value={judgeModel}
              onChange={(e) => setJudgeModel(e.target.value)}
              placeholder="qwen2.5:7b"
              style={{ flex: 1 }}
            />
          </div>

          <div className="field-row">
            <label>API Key</label>
            <input
              type="password"
              value={judgeAPIKey}
              onChange={(e) => setJudgeAPIKey(e.target.value)}
              placeholder={cfg.eval.judge.api_key_set ? "******** (already set)" : "optional"}
              style={{ flex: 1 }}
            />
          </div>
        </section>

        <section className="panel">
          <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 4 }}>
            <h2 style={{ margin: 0 }}>Remote eval service</h2>
            <span className={`badge ${cfg.eval.remote.enabled ? "key" : "guard"}`}>
              {cfg.eval.remote.enabled ? "Active" : "Inactive"}
            </span>
          </div>
          <p className="muted" style={{ marginTop: 6, marginBottom: 14 }}>
            Send traces to an external eval server (Ragas, TruLens, etc.).
          </p>

          <div className="field-row">
            <label>Service URL</label>
            <input
              value={evalServiceURL}
              onChange={(e) => setEvalServiceURL(e.target.value)}
              placeholder="http://localhost:8200"
              style={{ flex: 1 }}
            />
          </div>

          <div className="field-row">
            <label>Metrics</label>
            <input
              value={evalServiceMetrics}
              onChange={(e) => setEvalServiceMetrics(e.target.value)}
              placeholder="answer_relevancy, toxicity, bias"
              style={{ flex: 1 }}
            />
          </div>
        </section>

        <section className="panel">
          <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 4 }}>
            <h2 style={{ margin: 0 }}>Quality-aware routing</h2>
            <span className="badge cache">auto</span>
          </div>
          <p className="muted" style={{ marginTop: 6, marginBottom: 14 }}>
            Chooses the best model for <code>model: auto</code> requests based on weights.
            {!cfg.routing_enabled && " Requires ClickHouse or Postgres eval scores for rolling stats."}
          </p>

          <div className="field-row">
            <label>Quality weight</label>
            <input
              type="number"
              min={0}
              max={1}
              step={0.1}
              value={routeWQuality}
              onChange={(e) => setRouteWQuality(e.target.value)}
              style={{ width: 80 }}
            />
          </div>
          <div className="field-row">
            <label>Cost weight</label>
            <input
              type="number"
              min={0}
              max={1}
              step={0.1}
              value={routeWCost}
              onChange={(e) => setRouteWCost(e.target.value)}
              style={{ width: 80 }}
            />
          </div>
          <div className="field-row">
            <label>Latency weight</label>
            <input
              type="number"
              min={0}
              max={1}
              step={0.1}
              value={routeWLatency}
              onChange={(e) => setRouteWLatency(e.target.value)}
              style={{ width: 80 }}
            />
          </div>

          <div style={{ marginTop: 12 }} />

          <div className="field-row">
            <label>Stats window</label>
            <input
              value={routeWindow}
              onChange={(e) => setRouteWindow(e.target.value)}
              placeholder="1h"
              style={{ width: 120 }}
            />
          </div>
          <div className="field-row">
            <label>Route groups</label>
            <input
              value={routeGroups}
              onChange={(e) => setRouteGroups(e.target.value)}
              placeholder="fast=gpt-4o-mini,gemini-2.5-flash;smart=gpt-4o"
              style={{ flex: 1 }}
            />
          </div>
        </section>
      </div>

      <div style={{ display: "flex", gap: 12, alignItems: "center", marginTop: 8 }}>
        <button className="btn primary" onClick={onSave} disabled={saving}>
          {saving ? "Saving..." : "Save settings"}
        </button>
        {saving && <span className="muted">Applied instantly at runtime</span>}
      </div>

      <section className="panel" style={{ marginTop: 20 }}>
        <h2 style={{ marginTop: 0 }}>Status</h2>
        <div className="status-grid">
          <StatusCard label="Eval worker" value={cfg.eval_enabled ? "Running" : "Off"} ok={cfg.eval_enabled} />
          <StatusCard
            label="Score persistence"
            value={scoreStoreLabel(cfg.score_store, cfg.score_persisted)}
            ok={cfg.score_persisted}
          />
          <StatusCard
            label="Trace store"
            value={cfg.trace_store === "clickhouse" ? "ClickHouse" : "Live only"}
            ok={cfg.trace_store === "clickhouse"}
          />
          <StatusCard
            label="Routing stats"
            value={routingStatsLabel(cfg.routing_stats_store, cfg.routing_enabled)}
            ok={cfg.routing_enabled}
          />
          <StatusCard label="Routing" value={cfg.routing_enabled ? "Enabled" : "Disabled"} ok={cfg.routing_enabled} />
          <StatusCard label="SLM judge" value={cfg.eval.judge.enabled ? "Active" : "Inactive"} ok={cfg.eval.judge.enabled} />
          <StatusCard label="Remote eval" value={cfg.eval.remote.enabled ? "Active" : "Inactive"} ok={cfg.eval.remote.enabled} />
          <StatusCard label="Workers" value={String(cfg.eval.workers)} />
          <StatusCard label="Refresh" value={cfg.routing.refresh} />
        </div>

        {cfg.restart_required.length > 0 && (
          <div style={{ marginTop: 16 }}>
            <h3 style={{ fontSize: 13, margin: "0 0 8px" }} className="muted">Requires restart</h3>
            <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }}>
              {cfg.restart_required.map((item) => (
                <span key={item} className="badge guard" style={{ fontSize: 12 }}>
                  {item}
                </span>
              ))}
            </div>
          </div>
        )}
      </section>
    </>
  );
}

function routingStatsLabel(store: string, enabled: boolean): string {
  if (!enabled) return "Disabled";
  if (store === "postgres") return "Postgres (eval-only)";
  if (store === "clickhouse") return "ClickHouse";
  return store || "Unknown";
}

function scoreStoreLabel(store: string, persisted: boolean): string {
  if (!persisted) return "Not persisted";
  if (store === "postgres") return "Postgres";
  if (store === "clickhouse") return "ClickHouse";
  return store;
}

function StatusCard({ label, value, ok }: { label: string; value: string; ok?: boolean }) {
  return (
    <div className="status-card">
      <div className="status-card-label">{label}</div>
      <div className={`status-card-value ${ok === true ? "ok" : ok === false ? "err" : ""}`}>{value}</div>
    </div>
  );
}
