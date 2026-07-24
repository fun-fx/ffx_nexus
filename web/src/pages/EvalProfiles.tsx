import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  createEvalProfile,
  deleteEvalProfile,
  fetchEvalProfiles,
  patchEvalProfile,
  type EvalProfile,
  type EvalProfilePatch,
  type EvalScope,
  type KeySource,
  type ProfileKind,
} from "../api";
import { Chip } from "../components/Chip";
import { StatusPill } from "../components/StatusPill";
import { Drawer } from "../components/Drawer";

/**
 * PR #137 — per-eval profile CRUD UI.
 *
 * Renders the existing EvalProfile rows as cards inside Eval.tsx with
 * per-card actions (toggle / edit / delete). Admin can also create
 * new profiles through a drawer. The drawer gates key_source by
 * profile kind (heuristics → builtin only) so the validation logic
 * on the server can't be bypassed through a stray request.
 */

interface DraftProfile {
  name: string;
  kind: ProfileKind;
  scope: EvalScope;
  owner_user_id: string;
  endpoint_base: string;
  endpoint_model: string;
  key_source: KeySource;
  key_ref: string;
  metricsRaw: string;
  threshold: number;
  sample_rate: number;
  enabled: boolean;
}

const KIND_PRESETS: { id: ProfileKind; label: string; description: string }[] = [
  {
    id: "heuristic_pii",
    label: "PII heuristic",
    description: "Detects emails, phone numbers, identifiers. No external call.",
  },
  {
    id: "heuristic_completeness",
    label: "Completeness heuristic",
    description: "Flags truncated responses. No external call.",
  },
  {
    id: "slm_judge",
    label: "SLM judge",
    description: "Score with a local SLM (vLLM / Ollama).",
  },
  {
    id: "remote_eval",
    label: "Remote sidecar",
    description: "DeepEval / RAGAS via the Python sidecar.",
  },
];

const KEY_SOURCE_PRESETS: { id: KeySource; label: string; description: string }[] = [
  { id: "org", label: "Org keys", description: "Operator-managed provider_credentials (user_id NULL)." },
  { id: "user", label: "User keys", description: "BYOK — bound to the caller's user id." },
  { id: "inline", label: "Inline", description: "Stored encrypted in eval_credentials; key_ref is the surrogate token." },
  { id: "builtin", label: "Builtin", description: "Heuristic-only profiles; no external call required." },
];

function emptyDraft(): DraftProfile {
  return {
    name: "",
    kind: "slm_judge",
    scope: "org",
    owner_user_id: "",
    endpoint_base: "",
    endpoint_model: "",
    key_source: "org",
    key_ref: "",
    metricsRaw: "answer_relevancy,toxicity,bias",
    threshold: 0.5,
    sample_rate: 1.0,
    enabled: true,
  };
}

function fromProfile(p: EvalProfile): DraftProfile {
  return {
    name: p.name ?? "",
    kind: (p.kind as ProfileKind) ?? "slm_judge",
    scope: (p.scope as EvalScope) ?? "org",
    owner_user_id: p.owner_user_id ?? "",
    endpoint_base: p.endpoint?.base_url ?? "",
    endpoint_model: p.endpoint?.model ?? "",
    key_source: (p.endpoint?.key_source as KeySource) ?? "org",
    key_ref: p.endpoint?.key_ref ?? "",
    metricsRaw: (p.metrics ?? []).join(","),
    threshold: typeof p.threshold === "number" ? p.threshold : 0.5,
    sample_rate: typeof p.sample_rate === "number" ? p.sample_rate : 1.0,
    enabled: p.enabled ?? true,
  };
}

function toPayload(d: DraftProfile): EvalProfile {
  const metrics = d.metricsRaw
    .split(",")
    .map((m) => m.trim())
    .filter((m) => m.length > 0);
  return {
    name: d.name.trim(),
    kind: d.kind,
    scope: d.scope,
    owner_user_id: d.scope === "user" ? d.owner_user_id.trim() : "",
    endpoint: {
      base_url: d.endpoint_base.trim(),
      model: d.endpoint_model.trim(),
      key_source: d.key_source,
      key_ref: d.key_ref.trim(),
    },
    metrics,
    threshold: Number.isFinite(d.threshold) ? d.threshold : 0,
    sample_rate: clamp01(d.sample_rate),
    enabled: d.enabled,
  };
}

function toPatch(d: DraftProfile): EvalProfilePatch {
  return {
    name: d.name.trim(),
    kind: d.kind,
    scope: d.scope,
    owner_user_id: d.scope === "user" ? d.owner_user_id.trim() : "",
    endpoint: {
      base_url: d.endpoint_base.trim(),
      model: d.endpoint_model.trim(),
      key_source: d.key_source,
      key_ref: d.key_ref.trim(),
    },
    metrics: d.metricsRaw
      .split(",")
      .map((m) => m.trim())
      .filter((m) => m.length > 0),
    threshold: Number.isFinite(d.threshold) ? d.threshold : 0,
    sample_rate: clamp01(d.sample_rate),
    enabled: d.enabled,
  };
}

function clamp01(v: number): number {
  if (!Number.isFinite(v)) return 0;
  if (v < 0) return 0;
  if (v > 1) return 1;
  return v;
}

function kindLabel(k: ProfileKind): string {
  return KIND_PRESETS.find((p) => p.id === k)?.label ?? k;
}

function keySourceLabel(k: KeySource): string {
  return KEY_SOURCE_PRESETS.find((p) => p.id === k)?.label ?? k;
}

export function EvalProfilesCard({ isAdmin }: { isAdmin: boolean }) {
  const qc = useQueryClient();
  const profiles = useQuery({
    queryKey: ["eval-profiles"],
    queryFn: fetchEvalProfiles,
    refetchInterval: 30_000,
  });

  const [drawerOpen, setDrawerOpen] = useState(false);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [draft, setDraft] = useState<DraftProfile>(emptyDraft());
  const [error, setError] = useState<string | null>(null);

  // Heuristic kinds ignore non-builtin key sources — match the
  // server-side validation in profiles.go so the client never sends
  // an obviously-bad payload (the server still re-checks).
  const isHeuristic =
    draft.kind === "heuristic_pii" || draft.kind === "heuristic_completeness";
  useEffect(() => {
    if (isHeuristic && draft.key_source !== "builtin") {
      setDraft((d) => ({ ...d, key_source: "builtin" }));
    } else if (!isHeuristic && draft.key_source === "builtin") {
      setDraft((d) => ({ ...d, key_source: "org" }));
    }
  }, [isHeuristic, draft.key_source]);

  const createM = useMutation({
    mutationFn: createEvalProfile,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["eval-profiles"] });
      closeDrawer();
    },
    onError: (e: Error) => setError(e.message),
  });

  const patchM = useMutation({
    mutationFn: ({ id, patch }: { id: string; patch: EvalProfilePatch }) =>
      patchEvalProfile(id, patch),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["eval-profiles"] });
      closeDrawer();
    },
    onError: (e: Error) => setError(e.message),
  });

  const deleteM = useMutation({
    mutationFn: ({ id }: { id: string }) => deleteEvalProfile(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["eval-profiles"] }),
    onError: (e: Error) => setError(e.message),
  });

  const toggleM = useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) =>
      patchEvalProfile(id, { enabled }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["eval-profiles"] }),
  });

  function openCreate() {
    setEditingId(null);
    setDraft(emptyDraft());
    setError(null);
    setDrawerOpen(true);
  }

  function openEdit(p: EvalProfile) {
    if (!p.id) return;
    setEditingId(p.id);
    setDraft(fromProfile(p));
    setError(null);
    setDrawerOpen(true);
  }

  function closeDrawer() {
    setDrawerOpen(false);
    setEditingId(null);
    setError(null);
  }

  function submit() {
    const payload = toPayload(draft);
    if (!payload.name) {
      setError("name is required");
      return;
    }
    if (payload.scope === "user" && draft.scope === "user" && !draft.owner_user_id) {
      // server will reject too, but the client-side guard keeps the
      // UI consistent before the round-trip.
      setError("owner_user_id is required for user-scoped profiles");
      return;
    }
    setError(null);
    if (editingId) {
      patchM.mutate({ id: editingId, patch: toPatch(draft) });
    } else {
      createM.mutate(payload);
    }
  }

  const grouped = useMemo(() => groupProfiles(profiles.data ?? []), [profiles.data]);

  return (
    <section className="panel profiles-card" data-testid="eval-profiles">
      <header className="panel-head">
        <div>
          <h2 className="panel-title">Eval profiles</h2>
          <p className="muted small">
            Per-eval configuration. Each profile carries its own judge / sidecar endpoint,
            credential source, and sample rate.
          </p>
        </div>
        {isAdmin ? (
          <button
            type="button"
            className="btn-neon"
            onClick={openCreate}
            disabled={createM.isPending}
          >
            + New profile
          </button>
        ) : null}
      </header>
      <div className="profile-list" data-testid="profile-list">
        {profiles.isLoading ? (
          <p className="muted small">Loading profiles…</p>
        ) : (profiles.data?.length ?? 0) === 0 ? (
          <p className="muted">No profiles. Default env-var seeding runs on boot.</p>
        ) : (
          <>
            <Group title="Org profiles" rows={grouped.org} isAdmin={isAdmin} onEdit={openEdit} onToggle={(id, enabled) => toggleM.mutate({ id, enabled })} onDelete={(id) => deleteM.mutate({ id })} busyDeleteId={deleteM.isPending ? deleteM.variables?.id ?? null : null} />
            <Group title="My profiles" rows={grouped.user} isAdmin={isAdmin} onEdit={openEdit} onToggle={(id, enabled) => toggleM.mutate({ id, enabled })} onDelete={(id) => deleteM.mutate({ id })} busyDeleteId={deleteM.isPending ? deleteM.variables?.id ?? null : null} />
          </>
        )}
      </div>
      {error ? <p className="error small">{error}</p> : null}
      {drawerOpen ? (
        <ProfileDrawer
          draft={draft}
          setDraft={setDraft}
          onClose={closeDrawer}
          onSubmit={submit}
          busy={createM.isPending || patchM.isPending}
          editing={Boolean(editingId)}
          error={error}
        />
      ) : null}
    </section>
  );
}

function groupProfiles(rows: EvalProfile[]): { org: EvalProfile[]; user: EvalProfile[] } {
  const org: EvalProfile[] = [];
  const user: EvalProfile[] = [];
  for (const p of rows) {
    if (p.scope === "user") user.push(p);
    else org.push(p);
  }
  return { org, user };
}

interface GroupProps {
  title: string;
  rows: EvalProfile[];
  isAdmin: boolean;
  onEdit: (p: EvalProfile) => void;
  onToggle: (id: string, enabled: boolean) => void;
  onDelete: (id: string) => void;
  busyDeleteId: string | null;
}

function Group({ title, rows, isAdmin, onEdit, onToggle, onDelete, busyDeleteId }: GroupProps) {
  if (rows.length === 0) return null;
  return (
    <div className="profile-group">
      <h4 className="profile-group-title">{title}</h4>
      <div className="profile-row-list">
        {rows.map((p) => (
          <ProfileRow
            key={p.id ?? p.name}
            profile={p}
            isAdmin={isAdmin}
            onEdit={() => onEdit(p)}
            onToggle={(next) => p.id && onToggle(p.id, next)}
            onDelete={() => p.id && onDelete(p.id)}
            busyDelete={busyDeleteId === p.id}
          />
        ))}
      </div>
    </div>
  );
}

function ProfileRow({
  profile,
  isAdmin,
  onEdit,
  onToggle,
  onDelete,
  busyDelete,
}: {
  profile: EvalProfile;
  isAdmin: boolean;
  onEdit: () => void;
  onToggle: (next: boolean) => void;
  onDelete: () => void;
  busyDelete: boolean;
}) {
  const enabled = profile.enabled ?? false;
  const sample = typeof profile.sample_rate === "number" ? profile.sample_rate : 0;
  return (
    <article className="profile-row" data-testid={`profile-row-${profile.id}`}>
      <div className="profile-row-head">
        <div>
          <span className="profile-row-name">{profile.name}</span>{" "}
          <Chip tone="info">{kindLabel((profile.kind as ProfileKind) ?? "slm_judge")}</Chip>{" "}
          <Chip tone="neutral">{keySourceLabel((profile.endpoint?.key_source as KeySource) ?? "org")}</Chip>
        </div>
        <div className="profile-row-actions">
          <StatusPill label={enabled ? "on" : "off"} tone={enabled ? "ok" : "neutral"} />
          <button
            type="button"
            className="btn-ghost btn-small"
            aria-label={`toggle ${profile.name}`}
            onClick={() => onToggle(!enabled)}
          >
            {enabled ? "Disable" : "Enable"}
          </button>
          {isAdmin ? (
            <button type="button" className="btn-ghost btn-small" onClick={onEdit}>
              Edit
            </button>
          ) : null}
          {isAdmin && profile.id ? (
            <button
              type="button"
              className="btn-ghost btn-small row-action-danger"
              onClick={onDelete}
              disabled={busyDelete}
            >
              {busyDelete ? "Deleting…" : "Delete"}
            </button>
          ) : null}
        </div>
      </div>
      <dl className="profile-row-meta">
        <div>
          <dt>Sample rate</dt>
          <dd>{(sample * 100).toFixed(0)}%</dd>
        </div>
        <div>
          <dt>Endpoint</dt>
          <dd className="ellipsis">
            {profile.endpoint?.base_url || "—"}
            {profile.endpoint?.model ? ` · ${profile.endpoint.model}` : ""}
          </dd>
        </div>
        <div>
          <dt>Key ref</dt>
          <dd className="ellipsis">{profile.endpoint?.key_ref || "—"}</dd>
        </div>
        <div>
          <dt>Scope</dt>
          <dd>{profile.scope === "user" ? (profile.owner_user_id || "self") : "org"}</dd>
        </div>
      </dl>
      {(profile.metrics?.length ?? 0) > 0 ? (
        <div className="chip-row">
          {profile.metrics!.map((m) => (
            <Chip key={m} tone="neutral">
              {m}
            </Chip>
          ))}
        </div>
      ) : null}
    </article>
  );
}

function ProfileDrawer({
  draft,
  setDraft,
  onClose,
  onSubmit,
  busy,
  editing,
  error,
}: {
  draft: DraftProfile;
  setDraft: (next: DraftProfile) => void;
  onClose: () => void;
  onSubmit: () => void;
  busy: boolean;
  editing: boolean;
  error: string | null;
}) {
  return (
    <Drawer
      open
      title={editing ? "Edit profile" : "New profile"}
      onClose={onClose}
      testId="profile-drawer"
      footer={
        <>
          <button type="button" className="btn-ghost" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button
            type="button"
            className="btn-neon"
            onClick={onSubmit}
            disabled={busy}
            data-testid="profile-submit"
          >
            {busy ? "Saving…" : editing ? "Save changes" : "Create profile"}
          </button>
        </>
      }
    >
      <FieldRow label="Name">
        <input
          className="input"
          value={draft.name}
          onChange={(e) => setDraft({ ...draft, name: e.target.value })}
          placeholder="e.g. Claude safety judge"
          autoFocus
        />
      </FieldRow>
      <FieldRow label="Kind">
        <select
          className="input"
          value={draft.kind}
          onChange={(e) => setDraft({ ...draft, kind: e.target.value as ProfileKind })}
        >
          {KIND_PRESETS.map((p) => (
            <option key={p.id} value={p.id}>
              {p.label}
            </option>
          ))}
        </select>
        <p className="muted tiny">
          {KIND_PRESETS.find((p) => p.id === draft.kind)?.description}
        </p>
      </FieldRow>
      <FieldRow label="Scope">
        <select
          className="input"
          value={draft.scope}
          onChange={(e) => setDraft({ ...draft, scope: e.target.value as EvalScope })}
        >
          <option value="org">Org</option>
          <option value="user">User (BYOK)</option>
        </select>
        {draft.scope === "user" ? (
          <input
            className="input"
            placeholder="owner_user_id"
            value={draft.owner_user_id}
            onChange={(e) => setDraft({ ...draft, owner_user_id: e.target.value })}
          />
        ) : null}
      </FieldRow>
      <FieldRow label="Endpoint">
        <input
          className="input"
          placeholder="https://api.openai.com/v1"
          value={draft.endpoint_base}
          onChange={(e) => setDraft({ ...draft, endpoint_base: e.target.value })}
        />
        <input
          className="input"
          placeholder="model id"
          value={draft.endpoint_model}
          onChange={(e) => setDraft({ ...draft, endpoint_model: e.target.value })}
        />
      </FieldRow>
      <FieldRow label="Key source">
        <select
          className="input"
          value={draft.key_source}
          onChange={(e) => setDraft({ ...draft, key_source: e.target.value as KeySource })}
        >
          {KEY_SOURCE_PRESETS.map((p) => (
            <option key={p.id} value={p.id} disabled={isHeuristic(draft.kind) && p.id !== "builtin"}>
              {p.label}
            </option>
          ))}
        </select>
        <p className="muted tiny">
          {KEY_SOURCE_PRESETS.find((p) => p.id === draft.key_source)?.description}
        </p>
        {draft.key_source === "inline" ? (
          <input
            className="input"
            placeholder="key_ref (server-issued token)"
            value={draft.key_ref}
            onChange={(e) => setDraft({ ...draft, key_ref: e.target.value })}
          />
        ) : null}
      </FieldRow>
      <FieldRow label="Metrics (comma separated)">
        <input
          className="input"
          value={draft.metricsRaw}
          onChange={(e) => setDraft({ ...draft, metricsRaw: e.target.value })}
          placeholder="answer_relevancy,toxicity,bias"
        />
      </FieldRow>
      <FieldRow label="Threshold">
        <input
          type="number"
          className="input"
          step="0.05"
          min={0}
          max={1}
          value={draft.threshold}
          onChange={(e) => setDraft({ ...draft, threshold: Number(e.target.value) })}
        />
      </FieldRow>
      <FieldRow label="Sample rate">
        <input
          type="number"
          className="input"
          step="0.05"
          min={0}
          max={1}
          value={draft.sample_rate}
          onChange={(e) => setDraft({ ...draft, sample_rate: Number(e.target.value) })}
        />
      </FieldRow>
      <FieldRow label="Enabled">
        <label className="checkbox">
          <input
            type="checkbox"
            checked={draft.enabled}
            onChange={(e) => setDraft({ ...draft, enabled: e.target.checked })}
          />
          <span>On</span>
        </label>
      </FieldRow>
      {error ? <p className="error small">{error}</p> : null}
    </Drawer>
  );
}

function FieldRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="field-row">
      <span className="field-label">{label}</span>
      {children}
    </label>
  );
}

function isHeuristic(k: ProfileKind): boolean {
  return k === "heuristic_pii" || k === "heuristic_completeness";
}
