import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Link } from "react-router-dom";
import { GradientText } from "../components/GradientText";
import { ResizableGrid, type ColumnSpec, type RowSpec } from "../components/ResizableGrid";
import { TierCard } from "../components/TierCard";
import { Icon } from "../components/icons";
import { formatExact, formatTokens } from "../lib/format";
import {
  fetchEvalConfig,
  fetchMe,
  fetchProviderStats,
  fetchRouting,
  fetchStats,
  fetchTraces,
  fetchTurns,
  type EvalConfigSnapshot,
  type ProviderStat,
  type RoutingModel,
  type Stats,
  type TraceSummary,
  type TurnSummary,
  type User,
} from "../api";

async function fetchOverview() {
  const [me, stats, turns, routing, evalCfg, provider] = await Promise.allSettled([
    fetchMe(),
    fetchStats(),
    fetchTurns({ limit: 10 }),
    fetchRouting(),
    fetchEvalConfig(),
    fetchProviderStats(),
  ]);
  return {
    me: me.status === "fulfilled" ? (me.value as User | null) : null,
    stats: stats.status === "fulfilled" ? (stats.value as Stats) : null,
    turns: turns.status === "fulfilled" ? (turns.value as TurnSummary[]) : [],
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
    total_input_tokens: 0,
    total_output_tokens: 0,
    total_cost_usd: 0,
    cache_hits: 0,
    cache_hit_rate: 0,
    guardrail_events: 0,
  };

  const routing: RoutingModel[] = data?.routing ?? [];
  const turns: TurnSummary[] = data?.turns ?? [];
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
        <Stat
          label="Prompt tokens"
          value={formatTokens(stats.total_input_tokens ?? 0)}
        />
        <Stat
          label="Completion tokens"
          value={formatTokens(stats.total_output_tokens ?? 0)}
        />
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

      <RecentTurnsList turns={turns} isLoading={isLoading} />
    </div>
  );
}

// RecentTurnsList renders one row per agent turn: the user's question
// plus every model call the agent made while answering it, rolled up by
// the gateway-derived turn_id. Asking one question and watching ten rows
// scroll past is noise — the interesting unit is the turn, and the calls
// underneath it are a click away.
//
// This is not the old time-window heuristic that got reverted. Grouping
// happens server-side on a key derived from the request payload, so two
// unrelated questions that happen to land seconds apart stay apart.
// Column template for the parent turn-row. Defaults match the legacy
// fixed grid 84 / 104 / minmax(140px, 1.5fr) / 58 / 74 / 88 / 84 / 92.
const TURN_COLUMNS: ColumnSpec[] = [
  { id: "time", header: "Time", initialWidth: 84 },
  { id: "provider", header: "Provider", initialWidth: 104 },
  { id: "model", header: "Model", initialWidth: "minmax(140px, 1.5fr)" },
  { id: "calls", header: "Calls", initialWidth: 58, align: "right" },
  { id: "status", header: "Status", initialWidth: 74 },
  { id: "latency", header: "Latency", initialWidth: 88, align: "right" },
  { id: "tokens", header: "Tokens", initialWidth: 84, align: "right" },
  { id: "cost", header: "Cost", initialWidth: 92, align: "right" },
];

// Sub-row template drops Provider and Calls (every call in a turn shares
// the parent's provider; a call has no sub-count). Column widths live in
// a separate localStorage key so the drill-down panel and the
// turn-row above can be tuned independently. The first cell takes a
// colSpan to visually absorb the parent's Provider+Time slot — this is
// how we reuse a ResizableGrid instance for a different shape without
// forcing the consumer to re-think column-to-cell mapping.
const SUB_COLUMNS: ColumnSpec[] = [
  { id: "time", header: "Time", initialWidth: 84 },
  { id: "provider", header: "Provider", initialWidth: 104 },
  { id: "model", header: "Model", initialWidth: "minmax(140px, 1.5fr)" },
  { id: "status", header: "Status", initialWidth: 74 },
  { id: "latency", header: "Latency", initialWidth: 88, align: "right" },
  { id: "tokens", header: "Tokens", initialWidth: 84, align: "right" },
  { id: "cost", header: "Cost", initialWidth: 92, align: "right" },
];

// buildTurnRow cells each turn into a RowSpec using the 8-column
// template. The cells array is index-aligned with TURN_COLUMNS.
function buildTurnRow(
  t: TurnSummary,
  isOpen: boolean,
  canExpand: boolean,
  toggleOpen: () => void,
): RowSpec {
  return {
    rowKey: `turn-${t.turn_id}`,
    rowTestId: `overview-turn-row-${t.turn_id}`,
    className:
      "trace-row turn-row" +
      (canExpand ? " is-expandable" : "") +
      (isOpen ? " is-open" : ""),
    role: canExpand ? "button" : "row",
    tabIndex: canExpand ? 0 : undefined,
    ariaExpanded: canExpand ? isOpen : undefined,
    onClick: canExpand ? toggleOpen : undefined,
    cells: [
      {
        node: (
          <span title={new Date(t.first_at).toLocaleString()}>
            {new Date(t.last_at).toLocaleTimeString()}
          </span>
        ),
      },
      {
        node: (
          <span>
            <span className="provider-tag">{t.provider_name}</span>
          </span>
        ),
      },
      {
        node: (
          <span className="mono ellipsis rg-cell-truncate" title={t.request_model}>
            {t.request_model}
          </span>
        ),
      },
      {
        node: (
          <span className="right mono">
            {canExpand ? (
              <span className="turn-calls">
                <Icon.arrowRight
                  size={11}
                  className={
                    "turn-caret" + (isOpen ? " turn-caret--open" : "")
                  }
                />
                {t.trace_count}
              </span>
            ) : (
              <span className="muted">1</span>
            )}
          </span>
        ),
      },
      {
        node: (
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
        ),
      },
      { node: <span className="right">{t.latency_ms} ms</span> },
      {
        node: (
          <span
            className="right mono"
            title={`in ${formatExact(t.input_tokens ?? 0)} • out ${formatExact(t.output_tokens ?? 0)}`}
          >
            {formatTokens(
              t.total_tokens ??
                (t.input_tokens ?? 0) + (t.output_tokens ?? 0),
            )}
          </span>
        ),
      },
      {
        node: (
          <span className="right mono">
            ${Number(t.cost_usd ?? 0).toFixed(5)}
          </span>
        ),
      },
    ],
  };
}

function RecentTurnsList({
  turns,
  isLoading,
}: {
  turns: TurnSummary[];
  isLoading: boolean;
}) {
  const [expanded, setExpanded] = useState<string | null>(null);

  const rows: RowSpec[] = turns.map((t) => {
    const canExpand = t.trace_count > 1;
    const isOpen = expanded === t.turn_id;
    return buildTurnRow(t, isOpen, canExpand, () =>
      setExpanded(isOpen ? null : t.turn_id),
    );
  });

  return (
    <section className="panel" aria-label="Recent turns">
      <header className="panel-head">
        <h2>Recent turns</h2>
        <a className="panel-link" href="/traces">
          See all <Icon.arrowRight size={14} />
        </a>
      </header>
      <div className="trace-table" role="table">
        <ResizableGrid
          columns={TURN_COLUMNS}
          storageKey="nexus:rg:overview-turns"
          groups={[{ rows }]}
        />
        {turns.length === 0 ? (
          <div className="trace-row empty" role="row">
            {isLoading ? "Loading…" : "No traffic yet."}
          </div>
        ) : (
          turns.map((t) => {
            const isOpen = expanded === t.turn_id;
            if (!isOpen) return null;
            return (
              <div key={`drill-${t.turn_id}`}>
                <TurnCalls turnID={t.turn_id} />
              </div>
            );
          })
        )}
      </div>
    </section>
  );
}

// TurnCalls lazily fetches the individual calls behind one turn. Mounted
// only while its row is expanded, so collapsing and re-expanding refetches
// — which is what you want on a live console where a turn may still be
// running.
function TurnCalls({ turnID }: { turnID: string }) {
  const { data, isLoading } = useQuery({
    queryKey: ["turn-calls", turnID],
    queryFn: () => fetchTraces({ turn: turnID, limit: 50 }),
  });

  // The server orders newest-first for paging. Inside a turn we want the
  // agent's own sequence, so read it back the other way.
  const calls: TraceSummary[] = [...(data?.items ?? [])].reverse();

  const subRows: RowSpec[] = calls.map((c, i) => ({
    rowKey: `sub-${c.trace_id}`,
    className: "trace-row sub-row",
    cells: [
      // colSpan: 2 absorbs the parent's time+provider slot — the sub-
      // shape doesn't render Provider (every call in a turn shares the
      // parent's provider), so we collapse those two grid columns on
      // a single cell. This keeps model/status/latency/tokens/cost
      // aligned with the parent turn-row above.
      {
        colSpan: 2,
        node: (
          <span className="muted">
            #{i + 1} · {new Date(c.timestamp).toLocaleTimeString()}
          </span>
        ),
      },
      {
        node: (
          <span className="mono ellipsis rg-cell-truncate" title={c.request_model}>
            {c.request_model}
          </span>
        ),
      },
      {
        node: (
          <span>
            <span
              className={
                "status-pill " +
                (c.status_code >= 400 ? "is-err" : "is-ok")
              }
            >
              {c.status_code}
            </span>
          </span>
        ),
      },
      {
        node: <span className="right mono">{c.latency_ms} ms</span>,
      },
      {
        node: (
          <span
            className="right mono"
            title={`in ${formatExact(c.input_tokens ?? 0)} • out ${formatExact(c.output_tokens ?? 0)}`}
          >
            {formatTokens(
              c.total_tokens ??
                (c.input_tokens ?? 0) + (c.output_tokens ?? 0),
            )}
          </span>
        ),
      },
      {
        node: (
          <span className="right mono">
            ${Number(c.cost_usd ?? 0).toFixed(5)}
          </span>
        ),
      },
    ],
  }));

  if (isLoading) {
    return <div className="session-drill muted">Loading calls…</div>;
  }
  if (calls.length === 0) {
    return <div className="session-drill muted">No calls found.</div>;
  }
  return (
    <div className="session-drill">
      <ResizableGrid
        columns={SUB_COLUMNS}
        storageKey="nexus:rg:overview-turns-sub"
        groups={[{ showHeader: false, rows: subRows }]}
      />
    </div>
  );
}

function formatUsd(n: number): string {
  if (!Number.isFinite(n) || n === 0) return "$0.0000";
  if (n < 0.0001) return `$${n.toExponential(2)}`;
  if (n < 1) return `$${n.toFixed(4)}`;
  return `$${n.toFixed(2)}`;
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
