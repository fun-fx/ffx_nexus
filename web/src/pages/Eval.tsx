import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  fetchEvalConfig,
  fetchEvalProfiles,
  fetchEvalPlugins,
  patchEvalConfig,
  patchEvalProfile,
  patchEvalPlugin,
  deleteEvalPlugin,
  testEvalPlugin,
  type EvalConfigSnapshot,
  type EvalPluginRecord,
} from "../api";
import { DataTable, type Column } from "../components/DataTable";
import { Chip } from "../components/Chip";
import { GradientText } from "../components/GradientText";
import { Icon } from "../components/icons";
import { LabelToggle } from "../components/LabelToggle";
import { fetchMe, type EvalProfile } from "../api";
import { EvalProfilesCard } from "./EvalProfiles";
import {
  PluginEditorDrawer,
  type PluginFormState,
} from "./EvalPlugins";
import { parseYamlToForm } from "../lib/pluginManifest";

type EvalMetric = "heuristic_pii" | "heuristic_completeness" | "slm_judge" | "remote_eval";

interface EvalRule {
  metric: string;
  kind: EvalMetric;
  enabled: boolean;
  detail: string;
  /**
   * Optional link from a top-table row down to the underlying
   * EvalProfile. The four core kinds each resolve to their default
   * profile id (default-pii, default-completeness, default-judge,
   * default-remote) so a uniform Enable / Disable switch routes the
   * mutation straight to the right profile row.
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

  // Stable id resolution: each of the four core kinds has a default-*
  // profile seeded from env at boot. The top table button is wired to
  // whichever default matches, falling back to any org-scope profile of
  // the requested kind if the default has been deleted.
  //
  // `profileIdByKind` is keyed by the ProfileKind union (matches what
  // /api/eval/profiles returns) so a single `evalRule.kind` lookup
  // resolves straight to a profile row id.
  const profileIdByKind = useMemo(() => {
    const out: Record<string, string | undefined> = {
      heuristic_pii: undefined,
      heuristic_completeness: undefined,
      slm_judge: undefined,
      remote_eval: undefined,
    };
    const list = profilesQ.data ?? [];
    const wantDefault: Record<string, string> = {
      heuristic_pii: "default-pii",
      heuristic_completeness: "default-completeness",
      slm_judge: "default-judge",
      remote_eval: "default-remote",
    };
    for (const kind of Object.keys(wantDefault)) {
      const matches = list.filter((p) => p.kind === kind);
      out[kind] =
        matches.find((p) => p.id === wantDefault[kind])?.id ??
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

  // Plugin query + drawer state — the merged Evaluators card lists
  // plugin rows next to heuristic rows and routes the editor through
  // the same drawer the standalone /eval/plugins page uses.
  const pluginsQ = useQuery({
    queryKey: ["eval-plugins"],
    queryFn: fetchEvalPlugins,
    refetchInterval: 30_000,
  });
  const [pluginDrawer, setPluginDrawer] = useState<
    { open: boolean; initial?: PluginFormState }
  >({ open: false, initial: undefined });

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
      kind: "heuristic_pii",
      enabled: cfg.eval.pii_enabled,
      detail: "Detects requests / responses that contain emails, phone numbers, etc.",
      profileId: profileIdByKind["heuristic_pii"] ?? null,
    },
    {
      metric: "Completeness",
      kind: "heuristic_completeness",
      enabled: cfg.eval.completeness_enabled,
      detail: "Trims runaway responses and flags truncated outputs.",
      profileId: profileIdByKind["heuristic_completeness"] ?? null,
    },
    {
      metric: "SLM judge",
      kind: "slm_judge",
      enabled: cfg.eval.judge.enabled,
      detail: `${cfg.eval.judge.model} @ ${cfg.eval.judge.base_url}`,
      profileId: profileIdByKind["slm_judge"] ?? null,
    },
    {
      metric: "Remote eval",
      kind: "remote_eval",
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

      <LegacyDeprecationBanner profiles={profilesQ.data ?? []} />

      <EvaluatorsCard
        rules={heur}
        isAdmin={isAdmin}
        profileIdByKind={profileIdByKind}
        plugins={pluginsQ.data ?? []}
        onRequestOpenProfile={setPendingOpenProfileId}
        onToggleProfile={(id, enabled) =>
          profileToggleMut.mutate({ id, enabled })
        }
        profileToggleBusy={profileToggleMut.isPending}
        profileToggleRow={profileToggleMut.variables?.id ?? null}
        onCreatePlugin={() => setPluginDrawer({ open: true, initial: undefined })}
        onEditPlugin={(rec) =>
          setPluginDrawer({ open: true, initial: parseYamlToForm(rec.spec_yaml ?? "") })
        }
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

      <PluginEditorDrawer
        open={pluginDrawer.open}
        initial={pluginDrawer.initial}
        onClose={() => setPluginDrawer({ open: false, initial: undefined })}
        onSaved={() => {
          setPluginDrawer({ open: false, initial: undefined });
        }}
      />
    </div>
  );
}

function EvaluatorsCard({
  rules,
  isAdmin,
  profileIdByKind,
  plugins,
  onRequestOpenProfile,
  onToggleProfile,
  profileToggleBusy,
  profileToggleRow,
  onCreatePlugin,
  onEditPlugin,
}: {
  rules: EvalRule[];
  isAdmin: boolean;
  profileIdByKind: Record<string, string | undefined>;
  plugins: EvalPluginRecord[];
  onRequestOpenProfile: (id: string) => void;
  onToggleProfile: (id: string, enabled: boolean) => void;
  profileToggleBusy: boolean;
  profileToggleRow: string | null;
  onCreatePlugin: () => void;
  onEditPlugin: (rec: EvalPluginRecord) => void;
}) {
  const qc = useQueryClient();
  const pluginToggleM = useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) =>
      patchEvalPlugin(id, { enabled }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["eval-plugins"] }),
  });
  const pluginDeleteM = useMutation({
    mutationFn: (id: string) => deleteEvalPlugin(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["eval-plugins"] }),
  });
  const testM = useMutation({
    mutationFn: (ref: string) => testEvalPlugin(ref),
  });

  // One row per evaluator instance: built-in heuristics, legacy
  // (SLM judge / Remote eval), and per-org plugins. Rendered as a single
  // table so an operator sees the *whole* surface in one glance — no
  // round-trip between pages when adding/removing a plugin.
  type EvalRow =
    | {
        kind: "heuristic";
        id: string;
        title: string;
        enabled: boolean;
        source: "heuristic";
        detail: React.ReactNode;
        action?: React.ReactNode;
      }
    | {
        kind: "legacy";
        id: string;
        title: string;
        enabled: boolean;
        source: "legacy";
        detail: React.ReactNode;
        action?: React.ReactNode;
      }
    | {
        kind: "plugin";
        id: string;
        title: string;
        enabled: boolean;
        source: "plugin";
        detail: React.ReactNode;
        action?: React.ReactNode;
        rec: EvalPluginRecord;
      };

  const rows: EvalRow[] = [];
  for (const r of rules) {
    const isLegacy = r.kind === "slm_judge" || r.kind === "remote_eval";
    const profileId = profileIdByKind[r.kind];
    const detail = (
      <div className="muted small">
        <span>{r.detail}</span>{" "}
        {isLegacy ? (
          <span className="hint-tag warn">legacy — migrate to plugin</span>
        ) : null}
      </div>
    );
    const action = isAdmin ? (
      <div className="env-driven-cell">
        <LabelToggle
          checked={r.enabled}
          label={`toggle ${r.metric}`}
          disabled={profileToggleBusy && profileToggleRow === profileId}
          onChange={(next) => profileId && onToggleProfile(profileId, next)}
        />
        {profileId ? (
          <button
            type="button"
            className="btn-ghost btn-small"
            onClick={() => profileId && onRequestOpenProfile(profileId)}
          >
            Change configuration
          </button>
        ) : (
          <span className="hint-tag">default profile missing</span>
        )}
      </div>
    ) : null;
    if (isLegacy) {
      rows.push({
        kind: "legacy",
        id: r.kind,
        title: `${r.metric} (legacy)`,
        enabled: r.enabled,
        source: "legacy",
        detail,
        action,
      });
    } else {
      rows.push({
        kind: "heuristic",
        id: r.kind,
        title: r.metric,
        enabled: r.enabled,
        source: "heuristic",
        detail,
        action,
      });
    }
  }
  for (const p of plugins) {
    const parsed = safeParsePlugin(p.spec_yaml ?? "");
    const isRowTesting =
      !!testM.isPending && testM.variables === p.name;
    const isRowToggling =
      !!pluginToggleM.isPending && pluginToggleM.variables?.id === p.id;
    rows.push({
      kind: "plugin",
      id: `plugin:${p.id ?? p.name}`,
      title: p.name,
      enabled: p.enabled,
      source: "plugin",
      rec: p,
      detail: (() => {
        const lastTest =
          !isRowTesting && testM.variables === p.name ? testM.data : undefined;
        const lastTestErr =
          !isRowTesting && testM.variables === p.name && testM.error
            ? (testM.error as Error).message
            : undefined;
        return (
          <div className="muted small">
            <Chip tone="info">{parsed.type ?? "—"}</Chip>{" "}
            <Chip tone="neutral">sample {parsed.sampling ?? "—"}</Chip>{" "}
            <Chip tone="neutral">{parsed.mode ?? "—"}</Chip>
            {lastTest ? (
              <span
                className={`plugin-row-test ${lastTest.ok ? "ok" : "err"}`}
                data-testid={`plugin-test-${p.name}`}
              >
                {" "}
                {lastTest.ok ? "✓" : "✗"} {lastTest.message}
                {typeof lastTest.latency_ms === "number"
                  ? ` (${lastTest.latency_ms}ms)`
                  : ""}
              </span>
            ) : null}
            {lastTestErr ? (
              <span className="plugin-row-test err" data-testid={`plugin-test-${p.name}`}>
                {" "}✗ {lastTestErr}
              </span>
            ) : null}
          </div>
        );
      })(),
      action: isAdmin ? (
        <div className="env-driven-cell">
          <LabelToggle
            checked={p.enabled}
            label={`toggle ${p.name}`}
            disabled={isRowToggling}
            onChange={() => {
              if (!p.id) return;
              pluginToggleM.mutate({ id: p.id, enabled: !p.enabled });
            }}
          />
          <button
            type="button"
            className="btn-ghost btn-small"
            onClick={() => testM.mutate(p.name)}
            disabled={isRowTesting}
          >
            {isRowTesting ? "Testing…" : "Test"}
          </button>
          <button
            type="button"
            className="btn-ghost btn-small"
            onClick={() => onEditPlugin(p)}
          >
            Edit
          </button>
          {p.id ? (
            <button
              type="button"
              className="btn-ghost btn-small danger"
              data-testid={`plugin-delete-${p.name}`}
              disabled={
                !!pluginDeleteM.isPending && pluginDeleteM.variables === p.id
              }
              onClick={() => {
                if (
                  !window.confirm(
                    `Delete the plugin "${p.name}"? Traces will no longer be sent to it.`,
                  )
                )
                  return;
                if (p.id) pluginDeleteM.mutate(p.id);
              }}
            >
              {!!pluginDeleteM.isPending && pluginDeleteM.variables === p.id
                ? "Deleting…"
                : "Delete"}
            </button>
          ) : null}
        </div>
      ) : null,
    });
  }

  const cols: Column<EvalRow>[] = [
    {
      id: "title",
      header: "Evaluator",
      cell: (r) => <strong>{r.title}</strong>,
      sortValue: (r) => r.title,
    },
    {
      id: "source",
      header: "Source",
      width: "120px",
      cell: (r) =>
        r.source === "heuristic" ? (
          <Chip tone="info">heuristic</Chip>
        ) : r.source === "legacy" ? (
          <Chip tone="warn">legacy</Chip>
        ) : (
          <Chip tone="accent">plugin</Chip>
        ),
      sortValue: (r) =>
        r.source === "heuristic" ? 0 : r.source === "legacy" ? 1 : 2,
    },
    {
      id: "detail",
      header: "Detail",
      cell: (r) => r.detail,
    },
    {
      id: "action",
      header: "Action",
      width: "320px",
      cell: (r) => r.action ?? null,
    },
  ];

  return (
    <section>
      <header className="section-head">
        <div>
          <h2 className="section-title">Evaluators</h2>
          <p className="muted small">
            Heuristics run in-process;{" "}
            <strong>plugins</strong> forward traces to Langfuse,
            LangSmith, Datadog, etc. and pull scores back.{" "}
            <strong>legacy</strong> rows are kept on for transparency —
            migrate them to plugins when convenient.
          </p>
        </div>
        {isAdmin ? (
          <button type="button" className="btn-neon" onClick={onCreatePlugin}>
            + Install plugin
          </button>
        ) : null}
      </header>
      <div className="panel" style={{ padding: 4 }}>
        <DataTable
          rows={rows}
          columns={cols}
          emptyMessage="No evaluators."
        />
      </div>
    </section>
  );
}

// Cheap visual extract for the EvaluatorsCard's plugin rows. The same
// logic exists in EvalPlugins.tsx — duplicated here on purpose to avoid
// pulling the list card up into the parent and forcing a heavier
// refactor. Co-locating the extractor next to the table that uses it
// keeps the boundary obvious.
function safeParsePlugin(raw: string): {
  type?: string;
  sampling?: string;
  mode?: string;
} {
  const out: { type?: string; sampling?: string; mode?: string } = {};
  for (const line of raw.split("\n")) {
    const m = line.match(/^\s*type:\s*(.+)\s*$/);
    if (m && out.type === undefined) out.type = m[1].trim();
    const s = line.match(/^\s*sampling:\s*(.+)\s*$/);
    if (s && out.sampling === undefined) out.sampling = s[1].trim();
    const c = line.match(/^\s*mode:\s*(.+)\s*$/);
    if (c && out.mode === undefined) out.mode = c[1].trim();
  }
  return out;
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

// LegacyDeprecationBanner renders a one-time notice whenever an org
// has a `slm_judge` or `remote_eval` profile still enabled. The
// Plan swaps them out for EvalPlugins hosted on LangSmith /
// Langfuse, etc.; existing tenants continue to work but can
// migrate whenever convenient.
function LegacyDeprecationBanner({ profiles }: { profiles: EvalProfile[] }) {
  const legacy = profiles.filter(
    (p) => (p.kind === "slm_judge" || p.kind === "remote_eval") && p.enabled,
  );
  if (legacy.length === 0) return null;
  return (
    <div className="tier-card" role="status">
      <h2 className="tier-card-title">Legacy evaluator still enabled</h2>
      <p className="tier-card-desc">
        {legacy.length} evaluator{legacy.length === 1 ? "" : "s"} of
        type <code>slm_judge</code> or <code>remote_eval</code> are
        running. We recommend migrating to{" "}
        <a href="/eval/plugins">Eval plugins</a> (LangSmith, Langfuse,
        Datadog, …) — same functionality, no in-cluster Ollama or
        Python sidecar required.
      </p>
    </div>
  );
}
