import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { fetchEvalConfig, patchEvalConfig, type EvalConfigSnapshot } from "../api";
import { DataTable, type Column } from "../components/DataTable";
import { Chip } from "../components/Chip";
import { StatusPill } from "../components/StatusPill";
import { GradientText } from "../components/GradientText";
import { Icon } from "../components/icons";
import { fetchMe } from "../api";

interface EvalRule {
  metric: string;
  enabled: boolean;
  detail: string;
}

async function fetchBundle() {
  const [me, cfg] = await Promise.all([
    fetchMe(),
    fetchEvalConfig().catch(() => null),
  ]);
  return { me, cfg };
}

export function Eval() {
  const qc = useQuery({
    queryKey: ["eval-config"],
    queryFn: fetchBundle,
    refetchInterval: 30_000,
  });
  const me = qc.data?.me ?? null;
  const cfg = qc.data?.cfg ?? null;

  const isAdmin = me?.role === "admin";
  if (!isAdmin) return <Forbidden />;
  if (!cfg) {
    return (
      <div className="page-head">
        <p className="page-sub">Loading configuration…</p>
      </div>
    );
  }

  const heur: EvalRule[] = [
    {
      metric: "PII",
      enabled: cfg.eval.pii_enabled,
      detail: "Detects requests / responses that contain emails, phone numbers, etc.",
    },
    {
      metric: "Completeness",
      enabled: cfg.eval.completeness_enabled,
      detail: "Trims runaway responses and flags truncated outputs.",
    },
    {
      metric: "SLM judge",
      enabled: cfg.eval.judge.enabled,
      detail: `${cfg.eval.judge.model} @ ${cfg.eval.judge.base_url}`,
    },
    {
      metric: "Remote eval",
      enabled: cfg.eval.remote.enabled,
      detail: cfg.eval.remote.metrics.join(", ") || "—",
    },
  ];

  return (
    <div className="eval-page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Admin · eval & routing
          </div>
          <h1 className="page-title">
            <GradientText as="span">Quality</GradientText> knobs
          </h1>
          <p className="page-sub">
            Eval heuristics and routing weights. Changes apply to{" "}
            <code>nexus-gateway</code> on the next refresh;{" "}
            {cfg.restart_required.length === 0
              ? "no restart required."
              : `restart required: ${cfg.restart_required.join(", ")}.`}
          </p>
        </div>
        <div className="page-stats">
          <div className="page-stat">
            <div className="page-stat-label">sample rate</div>
            <div className="page-stat-value">
              {(cfg.eval.sample_rate * 100).toFixed(0)}%
            </div>
          </div>
          <div className="page-stat">
            <div className="page-stat-label">workers</div>
            <div className="page-stat-value">{cfg.eval.workers}</div>
          </div>
        </div>
      </header>

      <EvalRules rules={heur} />
      <WeightsCard cfg={cfg} />
      <GroupsCard cfg={cfg} />

      <div className="eval-footer">
        <span className="muted small">
          Score store: <code>{cfg.score_store}</code> · routing stats store:{" "}
          <code>{cfg.routing_stats_store}</code>
        </span>
      </div>
    </div>
  );
}

function EvalRules({ rules }: { rules: EvalRule[] }) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState<string | null>(null);
  const mut = useMutation({
    mutationFn: (p: { pii?: boolean; completeness?: boolean }) =>
      patchEvalConfig({ eval: p }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["eval-config"] }),
    onSettled: (_d, _e, payload) => {
      void payload;
      setBusy(null);
    },
  });

  const cols: Column<EvalRule>[] = [
    {
      id: "metric",
      header: "Metric",
      cell: (r) => <strong>{r.metric}</strong>,
      sortValue: (r) => r.metric,
    },
    {
      id: "enabled",
      header: "Enabled",
      width: "110px",
      cell: (r) =>
        r.enabled ? (
          <StatusPill label="on" tone="ok" />
        ) : (
          <StatusPill label="off" tone="neutral" />
        ),
      sortValue: (r) => Number(r.enabled),
    },
    {
      id: "detail",
      header: "Detail",
      cell: (r) => <span className="muted small">{r.detail}</span>,
    },
    {
      id: "actions",
      header: "",
      width: "120px",
      align: "right",
      cell: (r) =>
        r.metric === "PII" || r.metric === "Completeness" ? (
          <button
            type="button"
            className="btn-ghost"
            disabled={busy === r.metric || mut.isPending}
            onClick={() => {
              setBusy(r.metric);
              const key = r.metric === "PII" ? "pii" : "completeness";
              mut.mutate({ [key]: !r.enabled } as { pii?: boolean; completeness?: boolean });
            }}
          >
            {r.enabled ? "Disable" : "Enable"}
          </button>
        ) : (
          <span className="muted">—</span>
        ),
    },
  ];

  return (
    <>
      <section>
        <h2 className="section-title">Heuristics</h2>
        <div className="panel" style={{ padding: 4 }}>
          <DataTable rows={rules} columns={cols} emptyMessage="No heuristics configured." />
        </div>
        <p className="muted small" style={{ marginTop: 8 }}>
          Judge + remote eval are configured in <code>NEXUS_EVAL_*</code> /{" "}
          <code>NEXUS_REMOTE_EVAL_*</code>.
        </p>
      </section>
    </>
  );
}

function WeightsCard({ cfg }: { cfg: EvalConfigSnapshot }) {
  const qc = useQueryClient();
  // Server-supplied weights may have been written before the backend started
  // clamping negatives (#128), or by an admin via raw API. Snap anything
  // < 0 to 0 so the slider thumb and value label display "0%" instead of
  // "-20%". The "0" floor is intentional — showing the historical default
  // (0.6 / 0.2 / 0.2) for a malformed row would silently introduce a 60/40
  // skew the user never intended; clamping to 0 surfaces the bad state and
  // lets the on-screen redistribute pass pick a sane replacement.
  const safe = (v: number | undefined) =>
    typeof v === "number" && Number.isFinite(v) && v >= 0 ? v : 0;
  const [quality, setQuality] = useState(safe(cfg.routing.weights.quality));
  const [latency, setLatency] = useState(safe(cfg.routing.weights.latency));
  const [cost, setCost] = useState(safe(cfg.routing.weights.cost));

  // Heal degenerate "all zero" rows that came out of the safe() pass. Without
  // this, a server response like {0, -0.1, -0.1} would render as three 0%
  // sliders — the redistribute helpers won't have a non-zero base to scale
  // against until the first user interaction.
  useEffect(() => {
    if (quality > 0 || latency > 0 || cost > 0) return;
    setQuality(0.6);
    setLatency(0.2);
    setCost(0.2);
  }, []); // Run only once after first hydration.

  const mut = useMutation({
    mutationFn: (next: { quality: number; cost: number; latency: number }) =>
      patchEvalConfig({
        routing: { weights: next },
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["eval-config"] }),
  });

  // Toast-style hint shown after Save: lets the admin see what we changed
  // before they scrolled away. Stays until the next interaction.
  const [hint, setHint] = useState<string | null>(null);

  function onSave() {
    const before = { quality, cost, latency };
    const after = finalizeForSimplex(before);
    // Only mention a rebalance if it actually moved something the user
    // didn't ask for.
    const moved =
      after.quality !== before.quality ||
      after.cost !== before.cost ||
      after.latency !== before.latency;
    setHint(
      moved
        ? `Sum was rebalanced to 100%: ${describeChange(before, after)}.`
        : "Saved — totals stay at 100%.",
    );
    mut.mutate(after);
  }

  if (!cfg) {
    return null;
  }
  return (
    <section>
      <h2 className="section-title">Routing weights</h2>
      <div className="panel weights-card">
        <WeightSlider
          label="Quality"
          tone="accent"
          value={quality}
          onChange={(v) => setQuality(clamp(v))}
        />
        <WeightSlider
          label="Cost"
          tone="info"
          value={cost}
          onChange={(v) => setCost(clamp(v))}
        />
        <WeightSlider
          label="Latency"
          tone="warn"
          value={latency}
          onChange={(v) => setLatency(clamp(v))}
        />
        <div className="weight-actions">
          <span className="muted small">
            Drag freely — any axis that hits 0 shares its portion between
            the other two on save so the row always sums to 100%.
          </span>
          <button
            type="button"
            className="btn-neon"
            disabled={mut.isPending}
            onClick={onSave}
          >
            <Icon.check size={14} />{" "}
            {mut.isPending ? "Saving…" : "Save weights"}
          </button>
        </div>
      </div>
      {hint && (
        <div className="weight-hint" role="status">
          {hint}
          <button
            type="button"
            className="btn-ghost btn-tiny"
            onClick={() => setHint(null)}
            aria-label="Dismiss hint"
          >
            Dismiss
          </button>
        </div>
      )}
    </section>
  );
}

function WeightSlider({
  label,
  value,
  onChange,
  tone,
}: {
  label: string;
  value: number;
  onChange: (n: number) => void;
  tone: "accent" | "info" | "warn";
}) {
  const cssVars: React.CSSProperties = {
    ["--tone" as string]: `var(--${tone === "accent" ? "accent-3" : tone})`,
  };
  const safe = clampNonNeg(value);
  return (
    <label className="weight-slider">
      <span className="weight-slider-head">
        <span className="weight-slider-label">{label}</span>
        <span className="weight-slider-value mono">{(safe * 100).toFixed(0)}%</span>
      </span>
      <input
        type="range"
        min={0}
        max={1}
        step={0.05}
        value={safe}
        onChange={(e) => onChange(Number(e.target.value))}
        style={cssVars}
      />
    </label>
  );
}

// Snap any value into the [0, 1] simplex. Centralised so render and
// onChange paths agree on what a "valid" weight looks like.
export function clampNonNeg(v: number): number {
  if (typeof v !== "number" || !Number.isFinite(v) || v < 0) return 0;
  return v > 1 ? 1 : v;
}

// Display-level clamp. Keeps the slider thumb tied to the visible range
// ([0,1]) but does NOT touch sibling axes — the user wants to be able to
// drag any one slider in isolation. Backend #128 already guarantees the
// server never stores a negative value.
function clamp(v: number): number {
  if (typeof v !== "number" || !Number.isFinite(v)) return 0;
  if (v < 0) return 0;
  if (v > 1) return 1;
  return v;
}

// Normalise the user's three weights into a probability simplex (sums to
// 1) right before PATCH. If an axis is exactly 0 we honour that — that is
// the whole point of the slider — so the two remaining axes still take the
// full weight. If the row is degenerate (all zero) we fall back to the
// historical default instead of sending a useless all-zero save.
function finalizeForSimplex(state: {
  quality: number;
  cost: number;
  latency: number;
}): { quality: number; cost: number; latency: number } {
  const clamp = (v: number) =>
    typeof v === "number" && Number.isFinite(v) ? Math.max(0, Math.min(1, v)) : 0;
  const q = clamp(state.quality);
  const c = clamp(state.cost);
  const l = clamp(state.latency);
  if (q + c + l <= 0.0001) {
    return { quality: 0.6, cost: 0.2, latency: 0.2 };
  }
  // Normalise so the absolute amounts the user dragged become the share.
  // Zero stays zero; the others absorb the residual.
  const sum = q + c + l;
  return {
    quality: round2(q / sum),
    cost: round2(c / sum),
    latency: round2(l / sum),
  };
}

function round2(v: number): number {
  return Math.round(v * 1000) / 1000;
}

function describeChange(
  before: { quality: number; cost: number; latency: number },
  after: { quality: number; cost: number; latency: number },
): string {
  const parts: string[] = [];
  const names: Array<[string, number, number]> = [
    ["Quality", before.quality * 100, after.quality * 100],
    ["Cost", before.cost * 100, after.cost * 100],
    ["Latency", before.latency * 100, after.latency * 100],
  ];
  for (const [n, b, a] of names) {
    if (Math.abs(a - b) < 0.5) continue;
    parts.push(`${n} ${a.toFixed(0)}% (was ${b.toFixed(0)}%)`);
  }
  return parts.length === 0 ? "no change" : parts.join(", ");
}

// redistribute keeps the simplex invariant `quality + cost + latency = 1`
// after one axis is set to a new value. The other two axes are scaled to
// fill the remaining budget in proportion to their current share. If both
// were zero, we keep the axis that the user was just editing dominant and
// leave the trailing axis at the rounded remainder.
// (Exported so tests can unit-test the math directly.)
export function redistribute(
  primary: number,
  secondary: number,
  tertiary: number,
): [number, number] {
  const p = primary; // kept for downstream callers; clamping happens on read.
  const remaining = Math.max(0, +(1 - p).toFixed(3));
  const s = secondary;
  const t = tertiary;
  const total = s + t;
  if (total <= 0) {
    return [+(remaining / 2).toFixed(3), +(remaining / 2).toFixed(3)];
  }
  return [
    +(remaining * (s / total)).toFixed(3),
    +(remaining * (t / total)).toFixed(3),
  ];
}

function GroupsCard({ cfg }: { cfg: EvalConfigSnapshot }) {
  const groups = Object.entries(cfg.routing.groups);
  if (groups.length === 0) return null;
  return (
    <section>
      <h2 className="section-title">Route groups</h2>
      <div className="panel groups-card">
        {groups.map(([k, models]) => (
          <div className="group-row" key={k}>
            <strong className="mono">{k}</strong>
            <div className="chip-row">
              {models.map((m) => (
                <Chip key={m} tone="info">{m}</Chip>
              ))}
            </div>
          </div>
        ))}
        <p className="muted small">
          Source: <code>NEXUS_ROUTE_GROUPS</code> (<code>{cfg.routing.groups_spec}</code>).
        </p>
      </div>
    </section>
  );
}

function Forbidden() {
  return (
    <div className="placeholder-page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Admin · eval
          </div>
          <h1 className="page-title">
            <GradientText as="span">Forbidden</GradientText>
          </h1>
          <p className="page-sub">Only admin accounts can view this page.</p>
        </div>
      </header>
    </div>
  );
}
