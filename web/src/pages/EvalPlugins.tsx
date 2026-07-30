import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type EvalPluginRecord,
  createEvalPlugin,
  deleteEvalPlugin,
  fetchEvalPlugins,
  patchEvalPlugin,
  pingEvalPluginWebhook,
  testEvalPlugin,
  type PluginTestResult,
} from "../api";
import { LabelToggle } from "../components/LabelToggle";
import { Drawer } from "../components/Drawer";
import { Chip } from "../components/Chip";
import { Icon } from "../components/icons";
import { PluginKeysModal } from "../components/PluginKeysModal";
import {
  DEFAULT_PLUGIN_TEMPLATE,
  PLUGIN_PRESETS,
  pctToFraction,
  parseYamlToForm,
  rid,
  roundFraction,
  serializeFormToYaml,
  type PluginFormState,
  type ServiceKind,
  type Trigger,
  type CollectMode,
} from "../lib/pluginManifest";

// Re-export so callers (notably Eval.tsx) can talk about the form
// shape without taking a direct dependency on the manifest module.
export type { PluginFormState };

/**
 * Form-mode plugin editor.
 *
 * The shape the operator sees is a *typed* form (select dropdowns,
 * sliders, key/value pairs). Underneath it is the canonical YAML
 * manifest, regenerated on every keystroke. The user can opt into
 * "Edit as YAML" mode to hand-edit the document directly; pulling back
 * into "Form" mode re-hydrates the typed tree (best-effort — unknown
 * fields stay in the YAML view).
 *
 * The same form is consumed by the `/eval` page's merged `Evaluators`
 * card (Phase H), so all editing logic lives here rather than in
 * `EvalPlugins.tsx`. This file still owns the legacy list rendered at
 * `/eval/plugins` for backwards compatibility — but the *editing*
 * surface is the shared form.
 */
export function PluginForm({
  initial,
  onSaved,
  onCancel,
  busy,
  error,
  submitLabel,
}: {
  initial?: PluginFormState;
  onSaved: (rec: EvalPluginRecord, form: PluginFormState) => void;
  onCancel?: () => void;
  busy?: boolean;
  error?: string | null;
  submitLabel: string;
}) {
  const [form, setForm] = useState<PluginFormState>(initial ?? DEFAULT_PLUGIN_TEMPLATE);
  const [mode, setMode] = useState<"form" | "yaml">("form");
  const [yamlDraft, setYamlDraft] = useState<string>(serializeFormToYaml(initial ?? DEFAULT_PLUGIN_TEMPLATE));
  const [yamlError, setYamlError] = useState<string | null>(null);

  // Live YAML preview — rendered whenever any form field changes so the
  // operator sees the wire format they're about to submit.
  const yamlFromForm = useMemo(() => serializeFormToYaml(form), [form]);
  void yamlFromForm; // exposed below via serializeFormToYaml(form) in <pre>

  function switchToForm() {
    try {
      const parsed = parseYamlToForm(yamlDraft);
      setForm(parsed);
      setYamlError(null);
      setMode("form");
    } catch (e) {
      setYamlError((e as Error).message);
    }
  }

  // Field updaters — small family so individual input handlers stay short.
  const update = (patch: Partial<PluginFormState>) =>
    setForm((f) => ({ ...f, ...patch }));
  const updateService = (patch: Partial<PluginFormState["service"]>) =>
    setForm((f) => ({ ...f, service: { ...f.service, ...patch } }));
  const updateSend = (patch: Partial<PluginFormState["send"]>) =>
    setForm((f) => ({ ...f, send: { ...f.send, ...patch } }));
  const updateCollect = (patch: Partial<PluginFormState["collect"]>) =>
    setForm((f) => ({ ...f, collect: { ...f.collect, ...patch } }));

  const submit = () => onSaved({ ...form } as unknown as EvalPluginRecord, form);

  return (
    <div className="plugin-form">
      <div className="plugin-form-mode">
        <button
          type="button"
          className={`btn-ghost btn-small ${mode === "form" ? "is-active" : ""}`}
          onClick={() => (mode === "yaml" ? switchToForm() : setMode("form"))}
          disabled={mode === "form"}
        >
          Form
        </button>
        <button
          type="button"
          className={`btn-ghost btn-small ${mode === "yaml" ? "is-active" : ""}`}
          onClick={() => {
            setYamlDraft(serializeFormToYaml(form));
            setMode("yaml");
          }}
        >
          Edit as YAML
        </button>
      </div>

      {mode === "form" ? (
        <FormMode
          form={form}
          update={update}
          updateService={updateService}
          updateSend={updateSend}
          updateCollect={updateCollect}
        />
      ) : (
        <YamlMode
          yamlDraft={yamlDraft}
          setYamlDraft={setYamlDraft}
          yamlError={yamlError}
          onCommit={switchToForm}
        />
      )}

      <div className="plugin-form-preview">
        <header className="preview-head">
          <h4>YAML preview</h4>
          <span className="muted small">What the backend will receive.</span>
        </header>
        <pre className="yaml-preview" data-testid="yaml-preview">
{serializeFormToYaml(form)}
        </pre>
      </div>

      {error ? <p className="error small">{error}</p> : null}

      <div className="plugin-form-actions">
        {onCancel ? (
          <button type="button" className="btn-ghost" onClick={onCancel} disabled={busy}>
            Cancel
          </button>
        ) : null}
        <button
          type="button"
          className="btn-neon"
          onClick={submit}
          disabled={busy}
          data-testid="plugin-submit"
        >
          {busy ? "Saving…" : submitLabel}
        </button>
      </div>
    </div>
  );
}

/* ---------------------------------------------------------------------- */
/*                               Form mode                                 */
/* ---------------------------------------------------------------------- */

function FormMode({
  form,
  update,
  updateService,
  updateSend,
  updateCollect,
}: {
  form: PluginFormState;
  update: (patch: Partial<PluginFormState>) => void;
  updateService: (patch: Partial<PluginFormState["service"]>) => void;
  updateSend: (patch: Partial<PluginFormState["send"]>) => void;
  updateCollect: (patch: Partial<PluginFormState["collect"]>) => void;
}) {
  return (
    <div className="plugin-form-grid">
      <FieldRow label="Plugin name">
        <input
          className="input"
          value={form.name}
          onChange={(e) => update({ name: e.target.value })}
          placeholder="langfuse-judge"
        />
        <p className="muted tiny">
          Unique identifier. The DB enforces a DNS-style regex
          (lowercase, dash-separated).
        </p>
      </FieldRow>

      <Section title="Service">
        <FieldRow label="Service kind">
          <select
            className="input"
            value={form.service.kind}
            onChange={(e) => updateService({ kind: e.target.value as ServiceKind })}
          >
            <option value="langfuse">Langfuse</option>
            <option value="langsmith">LangSmith</option>
            <option value="datadog">Datadog LLM Obs</option>
            <option value="braintrust">Braintrust</option>
            <option value="arize">Arize Phoenix</option>
            <option value="otel">OpenTelemetry collector</option>
            <option value="webhook">Generic HTTPS webhook</option>
          </select>
        </FieldRow>
        <FieldRow label="Endpoint">
          <input
            className="input"
            value={form.service.endpoint}
            onChange={(e) => updateService({ endpoint: e.target.value })}
            placeholder="https://cloud.langfuse.com"
          />
        </FieldRow>
        <FieldRow label="Auth — K8s Secret name">
          <input
            className="input"
            value={form.service.secretRef}
            onChange={(e) => updateService({ secretRef: e.target.value })}
            placeholder="langfuse-creds"
          />
          <p className="muted tiny">
            Reference to a Kubernetes Secret. The Secret must hold the
            public/secret key pair (or API token) under the keys listed
            in "keyRef" below. Plaintext values are never written into
            the plugin record.
          </p>
        </FieldRow>
        <FieldRow label="Auth — keyRef (keys inside the Secret)">
          <input
            className="input"
            value={form.service.keyRef}
            onChange={(e) => updateService({ keyRef: e.target.value })}
            placeholder="public_key|secret_key"
          />
          <p className="muted tiny">
            Use <code>key1|key2</code> for two-token auth (Langfuse),
            or a single key name (LangSmith, Datadog).
          </p>
        </FieldRow>
      </Section>

      <Section title="Send — how traces are forwarded">
        <FieldRow label="Trigger">
          <select
            className="input"
            value={form.send.trigger}
            onChange={(e) => updateSend({ trigger: e.target.value as Trigger })}
          >
            <option value="on_trace">on_trace — every trace</option>
            <option value="scheduled">scheduled — periodic batch</option>
            <option value="manual">manual — on demand</option>
          </select>
        </FieldRow>
        <FieldRow label={`Sampling (${form.send.samplingPct.toFixed(0)}% — ${roundFraction(pctToFraction(form.send.samplingPct)).toFixed(4)})`}>
          <input
            type="range"
            min={0}
            max={100}
            step={1}
            value={form.send.samplingPct}
            onChange={(e) => updateSend({ samplingPct: Number(e.target.value) })}
          />
          <p className="muted tiny">
            Probability that any single trace is forwarded to the
            external service. 0 disables, 100 forwards every trace.
          </p>
        </FieldRow>
        <FieldRow label="Redact">
          <label className="checkbox">
            <input
              type="checkbox"
              checked={form.send.redact === "pii"}
              onChange={(e) =>
                updateSend({ redact: e.target.checked ? "pii" : "" })
              }
            />
            <span>Redact PII (email, phone, identifiers) before send</span>
          </label>
        </FieldRow>
        <FieldRow label="Payload — template variables">
          <PayloadPairs
            pairs={form.send.payload}
            onChange={(rows) => updateSend({ payload: rows })}
          />
          <p className="muted tiny">
            Each key is the JSON field the vendor receives. Value is a
            Go&nbsp;text/template expression — e.g.{" "}
            <code>{`{{ index .trace "gen_ai.input.messages" }}`}</code>.
          </p>
        </FieldRow>
      </Section>

      <Section title="Collect — how results come back">
        <FieldRow label="Mode">
          <select
            className="input"
            value={form.collect.mode}
            onChange={(e) => updateCollect({ mode: e.target.value as CollectMode })}
          >
            <option value="webhook">webhook — vendor pushes to Nexus</option>
            <option value="poll">poll — Nexus pulls every N seconds</option>
            <option value="sync">sync — inline response (rare)</option>
          </select>
        </FieldRow>
        <FieldRow label="Interval (poll mode only)">
          <input
            className="input"
            value={form.collect.interval}
            onChange={(e) => updateCollect({ interval: e.target.value })}
            placeholder="60s"
          />
          <p className="muted tiny">Accepts Go strings (<code>60s</code>) or bare seconds (<code>60</code>).</p>
        </FieldRow>
        <FieldRow label="Result mapping — vendor JSON → OTel eval fields">
          <MappingPairs
            pairs={form.collect.mapping}
            onChange={(rows) => updateCollect({ mapping: rows })}
          />
          <p className="muted tiny">
            Maps the vendor&apos;s response shape into the OTel
            {" "}<code>gen_ai.evaluation.result</code> schema so scores
            store uniformly regardless of vendor.
          </p>
        </FieldRow>
      </Section>

      <Section title="Wire">
        <FieldRow label="Timeout">
          <input
            className="input"
            value={form.timeout}
            onChange={(e) => update({ timeout: e.target.value })}
            placeholder="30s"
          />
          <p className="muted tiny">Max duration a single send/collect call may take.</p>
        </FieldRow>
      </Section>
    </div>
  );
}

function YamlMode({
  yamlDraft,
  setYamlDraft,
  yamlError,
  onCommit,
}: {
  yamlDraft: string;
  setYamlDraft: (s: string) => void;
  yamlError: string | null;
  onCommit: () => void;
}) {
  return (
    <div className="plugin-form-yaml">
      <textarea
        className="yaml-textarea"
        value={yamlDraft}
        onChange={(e) => setYamlDraft(e.target.value)}
        rows={26}
        spellCheck={false}
        data-testid="yaml-editor"
      />
      {yamlError ? <p className="error small">{yamlError}</p> : null}
      <button type="button" className="btn-ghost" onClick={onCommit}>
        Parse YAML into form
      </button>
    </div>
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

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <fieldset className="plugin-form-section">
      <legend>{title}</legend>
      {children}
    </fieldset>
  );
}

function PayloadPairs({
  pairs,
  onChange,
}: {
  pairs: PluginFormState["send"]["payload"];
  onChange: (next: PluginFormState["send"]["payload"]) => void;
}) {
  const update = (i: number, patch: Partial<PluginFormState["send"]["payload"][number]>) => {
    const next = pairs.map((p, idx) => (idx === i ? { ...p, ...patch } : p));
    onChange(next);
  };
  const remove = (i: number) => onChange(pairs.filter((_, idx) => idx !== i));
  const add = () =>
    onChange([...pairs, { id: rid("p"), key: "", template: "" }]);
  return (
    <div className="kv-editor">
      {pairs.length === 0 ? (
        <p className="muted tiny">No payload templates. Add one to send trace data.</p>
      ) : null}
      {pairs.map((p, i) => (
        <div key={p.id} className="kv-row">
          <input
            className="input"
            placeholder="key (e.g. input)"
            value={p.key}
            onChange={(e) => update(i, { key: e.target.value })}
          />
          <input
            className="input"
            placeholder='template (e.g. {{ index .trace "x" }})'
            value={p.template}
            onChange={(e) => update(i, { template: e.target.value })}
          />
          <button
            type="button"
            className="btn-ghost btn-tiny"
            onClick={() => remove(i)}
            aria-label="Remove payload"
          >
            <Icon.x size={12} />
          </button>
        </div>
      ))}
      <button type="button" className="btn-ghost btn-small" onClick={add}>
        + Add payload field
      </button>
    </div>
  );
}

function MappingPairs({
  pairs,
  onChange,
}: {
  pairs: PluginFormState["collect"]["mapping"];
  onChange: (next: PluginFormState["collect"]["mapping"]) => void;
}) {
  const update = (i: number, patch: Partial<PluginFormState["collect"]["mapping"][number]>) => {
    const next = pairs.map((p, idx) => (idx === i ? { ...p, ...patch } : p));
    onChange(next);
  };
  const remove = (i: number) => onChange(pairs.filter((_, idx) => idx !== i));
  const add = () =>
    onChange([
      ...pairs,
      { id: rid("m"), target: "name", jpath: "" },
    ]);
  return (
    <div className="kv-editor">
      {pairs.length === 0 ? (
        <p className="muted tiny">No mappings. Service defaults will be used.</p>
      ) : null}
      {pairs.map((p, i) => (
        <div key={p.id} className="kv-row">
          <select
            className="input"
            value={p.target}
            onChange={(e) => update(i, { target: e.target.value as typeof p.target })}
          >
            <option value="name">name</option>
            <option value="score">score</option>
            <option value="label">label</option>
            <option value="explanation">explanation</option>
            <option value="trace_id">trace_id</option>
            <option value="metric">metric</option>
          </select>
          <input
            className="input"
            placeholder="vendor JSON path (e.g. $.key)"
            value={p.jpath}
            onChange={(e) => update(i, { jpath: e.target.value })}
          />
          <button
            type="button"
            className="btn-ghost btn-tiny"
            onClick={() => remove(i)}
            aria-label="Remove mapping"
          >
            <Icon.x size={12} />
          </button>
        </div>
      ))}
      <button type="button" className="btn-ghost btn-small" onClick={add}>
        + Add mapping
      </button>
    </div>
  );
}

/* ---------------------------------------------------------------------- */
/*                 Plugin list — list rows used everywhere                 */
/* ---------------------------------------------------------------------- */

/**
 * Standalone list view + add button. Embedded by both the legacy
 * `/eval/plugins` page and the merged `Evaluators` card on `/eval`.
 */
export function PluginListCard({
  showHeader = true,
  onEdit,
  onCreate,
}: {
  showHeader?: boolean;
  onEdit: (rec: EvalPluginRecord) => void;
  onCreate: () => void;
}) {
  const qc = useQueryClient();
  const query = useQuery({
    queryKey: ["eval-plugins"],
    queryFn: fetchEvalPlugins,
    refetchInterval: 30_000,
  });

  const toggleM = useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) =>
      patchEvalPlugin(id, { enabled }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["eval-plugins"] }),
  });

  const deleteM = useMutation({
    mutationFn: (id: string) => deleteEvalPlugin(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["eval-plugins"] }),
  });

  const testM = useMutation({
    mutationFn: (id: string) => testEvalPlugin(id),
  });

  const lastError =
    (testM.error as Error | null) ||
    (toggleM.error as Error | null) ||
    (deleteM.error as Error | null);

  // Track which plugin's Keys modal is open. null == closed. We use
  // a string state (instead of boolean per row) so exactly one modal
  // can be open at a time. The refetch that PUT/DELETE triggers below
  // re-renders the row with the new configured state.
  const [keysFor, setKeysFor] = useState<string | null>(null);

  return (
    <div className="plugin-list-card">
      {showHeader ? (
        <header className="panel-head">
          <div>
            <h2 className="panel-title">External plugins</h2>
            <p className="muted small">
              Forward traces to Langfuse, LangSmith, Datadog, OTel, or
              any HTTPS endpoint. Each plugin reads auth from a Kubernetes
              Secret — plaintext values are never stored.
            </p>
          </div>
          <button type="button" className="btn-neon" onClick={onCreate}>
            + Install plugin
          </button>
        </header>
      ) : null}

      {query.isLoading ? <p className="muted small">Loading plugins…</p> : null}
      {query.error ? <p className="error small">Failed to load plugins.</p> : null}

      {!query.isLoading && (query.data?.length ?? 0) === 0 ? (
        <p className="muted small">
          No plugins installed yet. Click <strong>Install plugin</strong>{" "}
          to connect to Langfuse, LangSmith, Datadog, etc.
        </p>
      ) : null}

      {query.data && query.data.length > 0 ? (
        <div className="plugin-list">
          {query.data.map((p) => {
            const isRowTesting =
              !!testM.isPending && testM.variables === p.id;
            return (
              <PluginRow
                key={p.id ?? p.name}
                rec={p}
                onToggle={() => p.id && toggleM.mutate({ id: p.id, enabled: !p.enabled })}
                onDelete={() => p.id && deleteM.mutate(p.id)}
                onEdit={() => onEdit(p)}
                onTest={() => testM.mutate(p.name)}
                onKeys={() => setKeysFor(p.name)}
                testResult={
                  !isRowTesting && testM.variables === p.name ? testM.data : undefined
                }
                testError={
                  !isRowTesting && testM.variables === p.name && testM.error
                    ? (testM.error as Error).message
                    : undefined
                }
                busyToggle={!!toggleM.isPending && toggleM.variables?.id === p.id}
                busyDelete={!!deleteM.isPending && deleteM.variables === p.id}
                busyTest={isRowTesting}
              />
            );
          })}
        </div>
      ) : null}

      {lastError ? <p className="error small">{lastError.message}</p> : null}

      <PluginKeysModal
        pluginName={keysFor ?? ""}
        open={keysFor !== null}
        onClose={() => setKeysFor(null)}
      />
    </div>
  );
}

function PluginRow({
  rec,
  onEdit,
  onToggle,
  onDelete,
  onTest,
  onKeys,
  testResult,
  testError,
  busyToggle,
  busyDelete,
  busyTest,
}: {
  rec: EvalPluginRecord;
  onEdit: () => void;
  onToggle: () => void;
  onDelete: () => void;
  onTest: () => void;
  onKeys: () => void;
  testResult?: PluginTestResult;
  testError?: string;
  busyToggle: boolean;
  busyDelete: boolean;
  busyTest: boolean;
}) {
  const parsed = useMemo(() => safeParse(rec.spec_yaml), [rec.spec_yaml]);
  // Inbound webhook URL that vendors should POST score records to.
  // Computed lazily so it adapts to whatever origin the console is
  // mounted under (custom hosted tenants don't have to hard-code
  // their domain in the manifest).
  const webhookUrl = useMemo(() => {
    if (typeof window === "undefined") return "";
    return `${window.location.origin}/api/eval/plugins/${encodeURIComponent(rec.name)}/webhook`;
  }, [rec.name]);
  const [showWebhook, setShowWebhook] = useState(false);
  const [copied, setCopied] = useState(false);
  const pingM = useMutation({
    mutationFn: () =>
      pingEvalPluginWebhook(rec.name, {
        name: "smoke",
        score: 1.0,
        label: "pass",
        explanation: "smoke test from the console UI",
        trace_id: `smoke-${Date.now()}`,
      }),
  });
  const canPing = parsed.mode === "webhook" || parsed.mode === "sync";
  return (
    <article className="plugin-row" data-testid={`plugin-row-${rec.id}`}>
      <div className="plugin-row-head">
        <div>
          <strong className="plugin-row-name">{rec.name}</strong>{" "}
          {parsed.type ? <Chip tone="info">{parsed.type}</Chip> : null}{" "}
          {parsed.sampling ? (
            <Chip tone="neutral">sample {parsed.sampling}</Chip>
          ) : null}{" "}
          {parsed.mode ? <Chip tone="neutral">{parsed.mode}</Chip> : null}
        </div>
        <div className="plugin-row-actions">
          <LabelToggle
            checked={rec.enabled}
            onChange={onToggle}
            disabled={busyToggle}
            label={`Enable ${rec.name}`}
          />
          <button
            type="button"
            className="btn-ghost btn-small"
            onClick={onTest}
            disabled={busyTest || !rec.enabled}
          >
            {busyTest ? "Testing…" : "Test"}
          </button>
          <button type="button" className="btn-ghost btn-small" onClick={onEdit}>
            Edit
          </button>
          <button
            type="button"
            className="btn-ghost btn-small"
            onClick={() => setShowWebhook((v) => !v)}
            aria-expanded={showWebhook}
            data-testid={`plugin-webhook-toggle-${rec.name}`}
          >
            {showWebhook ? "Hide URL" : "Webhook URL"}
          </button>
          <button
            type="button"
            className="btn-ghost btn-small"
            onClick={onKeys}
            data-testid={`plugin-keys-button-${rec.name}`}
          >
            <Icon.keys size={12} /> Keys
          </button>
          {canPing ? (
            <button
              type="button"
              className="btn-ghost btn-small"
              onClick={() => pingM.mutate()}
              disabled={pingM.isPending}
              data-testid={`plugin-webhook-ping-${rec.name}`}
            >
              {pingM.isPending ? "Sending…" : "Send test score"}
            </button>
          ) : null}
          <button
            type="button"
            className="btn-ghost btn-small row-action-danger"
            onClick={() => {
              if (window.confirm(`Delete plugin ${rec.name}?`)) onDelete();
            }}
            disabled={busyDelete}
          >
            {busyDelete ? "Deleting…" : "Delete"}
          </button>
        </div>
      </div>
      {showWebhook ? (
        <div className="plugin-webhook-box" data-testid={`plugin-webhook-${rec.name}`}>
          <p className="muted small">
            Vendors POST score records to:
          </p>
          <code className="plugin-webhook-url">{webhookUrl}</code>
          <div className="plugin-webhook-actions">
            <button
              type="button"
              className="btn-ghost btn-small"
              onClick={async () => {
                try {
                  await navigator.clipboard.writeText(webhookUrl);
                  setCopied(true);
                  setTimeout(() => setCopied(false), 1500);
                } catch {
                  setCopied(false);
                }
              }}
            >
              {copied ? "Copied!" : "Copy URL"}
            </button>
          </div>
          {pingM.data ? (
            <p
              className={`plugin-row-test ${pingM.data.ok ? "ok" : "err"}`}
              data-testid={`plugin-webhook-ping-result-${rec.name}`}
            >
              {pingM.data.ok
                ? `Inbound accepted (${pingM.data.accepted ?? 1} score).`
                : `Inbound failed: ${pingM.data.message ?? "unknown error"}`}
            </p>
          ) : null}
          {pingM.error ? (
            <p className="plugin-row-test err">
              Inbound failed: {(pingM.error as Error).message}
            </p>
          ) : null}
        </div>
      ) : null}
      {testResult ? (
        <p
          className={`plugin-row-test ${testResult.ok ? "ok" : "err"}`}
          data-testid={`plugin-test-${rec.name}`}
        >
          {testResult.ok
            ? `✓ ${testResult.message}`
            : `✗ ${testResult.message}`}
          {typeof testResult.latency_ms === "number"
            ? ` (${testResult.latency_ms}ms)`
            : ""}
        </p>
      ) : null}
      {testError ? (
        <p className="plugin-row-test err" data-testid={`plugin-test-${rec.name}`}>
          ✗ {testError}
        </p>
      ) : null}
    </article>
  );
}

/**
 * Cheap visual extract with the same logic the old list used.
 */
function safeParse(raw: string): {
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

/* ---------------------------------------------------------------------- */
/*                  Legacy /eval/plugins page (drives form)                */
/* ---------------------------------------------------------------------- */

export function EvalPlugins() {
  const [editorOpen, setEditorOpen] = useState(false);
  const [editingForm, setEditingForm] = useState<PluginFormState | undefined>(undefined);

  function openCreate() {
    setEditingForm(undefined);
    setEditorOpen(true);
  }

  function openEdit(rec: EvalPluginRecord) {
    setEditingForm(parseYamlToForm(rec.spec_yaml ?? ""));
    setEditorOpen(true);
  }

  return (
    <div className="page eval-plugins-page">
      <header className="page-head">
        <div>
          <h1 className="page-title">Eval plugins</h1>
          <p className="page-sub">
            Connect Nexus to external evaluation services. The same
            form is also reachable from the{" "}
            <a href="/eval">main Eval page</a> (Phase H merged view).
          </p>
        </div>
      </header>
      <PluginListCard onCreate={openCreate} onEdit={openEdit} />
      <PluginEditorDrawer
        open={editorOpen}
        initial={editingForm}
        onClose={() => setEditorOpen(false)}
        onSaved={() => {
          /* the createM mutation already invalidates the list query */
        }}
      />
    </div>
  );
}

/* ---------------------------------------------------------------------- */
/*          Drawer composed from PluginForm (used by /eval too)            */
/* ---------------------------------------------------------------------- */

/**
 * The install/edit drawer used by both the /eval/plugins page and the
 * merged /eval page. The form is the same one everywhere; on a brand
 * new install we offer preset shortcuts.
 */
export function PluginEditorDrawer({
  open,
  initial,
  onClose,
  onSaved,
}: {
  open: boolean;
  initial?: PluginFormState;
  onClose: () => void;
  onSaved: (rec: EvalPluginRecord, form: PluginFormState) => void;
}) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [preset, setPreset] = useState<string>(
    initial?.service?.kind ?? "langfuse",
  );

  const submitM = useMutation({
    mutationFn: (form: PluginFormState) =>
      createEvalPlugin({
        name: form.name.trim(),
        spec_yaml: serializeFormToYaml(form),
        enabled: true,
      }),
    onSuccess: (rec, form) => {
      qc.invalidateQueries({ queryKey: ["eval-plugins"] });
      setError(null);
      onSaved(rec, form);
      onClose();
    },
    onError: (e: Error) => setError(e.message),
  });

  return (
    <Drawer
      open={open}
      title={initial ? "Edit plugin" : "Install plugin"}
      onClose={onClose}
      testId="plugin-editor-drawer"
    >
      {!initial ? (
        <div className="plugin-form-presets">
          <p className="muted small">Start from a preset:</p>
          <div className="chip-row">
            {Object.entries(PLUGIN_PRESETS).map(([key, p]) => (
              <button
                key={key}
                type="button"
                className={`btn-ghost btn-small ${preset === key ? "is-active" : ""}`}
                onClick={() => setPreset(key)}
              >
                {p.label}
              </button>
            ))}
          </div>
        </div>
      ) : null}

      <PluginForm
        initial={initial ?? PLUGIN_PRESETS[preset]?.form ?? DEFAULT_PLUGIN_TEMPLATE}
        onCancel={onClose}
        onSaved={(_rec, form) => submitM.mutate(form)}
        busy={submitM.isPending}
        error={error}
        submitLabel={initial ? "Save changes" : "Install plugin"}
      />
    </Drawer>
  );
}
