import { useQuery } from "@tanstack/react-query";
import { useState, useEffect, useMemo } from "react";
import { Link } from "react-router-dom";
import { GradientText } from "../components/GradientText";
import { TierCard } from "../components/TierCard";
import { Icon } from "../components/icons";
import {
  fetchEvalConfig,
  fetchMe,
  fetchProviderStats,
  fetchRouting,
  fetchStats,
  fetchTraces,
  type EvalConfigSnapshot,
  type ProviderStat,
  type RoutingModel,
  type Stats,
  type TraceSummary,
  type User,
} from "../api";
import {
  sessionizeTraces,
  type SessionRow,
} from "../lib/sessionize";

async function fetchOverview() {
  const [me, stats, traces, routing, evalCfg, provider] = await Promise.allSettled([
    fetchMe(),
    fetchStats(),
    fetchTraces(),
    fetchRouting(),
    fetchEvalConfig(),
    fetchProviderStats(),
  ]);
  return {
    me: me.status === "fulfilled" ? (me.value as User | null) : null,
    stats: stats.status === "fulfilled" ? (stats.value as Stats) : null,
    traces:
      traces.status === "fulfilled" ? traces.value.items : [],
    routing:
      routing.status === "fulfilled" ? (routing.value as RoutingModel[]) : [],
    eval:
      evalCfg.status === "fulfilled"
        ? (evalCfg.value as EvalConfigSnapshot)
        : null,
    provider:
      provider.status === "fulfilled"
        ? (provider.value as ProviderStat[])
        : [],
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
  // Sessionize the freshly fetched trace list once per render so the
  // roll-up order / counts stay stable while the operator drills in.
  const sessions = useMemo(() => sessionizeTraces(traces), [traces]);
  const evalCfg: EvalConfigSnapshot | null = data?.eval ?? null;
  const user: User | null = data?.me ?? null;
  const providerStats: ProviderStat[] = data?.provider ?? [];

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
          <Link to="/playground" className="btn-neon">
            <Icon.play size={14} />
            Open Playground
          </Link>
          <Link to="/traces" className="btn-ghost">
            <Icon.chart size={14} />
            View Traces
          </Link>
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

      <SpendByProvider providerStats={providerStats} />

      <SessionsTable sessions={sessions} isLoading={isLoading} />
    </div>
  );
}

// SessionsTable replaces the flat "Recent traces" roll on the overview
// with one row per session / agent loop turn. Clicking a row flips it
// open and renders the underlying TraceSummary list — same data the
// /api/traces endpoint already returns — so a click is a drill-down,
// not a new server-side join. A session with `trace_count > 1` and a
// wire-side `session_id` is a Cursor agent loop; we surface that
// explicitly so the operator understands the row, and so they can
// reach into a single turn via the trace list when they want to.
function SessionsTable({
  sessions,
  isLoading,
}: {
  sessions: SessionRow[];
  isLoading: boolean;
}) {
  const [openKey, setOpenKey] = useState<string | null>(null);
  const [children, setChildren] = useState<Record<string, TraceSummary[]>>({});

  // Reset the drill-down cache when the rolled-up list changes so old
  // expanded rows don't survive a refetch on the same dashboard tab.
  useEffect(() => {
    setOpenKey(null);
    setChildren({});
  }, [sessions]);

  if (sessions.length === 0) {
    return (
      <section className="panel">
        <header className="panel-head">
          <h2>Recent sessions</h2>
          <a className="panel-link" href="/traces">
            See all <Icon.arrowRight size={14} />
          </a>
        </header>
        <div className="trace-table" role="table">
          <div className="trace-row empty" role="row">
            {isLoading ? "Loading…" : "No traces yet."}
          </div>
        </div>
      </section>
    );
  }

  function toggle(key: string) {
    if (openKey === key) {
      setOpenKey(null);
      return;
    }
    setOpenKey(key);
    const row = sessions.find((s) => s.session_key === key);
    if (!row || row.trace_count <= 1) {
      // Single-turn "session" — meaning one trace, no roll-up. We
      // already render the roll-up row, so the drill-down would be
      // redundant; we just keep the row open with an explanatory
      // tooltip in place of the sub-list.
      return;
    }
    if (children[key]) return;
    // Pull the full flat list and filter by trace id. We re-use the
    // existing /api/traces no-filter page so we do not need a new
    // server endpoint; the in-memory filter is cheap at this scale.
    void loadChildren(key, row.trace_ids);
  }

  async function loadChildren(key: string, traceIds: string[]) {
    try {
      const page = await fetchTraces({});
      const wanted = new Set(traceIds);
      const filtered = page.items.filter((t) => wanted.has(t.trace_id));
      // Sort chronological-asc inside the session — newest trace at
      // the bottom — to match how the operator saw the conversation
      // unfold.
      filtered.sort((a, b) => a.timestamp.localeCompare(b.timestamp));
      setChildren((prev) => ({ ...prev, [key]: filtered }));
    } catch {
      setChildren((prev) => ({ ...prev, [key]: [] }));
    }
  }

  return (
    <section className="panel" aria-label="Recent sessions">
      <header className="panel-head">
        <h2>Recent sessions</h2>
        <a className="panel-link" href="/traces">
          See all <Icon.arrowRight size={14} />
        </a>
      </header>
      <div className="trace-table" role="table">
        <div className="trace-row head" role="row">
          <span role="columnheader">Last seen</span>
          <span role="columnheader">Provider</span>
          <span role="columnheader">Model</span>
          <span role="columnheader">Turns</span>
          <span role="columnheader">Status</span>
          <span role="columnheader" className="right">
            Avg latency
          </span>
          <span role="columnheader" className="right">
            Tokens
          </span>
          <span role="columnheader" className="right">
            Cost
          </span>
        </div>
        {sessions.slice(0, 10).map((s) => {
          const open = openKey === s.session_key;
          const traceCount =
            (children[s.session_key] ?? []).length || s.trace_count;
          return (
            <div className="session-stack" key={s.session_key}>
              <div
                className={"trace-row session-row" + (open ? " is-open" : "")}
                role="row"
                onClick={() => toggle(s.session_key)}
                tabIndex={0}
                aria-expanded={open}
                title={
                  s.from_wire
                    ? `Session id: ${s.session_key}`
                    : "Merged by time window — no session_id on the wire"
                }
              >
                <span>
                  {new Date(s.last_at).toLocaleTimeString()}
                  {!s.from_wire && (
                    <span className="session-badge" aria-label="heuristic merge">
                      heur
                    </span>
                  )}
                </span>
                <span>
                  <span className="provider-tag">{s.provider_name}</span>
                </span>
                <span
                  className="mono ellipsis"
                  title={
                    s.response_model && s.response_model !== s.request_model
                      ? `requested: ${s.request_model}\nserved: ${s.response_model}`
                      : s.request_model
                  }
                >
                  {s.request_model}
                  {s.response_model && s.response_model !== s.request_model && (
                    <span className="model-served-pill">
                      → {s.response_model}
                    </span>
                  )}
                </span>
                <span className="mono">{s.trace_count}</span>
                <span>
                  {s.first_error ? (
                    <span className="status-pill is-err">
                      {s.first_error.status}
                    </span>
                  ) : (
                    <span className="status-pill is-ok">ok</span>
                  )}
                </span>
                <span className="right">
                  {Math.round(s.avg_latency_ms)} ms
                </span>
                <span
                  className="right mono"
                  title={`in ${formatThousands(s.total_input_tokens)} • out ${formatThousands(s.total_output_tokens)}`}
                >
                  {formatThousands(s.total_tokens)}
                </span>
                <span className="right mono">${s.total_cost_usd.toFixed(5)}</span>
              </div>
              {open && (
                <div className="session-drill">
                  {traceCount > 1 ? (
                    !children[s.session_key] ? (
                      <div className="trace-row empty">Loading turns…</div>
                    ) : (children[s.session_key] ?? []).length === 0 ? (
                      <div className="trace-row empty">No turns found.</div>
                    ) : (
                      children[s.session_key]!.map((t) => (
                        <div className="trace-row sub-row" key={t.trace_id}>
                          <span>
                            {new Date(t.timestamp).toLocaleTimeString()}
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
                          <span className="right">
                            {t.latency_ms} ms
                          </span>
                          <span
                            className="right mono"
                            title={`in ${formatThousands(t.input_tokens ?? 0)} • out ${formatThousands(t.output_tokens ?? 0)}`}
                          >
                            {formatThousands(
                              t.total_tokens ??
                                ((t.input_tokens ?? 0) + (t.output_tokens ?? 0))
                            )}
                          </span>
                          <span className="right mono">
                            ${Number(t.cost_usd ?? 0).toFixed(5)}
                          </span>
                        </div>
                      ))
                    )
                  ) : (
                    <div className="trace-row empty">
                      Single-turn session — same as the row above.
                    </div>
                  )}
                </div>
              )}
            </div>
          );
        })}
      </div>
    </section>
  );
}

function formatUsd(n: number): string {
  if (!Number.isFinite(n) || n === 0) return "$0.0000";
  if (n < 0.0001) return `$${n.toExponential(2)}`;
  if (n < 1) return `$${n.toFixed(4)}`;
  return `$${n.toFixed(2)}`;
}

// formatThousands turns e.g. 12345 into "12,345" so the Tokens column
// stays readable when a multi-turn session hits six digits. We expose
// the raw count via the title= hover (handled inline at the call site
// with in/out breakdown) so the comma-formatted value here is purely
// for the eye.
function formatThousands(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return "0";
  return Math.round(n).toLocaleString("en-US");
}

// SpendByProvider renders a LiteLLM-style horizontal bar widget grouped by
// provider_name. The server endpoint behind it is /api/stats/providers (30s
// in-process cache, so multiple dashboard tabs polling together are not
// hammering ClickHouse). The localScalePct here renormalises within the
// visible providers so the largest bar is always 100%, even when the
// absolute total is small — at $0.05 across 4 providers the user can still
// see which one is the biggest spender rather than four equal-width stubs.
function SpendByProvider({ providerStats }: { providerStats: ProviderStat[] }) {
  const totalCost = providerStats.reduce((acc, p) => acc + (p.cost_usd || 0), 0);
  const hasData = providerStats.length > 0;
  const maxCost = Math.max(1e-9, ...providerStats.map((p) => p.cost_usd || 0));

  return (
    <section className="panel" aria-label="Spend by provider">
      <header className="panel-head">
        <h2>Spend by provider</h2>
        <span className="panel-link muted">
          {hasData ? `Total ${formatUsd(totalCost)} · last 1h` : "No spend yet"}
        </span>
      </header>
      <div className="spend-by-provider">
        {!hasData && (
          <div className="spend-by-provider__empty">
            Connect a provider key and run a request — spend will roll up here
            as gateway_traces aggregate.
          </div>
        )}
        {providerStats.map((p) => {
          const pct = ((p.cost_usd || 0) / maxCost) * 100;
          const share = totalCost > 0 ? ((p.cost_usd || 0) / totalCost) * 100 : 0;
          return (
            <div className="spend-row" key={p.provider} role="row">
              <span className="spend-row__name">
                <span className="provider-tag">{p.provider}</span>
              </span>
              <span className="spend-row__bar" role="presentation">
                <span
                  className="spend-row__bar-fill"
                  style={{ width: `${Math.max(1, pct)}%` }}
                />
              </span>
              <span className="spend-row__cost mono" role="cell">
                {formatUsd(p.cost_usd)}
              </span>
              <span className="spend-row__share mono muted" role="cell">
                {share.toFixed(1)}%
              </span>
              <span className="spend-row__requests muted" role="cell">
                {p.requests.toLocaleString()} req
              </span>
            </div>
          );
        })}
      </div>
    </section>
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
