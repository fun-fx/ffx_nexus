import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  fetchEvalConfig,
  fetchEvalProfiles,
  patchEvalConfig,
  patchEvalProfile,
  type EvalConfigSnapshot,
} from "../api";
import { DataTable, type Column } from "../components/DataTable";
import { Chip } from "../components/Chip";
import { GradientText } from "../components/GradientText";
import { Icon } from "../components/icons";
import { LabelToggle } from "../components/LabelToggle";
import { fetchMe } from "../api";
import { EvalProfilesCard } from "./EvalProfiles";

interface EvalRule {
  metric: string;
  enabled: boolean;
  detail: string;
  /**
   * Optional link from a top-table row down to the underlying
   * EvalProfile. Populated for SLM judge / Remote eval so admin
   * actions (Change configuration / Disable evaluation) can route
   * directly to the right profile row.
   */
  profileId?: string | null;
}

async function fetchBundle() {
  const [me, cfg] = await Promise.all([
    fetchMe(),
    fetchEvalConfig().catch(() => null),
  ]);
  return { me, cfg };
}

export function Eval() {
  const qc = useQueryClient();
  const evalConfigQ = useQuery({
    queryKey: ["eval-config"],
    queryFn: fetchBundle,
    refetchInterval: 30_000,
  });
  const me = evalConfigQ.data?.me ?? null;
  const cfg = evalConfigQ.data?.cfg ?? null;

  const isAdmin = me?.role === "admin";

  // Eval Profiles list — TanStack dedupes across the page and the
  // EvalProfilesCard so the network is a single round trip. The
  // top-of-table rows (SLM judge / Remote eval) link to specific
  // profile ids so admin's "Change configuration" / "Disable
  // evaluation" buttons operate on the right row without a second
  // round-trip to resolve names.
  const profilesQ = useQuery({
    queryKey: ["eval-profiles"],
    queryFn: fetchEvalProfiles,
    refetchInterval: 30_000,
  });

  // Stable id resolution: the backend seeds default-judge and
  // default-remote as org-scoped profiles at first boot. If a custom
  // profile of the same kind exists we prefer it (admin already chose
  // it specifically), otherwise the seeded default is the right one
  // to operate on.
  const profileIdByKind = useMemo(() => {
    const out: Record<string, string | undefined> = {
      slm_judge: undefined,
      remote_eval: undefined,
    };
    const list = profilesQ.data ?? [];
    for (const kind of ["slm_judge", "remote_eval"] as const) {
      const matches = list.filter((p) => p.kind === kind);
      out[kind] =
        matches.find((p) => p.id === `default-${kind === "slm_judge" ? "judge" : "remote"}`)?.id ??
        matches.find((p) => p.scope === "org")?.id ??
        matches[0]?.id;
    }
    return out;
  }, [profilesQ.data]);

  // Profile id we ask EvalProfilesCard to open in the editor next
  // time it renders. Lifted up here because the top table owns the
  // "Change configuration" buttons.
  const [pendingOpenProfileId, setPendingOpenProfileId] = useState<string | null>(null);

  // Profile toggle mutation — backend-facing patch on the same
  // /api/eval/profiles/{id} endpoint, opted-in for admin only via
  // console RBAC. The EvalProfilesCard's own toggle on the row below
  // already uses the same path; this one is just a faster shortcut.
  const profileToggleMut = useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) =>
      patchEvalProfile(id, { enabled }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["eval-config"] });
      qc.invalidateQueries({ queryKey: ["eval-profiles"] });
    },
  });

  // Lifted weight state so the upper stats bar and the slider card stay in
  // sync after `Save weights` re-normalises the row. Hooks must run on
  // every render *before* any early return — otherwise the previous
  // render's hook count disagrees with the current one and React throws.
  // The initial seed is 0 because we run before the config is necessarily
  // present; we hydrate from `cfg.routing.weights` in the effect below.
  const [quality, setQuality] = useState(0);
  const [cost, setCost] = useState(0);
  const [latency, setLatency] = useState(0);

  // Re-sync weights whenever a fresh config fetch lands. This is the only
  // path that writes the initial values into local state — the lazy
  // `useState` initialiser fires once at mount and would otherwise be
  // permanently clamped to 0 if the first render observed `cfg = null`.
  // We also heal "all-zero" rows (server may have written negatives before
  // #128) by falling back to the historical 60/20/20 default.
  const cfgWeights = cfg?.routing.weights;
  useEffect(() => {
    if (!cfgWeights) return;
    const safe = (v: number | undefined) =>
      typeof v === "number" && Number.isFinite(v) && v >= 0 ? v : 0;
    const q = safe(cfgWeights.quality);
    const c = safe(cfgWeights.cost);
    const l = safe(cfgWeights.latency);
    if (q > 0 || c > 0 || l > 0) {
      setQuality(q);
      setCost(c);
      setLatency(l);
    } else {
      // Server read a malformed all-zero row. Fall back to the historical
      // default rather than rendering three 0% sliders.
      setQuality(0.6);
      setCost(0.2);
      setLatency(0.2);
    }
  }, [
    cfgWeights?.quality,
    cfgWeights?.cost,
    cfgWeights?.latency,
    cfgWeights,
  ]);
  void quality; void cost; void latency;

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
      profileId: profileIdByKind["slm_judge"] ?? null,
    },
    {
      metric: "Remote eval",
      enabled: cfg.eval.remote.enabled,
      detail: cfg.eval.remote.metrics.join(", ") || "—",
      profileId: profileIdByKind["remote_eval"] ?? null,
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
          <div className="page-stat" data-stat="quality">
            <div className="page-stat-label">quality</div>
            <div className="page-stat-value">
              {(quality * 100).toFixed(0)}%
            </div>
          </div>
          <div className="page-stat" data-stat="cost">
            <div className="page-stat-label">cost</div>
            <div className="page-stat-value">
              {(cost * 100).toFixed(0)}%
            </div>
          </div>
          <div className="page-stat" data-stat="latency">
            <div className="page-stat-label">latency</div>
            <div className="page-stat-value">
              {(latency * 100).toFixed(0)}%
            </div>
          </div>
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

      <EvalRules
          rules={heur}
          isAdmin={isAdmin}
          profileIdByKind={profileIdByKind}
          onRequestOpenProfile={setPendingOpenProfileId}
          onDisableProfile={(id) => profileToggleMut.mutate({ id, enabled: false })}
          disableBusy={profileToggleMut.isPending}
        />
      <EvalProfilesCard
        isAdmin={isAdmin}
        pendingOpenProfileId={pendingOpenProfileId}
        onPendingOpenConsumed={() => setPendingOpenProfileId(null)}
      />
      <WeightsCard
        cfg={cfg}
        quality={quality}
        cost={cost}
        latency={latency}
        onQuality={setQuality}
        onCost={setCost}
        onLatency={setLatency}
        onSaved={(after) => {
          setQuality(after.quality);
          setCost(after.cost);
          setLatency(after.latency);
        }}
      />
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

// The SLM judge / Remote eval rows are not toggleable from the console
// in the same way PII / Completeness are: their BaseURL/Model/Metrics
// are seeded from env at boot, and the operator's control surface is
// the seeded default-* profile row below. This cell renders a pill
// status that's read-only, plus admin-only action buttons that
// delegate to the profile editor / to a profile-level enable toggle.
// Non-admins see only the pill and a hint that the row is locked to
// env config.
function EnvDrivenCell({
  rule,
  isAdmin,
  busy,
  onChangeConfig,
  onDisable,
}: {
  rule: EvalRule;
  isAdmin: boolean;
  busy: boolean;
  onChangeConfig: () => void;
  onDisable: () => void;
}) {
  const detail = rule.profileId
    ? `Model: ${rule.detail}`
    : `No seeded profile yet for ${rule.metric}.`;
  return (
    <div className="env-driven-cell" title={detail}>
      <LabelToggle
        checked={rule.enabled}
        label={`status ${rule.metric}`}
        aria-disabled
        onChange={() => undefined}
      />
      {isAdmin && rule.profileId ? (
        <div className="env-driven-actions">
          <button
            type="button"
            className="btn-ghost btn-small"
            onClick={onChangeConfig}
          >
            Change configuration
          </button>
          <button
            type="button"
            className="btn-ghost btn-small"
            disabled={busy || !rule.enabled}
            onClick={onDisable}
          >
            Disable evaluation
          </button>
        </div>
      ) : null}
      {isAdmin && !rule.profileId ? (
        <span className="hint-tag">seed not present</span>
      ) : null}
    </div>
  );
}

function EvalRules({
  rules,
  isAdmin,
  profileIdByKind,
  onRequestOpenProfile,
  onDisableProfile,
  disableBusy,
}: {
  rules: EvalRule[];
  isAdmin: boolean;
  profileIdByKind: Record<string, string | undefined>;
  onRequestOpenProfile: (id: string) => void;
  onDisableProfile: (id: string) => void;
  disableBusy: boolean;
}) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState<string | null>(null);
  void isAdmin;
  const mut = useMutation({
    // Backend EvalConfigPatch accepts both nested {eval:{pii_enabled:..}}
    // and flat {pii_enabled:..} (the dual form makes the wire format
    // tolerant of older scripts). Use flat form here so the wire payload
    // matches the snake_case keys EvalConfigSnapshot returns.
    mutationFn: (p: Record<string, unknown>) => patchEvalConfig(p),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["eval-config"] }),
    onSettled: (_d, _e, payload) => {
      void payload;
      setBusy(null);
    },
  });

  // Every row lives in one panel now. PII / Completeness have a clickable
  // toggle; SLM judge / Remote eval are env-driven and rendered as a
  // locked-to-env badge with admin-only configurable actions.
  const interactiveMetrics = new Set(["PII", "Completeness"]);

  const cols: Column<EvalRule>[] = [
    {
      id: "metric",
      header: "Metric",
      cell: (r) => <strong>{r.metric}</strong>,
      sortValue: (r) => r.metric,
    },
    {
      id: "enabled",
      header: "Status",
      width: "320px",
      cell: (r) => {
        const interactive = interactiveMetrics.has(r.metric);
        if (interactive) {
          return (
            <LabelToggle
              checked={r.enabled}
              disabled={busy === r.metric || mut.isPending}
              onChange={(next) => {
                setBusy(r.metric);
                const key =
                  r.metric === "PII"
                    ? { pii_enabled: next }
                    : { completeness_enabled: next };
                mut.mutate(key);
              }}
              label={`toggle ${r.metric}`}
            />
          );
        }
        // Non-interactive row (SLM judge / Remote eval). Cells render a
        // "running / disabled" pill that's purely informational — never
        // clickable — alongside admin-only buttons to either open the
        // profile drawer for editing, or flip the profile's enabled
        // flag (which is the new hot-path "disable evaluation" switch).
        return (
          <EnvDrivenCell
            rule={r}
            isAdmin={isAdmin}
            busy={disableBusy}
            onChangeConfig={() => {
              const id =
                profileIdByKind[r.metric === "SLM judge" ? "slm_judge" : "remote_eval"];
              if (id) onRequestOpenProfile(id);
            }}
            onDisable={() => {
              const id =
                profileIdByKind[r.metric === "SLM judge" ? "slm_judge" : "remote_eval"];
              if (id) onDisableProfile(id);
            }}
          />
        );
      },
      sortValue: (r) => Number(!interactiveMetrics.has(r.metric)) * 10 + Number(r.enabled),
    },
    {
      id: "detail",
      header: "Detail",
      cell: (r) => <span className="muted small">{r.detail}</span>,
    },
  ];

  return (
    <>
      <section>
        <h2 className="section-title">Heuristics</h2>
        <div className="panel" style={{ padding: 4 }}>
          <DataTable
            rows={rules}
            columns={cols}
            emptyMessage="No metrics."
          />
        </div>
        <p className="muted small" style={{ marginTop: 10 }}>
          SLM judge and Remote eval are seeded from <code>NEXUS_EVAL_*</code> /
          <code> NEXUS_REMOTE_EVAL_*</code> and exposed through default
          profiles. Admins can change the wiring inside the profile
          editor or flip the profile&apos;s enabled flag to turn
          evaluation off — both apply without a Pod restart. Other
          surfaces (PII / Completeness, routing weights, sample rate)
          are unaffected.
        </p>
      </section>
    </>
  );
}

function WeightsCard({
  cfg,
  quality,
  cost,
  latency,
  onQuality,
  onCost,
  onLatency,
  onSaved,
}: {
  cfg: EvalConfigSnapshot;
  quality: number;
  cost: number;
  latency: number;
  onQuality: (v: number) => void;
  onCost: (v: number) => void;
  onLatency: (v: number) => void;
  onSaved: (next: { quality: number; cost: number; latency: number }) => void;
}) {
  const qc = useQueryClient();
  // State (quality / cost / latency) lives in the parent so the upper stats
  // bar and the slider card stay in sync after Save re-normalises the row.

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
    // Push the normalised row up so the upper stats bar re-paints to the
    // post-save values as well, not just the slider thumbs.
    onSaved(after);
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
          onChange={(v) => onQuality(clamp(v))}
        />
        <WeightSlider
          label="Cost"
          tone="info"
          value={cost}
          onChange={(v) => onCost(clamp(v))}
        />
        <WeightSlider
          label="Latency"
          tone="warn"
          value={latency}
          onChange={(v) => onLatency(clamp(v))}
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
