import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type EvalPluginRecord,
  createEvalPlugin,
  deleteEvalPlugin,
  fetchEvalPlugins,
  patchEvalPlugin,
} from "../api";
import { LabelToggle } from "../components/LabelToggle";

// Reasonable boilerplate so a fresh user can copy-paste. The default
// `mode: poll` is intentionally conservative: the webhook endpoint
// requires an admin to set a signed token first; poll works out of
// the box against any service that exposes /v1/feedback or
// /api/public/scores.
const LANGFUSE_TEMPLATE = `apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: langfuse-judge
spec:
  service:
    type: langfuse
    endpoint: https://cloud.langfuse.com
    auth:
      secretRef: langfuse-public-key
  send:
    trigger: on_trace
    sampling: "0.1"
    payload:
      input: "{{ .trace.input_messages }}"
      output: "{{ .trace.output_messages }}"
    redact: [pii]
  collect:
    mode: poll
    interval: 60s
    mapping:
      name: "$.name"
      score: "$.value"
      explanation: "$.comment"
      trace_id: "$.traceId"
  timeout: 30s
`;

export function EvalPlugins() {
  const qc = useQueryClient();
  const query = useQuery({
    queryKey: ["eval-plugins"],
    queryFn: fetchEvalPlugins,
  });
  const createM = useMutation({
    mutationFn: (body: Omit<EvalPluginRecord, "id">) => createEvalPlugin(body),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["eval-plugins"] }),
  });
  const patchM = useMutation({
    mutationFn: ({ id, patch }: { id: string; patch: { spec_yaml?: string; enabled?: boolean } }) =>
      patchEvalPlugin(id, patch),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["eval-plugins"] }),
  });
  const deleteM = useMutation({
    mutationFn: (id: string) => deleteEvalPlugin(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["eval-plugins"] }),
  });

  const [draft, setDraft] = useState<string>(LANGFUSE_TEMPLATE);
  const [draftName, setDraftName] = useState<string>("langfuse-judge");

  const onCreate = () => {
    const rec: Omit<EvalPluginRecord, "id"> = {
      name: draftName.trim(),
      spec_yaml: draft,
      enabled: true,
    };
    createM.mutate(rec);
  };

  const onToggle = (rec: EvalPluginRecord) => {
    patchM.mutate({
      id: rec.id!,
      patch: { enabled: !rec.enabled },
    });
  };

  const onDelete = (rec: EvalPluginRecord) => {
    if (!window.confirm(`Delete plugin ${rec.name}? This cannot be undone.`)) {
      return;
    }
    deleteM.mutate(rec.id!);
  };

  const lastError =
    (createM.error as Error | null) ||
    (patchM.error as Error | null) ||
    (deleteM.error as Error | null);

  return (
    <div className="page eval-plugins-page">
      <header className="page-head">
        <div>
          <h1 className="page-title">Eval plugins</h1>
          <p className="page-sub">
            Connect Nexus to external evaluation services (LangSmith,
            Langfuse, Datadog, Braintrust, Arize, OTel collector, or any
            HTTPS endpoint). Plugins are config-only — Nexus never runs
            evals in-process for these targets.
          </p>
        </div>
      </header>

      <section className="tier-card">
        <h2 className="tier-card-title">Installed plugins</h2>
        {query.isLoading && <div className="muted small">Loading…</div>}
        {query.error && (
          <div className="error small">Failed to load plugins.</div>
        )}
        {!query.isLoading && (query.data?.length ?? 0) === 0 && (
          <div className="muted small">
            No plugins installed yet. Pick a template below or write
            your own YAML.
          </div>
        )}
        {query.data && query.data.length > 0 && (
          <table className="data-table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Service</th>
                <th>Sampling</th>
                <th>Collect mode</th>
                <th>Enabled</th>
                <th>Source</th>
                <th aria-label="actions" />
              </tr>
            </thead>
            <tbody>
              {query.data.map((p) => (
                <PluginRow
                  key={p.id}
                  rec={p}
                  onToggle={() => onToggle(p)}
                  onDelete={() => onDelete(p)}
                />
              ))}
            </tbody>
          </table>
        )}
        {lastError && (
          <div className="error small">{lastError.message}</div>
        )}
      </section>

      <section className="tier-card">
        <h2 className="tier-card-title">Install a new plugin</h2>
        <p className="muted small">
          Paste an <code>EvalPlugin</code> manifest. The auth block
          must point at a Kubernetes Secret or in-cluster key — inline
          secrets are rejected so they never end up in source control.
        </p>
        <div className="field-row">
          <label>
            Name
            <input
              type="text"
              value={draftName}
              onChange={(e) => setDraftName(e.target.value)}
            />
          </label>
        </div>
        <textarea
          className="yaml-textarea"
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          rows={22}
          spellCheck={false}
        />
        <div className="row gap-sm">
          <button
            className="btn btn-neon"
            disabled={createM.isPending}
            onClick={onCreate}
          >
            Install plugin
          </button>
          <button
            className="btn btn-ghost"
            onClick={() => setDraft(LANGFUSE_TEMPLATE)}
          >
            Reset to Langfuse template
          </button>
        </div>
      </section>
    </div>
  );
}

function PluginRow({
  rec,
  onToggle,
  onDelete,
}: {
  rec: EvalPluginRecord;
  onToggle: () => void;
  onDelete: () => void;
}) {
  const parsed = safeParse(rec.spec_yaml);
  return (
    <tr>
      <td>
        <strong>{rec.name}</strong>
        {rec.org_id && <div className="muted xs">org: {rec.org_id}</div>}
      </td>
      <td>{parsed.type ?? "—"}</td>
      <td>{parsed.sampling ?? "—"}</td>
      <td>{parsed.mode ?? "—"}</td>
      <td>
        <LabelToggle
          label={`Enable ${rec.name}`}
          checked={rec.enabled}
          onChange={onToggle}
        />
      </td>
      <td>
        <span className="muted small">{rec.org_id ? "DB" : "Helm"}</span>
      </td>
      <td>
        <button className="btn btn-ghost" onClick={onDelete}>
          Delete
        </button>
      </td>
    </tr>
  );
}

// safeParse reads a few prominent fields from the YAML without
// pulling in a full YAML parser; it's a UI affordance, not an
// authority on what's actually deployed (the server re-validates on
// every create/patch).
function safeParse(raw: string): {
  type?: string;
  sampling?: string;
  mode?: string;
} {
  const out: { type?: string; sampling?: string; mode?: string } = {};
  for (const line of raw.split("\n")) {
    const m = line.match(/^\s*type:\s*(.+)\s*$/);
    if (m && out.type === undefined) out.type = m[1];
    const s = line.match(/^\s*sampling:\s*(.+)\s*$/);
    if (s && out.sampling === undefined) out.sampling = s[1];
    const c = line.match(/^\s*mode:\s*(.+)\s*$/);
    if (c && out.mode === undefined) out.mode = c[1];
  }
  return out;
}
