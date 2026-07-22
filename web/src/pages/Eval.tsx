import { useState } from "react";
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
  const [quality, setQuality] = useState(cfg.routing.weights.quality ?? 0.6);
  const [latency, setLatency] = useState(cfg.routing.weights.latency ?? 0.2);
  const [cost, setCost] = useState(cfg.routing.weights.cost ?? 0.2);

  const mut = useMutation({
    mutationFn: () =>
      patchEvalConfig({
        routing: {
          weights: {
            quality,
            cost,
            latency,
          },
        },
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["eval-config"] }),
  });
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
          onChange={(v) => {
            setQuality(v);
            setLatency(+(1 - v - cost).toFixed(3));
          }}
        />
        <WeightSlider
          label="Cost"
          tone="info"
          value={cost}
          onChange={(v) => {
            setCost(v);
            setLatency(+(1 - quality - v).toFixed(3));
          }}
        />
        <WeightSlider
          label="Latency"
          tone="warn"
          value={latency}
          onChange={(v) => {
            setLatency(v);
            setCost(+(1 - quality - v).toFixed(3));
          }}
        />
        <div className="weight-actions">
          <span className="muted small">
            Sum is auto-balanced to 1.00 on save.
          </span>
          <button
            type="button"
            className="btn-neon"
            disabled={mut.isPending}
            onClick={() => mut.mutate()}
          >
            <Icon.check size={14} /> Save weights
          </button>
        </div>
      </div>
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
  return (
    <label className="weight-slider">
      <span className="weight-slider-head">
        <span className="weight-slider-label">{label}</span>
        <span className="weight-slider-value mono">{(value * 100).toFixed(0)}%</span>
      </span>
      <input
        type="range"
        min={0}
        max={1}
        step={0.05}
        value={value}
        onChange={(e) => onChange(Number(e.target.value))}
        style={cssVars}
      />
    </label>
  );
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
