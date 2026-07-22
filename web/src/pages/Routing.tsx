import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { fetchEvalConfig, fetchRouting, type EvalConfigSnapshot, type RoutingModel } from "../api";
import { TierCard } from "../components/TierCard";
import { Chip } from "../components/Chip";
import { GradientText } from "../components/GradientText";

async function fetchRoutingBundle() {
  const [routing, cfg] = await Promise.allSettled([fetchRouting(), fetchEvalConfig()]);
  return {
    routing: routing.status === "fulfilled" ? (routing.value as RoutingModel[]) : [],
    cfg: cfg.status === "fulfilled" ? (cfg.value as EvalConfigSnapshot) : null,
  };
}

interface AliasRow {
  alias: string;
  description: string;
  candidateCount: number;
  candidates: string[];
}

function buildAliasRows(cfg: EvalConfigSnapshot | null): AliasRow[] {
  const groups = cfg?.routing.groups ?? {};
  const out: AliasRow[] = [];
  out.push({
    alias: "auto",
    description: "Quality-aware routing across the entire registered catalog.",
    candidateCount: cfg ? Object.values(groups).reduce((n, g) => n + g.length, 0) : 0,
    candidates: [],
  });
  for (const [alias, models] of Object.entries(groups)) {
    out.push({
      alias,
      description: customAliasDescription(alias),
      candidateCount: models.length,
      candidates: models,
    });
  }
  return out;
}

function customAliasDescription(alias: string): string {
  if (alias === "fast") return "Low-latency Gemini models for snappy responses.";
  if (alias === "smart") return "Highest-quality Gemini model for hard tasks.";
  return `Routing group "${alias}" — ${alias}.models defined at runtime.`;
}

export function Routing() {
  const { data } = useQuery({
    queryKey: ["routing"],
    queryFn: fetchRoutingBundle,
    refetchInterval: 60_000,
  });
  const routing = data?.routing ?? [];
  const cfg = data?.cfg ?? null;
  const aliases = buildAliasRows(cfg);

  // Rank candidates (model direct entries) by eff_quality
  const sorted = [...routing].sort((a, b) => b.eff_quality - a.eff_quality);
  const top = sorted.slice(0, 3);

  return (
    <div className="routing-page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Routing · 1h window
          </div>
          <h1 className="page-title">
            <GradientText as="span">Quality-aware</GradientText> routing
          </h1>
          <p className="page-sub">
            Pick a routing alias to send requests, or browse the underlying model
            ranks behind it.
          </p>
        </div>
        <div className="page-stats">
          <div className="page-stat">
            <div className="page-stat-label">aliases</div>
            <div className="page-stat-value">{aliases.length}</div>
          </div>
          <div className="page-stat">
            <div className="page-stat-label">q weight</div>
            <div className="page-stat-value">
              {((cfg?.routing.weights.quality ?? 0.6) * 100).toFixed(0)}%
            </div>
          </div>
          <div className="page-stat">
            <div className="page-stat-label">samples</div>
            <div className="page-stat-value">
              {routing.reduce((n, r) => n + r.samples, 0).toLocaleString()}
            </div>
          </div>
        </div>
      </header>

      <section className="routing-section">
        <h2 className="section-title">Aliases</h2>
        <div className="tier-grid">
          {aliases.map((a) => (
            <TierCard
              key={a.alias}
              eyebrow="routing alias"
              title={a.alias}
              metric={`${a.candidateCount} candidates`}
              description={a.description}
              glow={a.alias === "auto" ? "violet" : a.alias === "fast" ? "cyan" : "pink"}
              accent={
                a.alias === "auto" ? "#a855f7" :
                a.alias === "fast" ? "#22d3ee" :
                a.alias === "smart" ? "#ec4899" : "#a855f7"
              }
              ctaLabel={
                <Link to={`/routing/${encodeURIComponent(a.alias)}`}>
                  Open detail →
                </Link>
              }
            />
          ))}
        </div>
      </section>

      <section className="routing-section">
        <h2 className="section-title">Top models (by eff. quality)</h2>
        <div className="tier-grid">
          {top.length === 0 && (
            <div className="empty-card">
              No routing stats yet. Send some traffic via <code>auto</code> first.
            </div>
          )}
          {top.map((m) => (
            <TierCard
              key={m.model}
              eyebrow="model"
              title={m.model}
              metric={`q ${(m.eff_quality * 100).toFixed(0)}%`}
              description={`${m.samples.toLocaleString()} samples · ~${Math.round(m.avg_latency_ms)}ms latency · $${m.avg_cost_usd.toFixed(4)}`}
              glow="violet"
              accent="#a855f7"
            />
          ))}
        </div>
      </section>

      <section className="routing-section">
        <h2 className="section-title">All candidates</h2>
        <div className="panel">
          <div className="routing-table">
            <div className="routing-row is-head">
              <span>Model</span>
              <span>Eff. quality</span>
              <span>Latency</span>
              <span>Cost</span>
              <span>Samples</span>
              <span>Status</span>
            </div>
            {sorted.length === 0 ? (
              <div className="routing-row empty">No routing data yet.</div>
            ) : (
              sorted.map((m) => (
                <div className="routing-row" key={m.model}>
                  <span className="mono">{m.model}</span>
                  <span>
                    <ProgressBar value={m.eff_quality} />
                  </span>
                  <span className="mono">{Math.round(m.avg_latency_ms)} ms</span>
                  <span className="mono">${m.avg_cost_usd.toFixed(4)}</span>
                  <span className="mono">{m.samples.toLocaleString()}</span>
                  <span>
                    <Chip tone={m.eff_quality > 0.7 ? "ok" : m.eff_quality > 0.4 ? "warn" : "err"}>
                      {m.eff_quality > 0.7
                        ? "qualified"
                        : m.eff_quality > 0.4
                          ? "marginal"
                          : "explore"}
                    </Chip>
                  </span>
                </div>
              ))
            )}
          </div>
        </div>
      </section>
    </div>
  );
}

function ProgressBar({ value }: { value: number }) {
  const v = Math.max(0, Math.min(1, value || 0));
  return (
    <span className="bar">
      <span className="bar-fill" style={{ width: `${v * 100}%` }} aria-hidden="true" />
      <span className="bar-label">{Math.round(v * 100)}%</span>
    </span>
  );
}
