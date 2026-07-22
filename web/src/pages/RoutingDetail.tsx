import { Link, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { fetchEvalConfig, fetchRouting, type EvalConfigSnapshot, type RoutingModel } from "../api";
import { GradientText } from "../components/GradientText";
import { TierCard } from "../components/TierCard";
import { Chip } from "../components/Chip";
import { Icon } from "../components/icons";

async function fetchBundle() {
  const [r, c] = await Promise.allSettled([fetchRouting(), fetchEvalConfig()]);
  return {
    routing: r.status === "fulfilled" ? (r.value as RoutingModel[]) : [],
    cfg: c.status === "fulfilled" ? (c.value as EvalConfigSnapshot) : null,
  };
}

export function RoutingDetail() {
  const { alias = "auto" } = useParams();
  const decoded = decodeURIComponent(alias);
  const { data } = useQuery({ queryKey: ["routing-detail"], queryFn: fetchBundle });
  const routing = data?.routing ?? [];
  const cfg = data?.cfg ?? null;

  const explicit = cfg?.routing.groups[decoded];
  const candidates = decoded === "auto" ? routing.map((r) => r.model) : explicit ?? [];
  const byName = new Map(routing.map((r) => [r.model, r]));
  const ranked = candidates
    .map((id) => byName.get(id))
    .filter((r): r is RoutingModel => Boolean(r))
    .sort((a, b) => b.eff_quality - a.eff_quality);

  const aliasIsUnknown =
    decoded !== "auto" && !explicit;

  const weights = cfg?.routing.weights ?? { quality: 0.6, cost: 0.2, latency: 0.2 };

  return (
    <div className="routing-detail-page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <Link to="/routing" className="eyebrow-link">
              ← All aliases
            </Link>
          </div>
          <h1 className="page-title">
            <GradientText as="span">{decoded}</GradientText>
          </h1>
          <p className="page-sub">
            {decoded === "auto"
              ? "Quality-aware across the entire registered catalog."
              : explicit
                ? `${explicit.length} candidates in this group.`
                : "Unknown alias — not present in current route groups."}
          </p>
          {cfg && (
            <div className="weight-row">
              <span>Composite weights:</span>
              <Chip tone="accent">q {(weights.quality ?? 0.6) * 100}%</Chip>
              <Chip tone="info">c {(weights.cost ?? 0.2) * 100}%</Chip>
              <Chip tone="info">l {(weights.latency ?? 0.2) * 100}%</Chip>
              <Chip tone="neutral">window {cfg.routing.window}</Chip>
              <Chip tone={cfg.routing.load_balance ? "ok" : "neutral"}>
                load-balance {cfg.routing.load_balance ? "on" : "off"}
              </Chip>
            </div>
          )}
        </div>
        <div className="page-stats">
          <div className="page-stat">
            <div className="page-stat-label">candidates</div>
            <div className="page-stat-value">{candidates.length}</div>
          </div>
          <div className="page-stat">
            <div className="page-stat-label">best q.</div>
            <div className="page-stat-value">
              {ranked.length === 0 ? "—" : `${Math.round((ranked[0].eff_quality ?? 0) * 100)}%`}
            </div>
          </div>
        </div>
      </header>

      {aliasIsUnknown && (
        <div className="callout">
          Alias <code>{decoded}</code> is not defined in{" "}
          <code>NEXUS_ROUTE_GROUPS</code>. Requests with this <code>model</code> field will
          return <code>model_not_found</code> from the gateway.
        </div>
      )}

      <section className="routing-section">
        <h2 className="section-title">Try this alias</h2>
        <div className="tier-grid">
          <TierCard
            eyebrow="Open in Playground"
            title={decoded}
            metric="send →"
            description="Pressing the button below opens the playground pre-set with this alias and a quick smoke prompt."
            glow={decoded === "fast" ? "cyan" : decoded === "smart" ? "pink" : "violet"}
            accent={
              decoded === "fast" ? "#22d3ee" :
              decoded === "smart" ? "#ec4899" : "#a855f7"
            }
            ctaLabel={
              <Link to={`/playground?model=${encodeURIComponent(decoded)}`}>
                Open Playground <Icon.arrowRight size={12} />
              </Link>
            }
          />
        </div>
      </section>

      <section className="routing-section">
        <h2 className="section-title">Candidates (ranked)</h2>
        <div className="panel">
          {ranked.length === 0 ? (
            <div className="empty-card">
              No measured candidates yet. Run some traffic via{" "}
              <code>model: {decoded}</code> to populate stats.
            </div>
          ) : (
            <div className="routing-table">
              <div className="routing-row is-head">
                <span>#</span>
                <span>Model</span>
                <span>Eff. quality</span>
                <span>Quality</span>
                <span>Safety</span>
                <span>Latency</span>
                <span>Cost</span>
                <span>Samples</span>
              </div>
              {ranked.map((m, i) => (
                <div className="routing-row" key={m.model}>
                  <span className="mono rank">{i + 1}</span>
                  <span className="mono">{m.model}</span>
                  <span>
                    <Bar value={m.eff_quality} />
                  </span>
                  <span className="mono">{Math.round((m.quality ?? 0) * 100)}%</span>
                  <span className="mono">{Math.round((m.safety_pass_rate ?? 0) * 100)}%</span>
                  <span className="mono">{Math.round(m.avg_latency_ms)} ms</span>
                  <span className="mono">${m.avg_cost_usd.toFixed(4)}</span>
                  <span className="mono">{m.samples.toLocaleString()}</span>
                </div>
              ))}
            </div>
          )}
        </div>
      </section>
    </div>
  );
}

function Bar({ value }: { value: number }) {
  const v = Math.max(0, Math.min(1, value || 0));
  return (
    <span className="bar">
      <span
        className="bar-fill"
        style={{ width: `${v * 100}%` }}
        aria-hidden="true"
      />
      <span className="bar-label">{Math.round(v * 100)}%</span>
    </span>
  );
}
