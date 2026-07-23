import { useQuery } from "@tanstack/react-query";
import { GradientText } from "../components/GradientText";
import { TierCard } from "../components/TierCard";
import { Icon } from "../components/icons";
import {
  fetchEvalConfig,
  fetchMe,
  fetchRouting,
  fetchStats,
  fetchTraces,
  type EvalConfigSnapshot,
  type RoutingModel,
  type Stats,
  type TraceSummary,
  type User,
} from "../api";

async function fetchOverview() {
  const [me, stats, traces, routing, evalCfg] = await Promise.allSettled([
    fetchMe(),
    fetchStats(),
    fetchTraces(),
    fetchRouting(),
    fetchEvalConfig(),
  ]);
  return {
    me: me.status === "fulfilled" ? (me.value as User | null) : null,
    stats: stats.status === "fulfilled" ? (stats.value as Stats) : null,
    traces:
      traces.status === "fulfilled" ? (traces.value as TraceSummary[]) : [],
    routing:
      routing.status === "fulfilled" ? (routing.value as RoutingModel[]) : [],
    eval:
      evalCfg.status === "fulfilled"
        ? (evalCfg.value as EvalConfigSnapshot)
        : null,
  };
}

export function Overview() {
  const { data, isLoading } = useQuery({
    queryKey: ["overview"],
    queryFn: fetchOverview,
    refetchInterval: 30_000,
  });

  const stats: Stats = data?.stats ?? {
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

  const routing: RoutingModel[] = data?.routing ?? [];
  const traces: TraceSummary[] = data?.traces ?? [];
  const evalCfg: EvalConfigSnapshot | null = data?.eval ?? null;
  const user: User | null = data?.me ?? null;

  return (
    <div className="overview">
      <section className="overview-hero">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Realtime ops console
          </div>
          <h1 className="hero-title">
            Welcome back
            {user?.email && (
              <>
                , <GradientText as="span">{user.email.split("@")[0]}</GradientText>
              </>
            )}
          </h1>
          <p className="hero-sub">
            Routing quality, live traffic, and per-user spend — at a glance.
          </p>
        </div>
        <div className="hero-cta">
          <button className="btn-neon" type="button">
            <Icon.play size={14} />
            Open Playground
          </button>
          <button className="btn-ghost" type="button">
            <Icon.chart size={14} />
            View Traces
          </button>
        </div>
      </section>

      <section className="stat-grid" aria-label="Activity (1h)">
        <Stat
          label="Requests (1h)"
          value={stats.total_requests.toLocaleString()}
          trend={null}
        />
        <Stat
          label="Error rate"
          value={`${(stats.error_rate * 100).toFixed(1)}%`}
          tone={
            stats.error_rate > 0.05
              ? "err"
              : stats.error_rate > 0.02
                ? "warn"
                : "ok"
          }
        />
        <Stat label="Avg latency" value={`${Math.round(stats.avg_latency_ms)} ms`} />
        <Stat
          label="P95 latency"
          value={`${Math.round(stats.p95_latency_ms)} ms`}
          tone={stats.p95_latency_ms > 2500 ? "warn" : "ok"}
        />
        <Stat
          label="Cache hit rate"
          value={`${(stats.cache_hit_rate * 100).toFixed(1)}%`}
        />
        <Stat
          label="Guardrail events"
          value={stats.guardrail_events.toLocaleString()}
        />
        <Stat label="Tokens" value={stats.total_tokens.toLocaleString()} />
        <Stat label="Cost" value={`$${stats.total_cost_usd.toFixed(4)}`} />
      </section>

      <section className="why-row" aria-label="Why FFX Nexus">
        <header className="panel-head section-heading">
          <h2>Why FFX Nexus</h2>
          <span className="panel-link muted">Sense · Govern · Defend</span>
        </header>
        <div className="tier-row">
          <TierCard
            eyebrow="Sense"
            title="Quality-aware auto"
            metric="auto alias"
            description="Single 'auto' alias ranks every model against a fresh composite of quality × cost × latency — re-ranked on every refresh so today's fast model is today's fast model."
            glow="pink"
            accent="#ec4899"
            ctaLabel="See routing"
            onClick={() => window.location.assign("/routing")}
          />
          <TierCard
            eyebrow="Govern"
            title="Strict BYOK + audit"
            metric="100% your keys"
            description="Per-user or per-org provider keys, encrypted at rest with a chart-rotated master, never logged. Every control-plane change is auditable with actor + target + detail."
            glow="cyan"
            accent="#22d3ee"
            ctaLabel="Open audit log"
            onClick={() => window.location.assign("/audit")}
          />
          <TierCard
            eyebrow="Defend"
            title="Eval-aware failover"
            metric="PII + SLM judge"
            description="Built-in heuristics (PII, completeness) and a local SLM judge flag regressions in real time; routing auto-rotates and an alert fires through the failover notifier."
            glow="violet"
            accent="#a855f7"
            ctaLabel="Tune evals"
            onClick={() => window.location.assign("/eval")}
          />
        </div>
      </section>

      <section className="tier-row" aria-label="Routing picks">
        <TierCard
          eyebrow="Routing · top quality"
          title="Best pick"
          metric={
            routing.length > 0
              ? routing.reduce((a, b) =>
                  a.eff_quality > b.eff_quality ? a : b,
                ).model
              : "—"
          }
          description="High overall quality; safe default for hand-off to a general agent."
          glow="pink"
          accent="#ec4899"
        />
        <TierCard
          eyebrow="Routing · fastest"
          title="Lowest p95"
          metric={
            routing.length > 0
              ? `${Math.round(
                  Math.min(...routing.map((r) => r.avg_latency_ms)),
                )} ms`
              : "—"
          }
          description="Sub-second p95 from samples; great for tight feedback loops."
          glow="cyan"
          accent="#22d3ee"
        />
        <TierCard
          eyebrow="Eval window"
          title={evalCfg?.routing.window ?? "1h"}
          metric={
            evalCfg
              ? `q ${((evalCfg.routing.weights.quality ?? 0.6) * 100).toFixed(0)}% / c ${(
                  (evalCfg.routing.weights.cost ?? 0.2) * 100
                ).toFixed(0)}% / l ${(
                  (evalCfg.routing.weights.latency ?? 0.2) * 100
                ).toFixed(0)}%`
              : "—"
          }
          description="Composite weights for auto routing. Adjust on the Eval page."
          glow="violet"
          accent="#a855f7"
        />
      </section>

      <section className="panel">
        <header className="panel-head">
          <h2>Recent traces</h2>
          <a className="panel-link" href="/traces">
            See all <Icon.arrowRight size={14} />
          </a>
        </header>
        <div className="trace-table" role="table">
          <div className="trace-row head" role="row">
            <span role="columnheader">Time</span>
            <span role="columnheader">Provider</span>
            <span role="columnheader">Model</span>
            <span role="columnheader">Status</span>
            <span role="columnheader" className="right">
              Latency
            </span>
            <span role="columnheader" className="right">
              Cost
            </span>
          </div>
          {traces.length === 0 && (
            <div className="trace-row empty" role="row">
              {isLoading ? "Loading…" : "No traces yet."}
            </div>
          )}
          {traces.slice(0, 10).map((t) => (
            <div className="trace-row" role="row" key={t.trace_id}>
              <span>{new Date(t.timestamp).toLocaleTimeString()}</span>
              <span>
                <span className="provider-tag">{t.provider_name}</span>
              </span>
              <span className="mono ellipsis">{t.request_model}</span>
              <span>
                <span
                  className={
                    "status-pill " +
                    (t.status_code >= 400 ? "is-err" : "is-ok")
                  }
                >
                  {t.status_code}
                </span>
              </span>
              <span className="right">{t.latency_ms} ms</span>
              <span className="right mono">${t.cost_usd.toFixed(5)}</span>
            </div>
          ))}
        </div>
      </section>
    </div>
  );
}

function Stat({
  label,
  value,
  tone,
  trend,
}: {
  label: string;
  value: string;
  tone?: "ok" | "warn" | "err";
  trend?: number | null;
}) {
  return (
    <div className={`stat ${tone ? "stat-" + tone : ""}`}>
      <div className="stat-label">{label}</div>
      <div className="stat-value">{value}</div>
      {trend !== null && (
        <div className="stat-trend" aria-hidden="true">
          {trend! > 0 ? "▲" : trend! < 0 ? "▼" : "—"} {Math.abs(trend ?? 0)}%
        </div>
      )}
    </div>
  );
}
