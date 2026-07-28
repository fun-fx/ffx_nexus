/**
 * Plugin manifest <-> form state converter.
 *
 * Operators configure an EvalPlugin via structured fields (no raw YAML,
 * no surprise line-formatting surprises), but the wire format is still
 * the canonical YAML schema from `internal/evalplugin/types.go`. This
 * module owns the translation so a single field-state object drives both
 * the form inputs and the live YAML preview.
 *
 * The form is a *named tree* of typed fields; the server validator
 * (evalplugin.Validate) is still the authoritative contract. Anything
 * we round-trip here must survive a server round-trip without losing
 * information — round-trip is asserted by the unit tests.
 */

export type ServiceKind =
  | "langsmith"
  | "langfuse"
  | "datadog"
  | "braintrust"
  | "arize"
  | "otel"
  | "webhook";

export type Trigger = "on_trace" | "scheduled" | "manual";

export type CollectMode = "sync" | "webhook" | "poll";

export type Redact = "" | "pii";

export interface PluginFormState {
  name: string;
  service: {
    kind: ServiceKind;
    endpoint: string;
    secretRef: string;
    keyRef: string;
  };
  send: {
    trigger: Trigger;
    samplingPct: number; // 0..100 — sliders are easier than raw fractions
    payload: Array<{ id: string; key: string; template: string }>;
    redact: Redact;
  };
  collect: {
    mode: CollectMode;
    interval: string; // "60s", "1m", "60"
    mapping: Array<{
      id: string;
      target: "name" | "score" | "label" | "explanation" | "trace_id" | "metric";
      jpath: string;
    }>;
  };
  timeout: string;
}

export const DEFAULT_PLUGIN_TEMPLATE: PluginFormState = {
  name: "langfuse-judge",
  service: {
    kind: "langfuse",
    endpoint: "https://cloud.langfuse.com",
    secretRef: "langfuse-creds",
    keyRef: "public_key|secret_key",
  },
  send: {
    trigger: "on_trace",
    samplingPct: 10,
    payload: [
      {
        id: "p_in",
        key: "input",
        template: '{{ index .trace "gen_ai.input.messages" }}',
      },
      {
        id: "p_out",
        key: "output",
        template: '{{ index .trace "gen_ai.output.messages" }}',
      },
    ],
    redact: "pii",
  },
  collect: {
    mode: "webhook",
    interval: "60s",
    mapping: [
      { id: "m1", target: "name", jpath: "name" },
      { id: "m2", target: "score", jpath: "value" },
      { id: "m3", target: "explanation", jpath: "comment" },
      { id: "m4", target: "trace_id", jpath: "traceId" },
    ],
  },
  timeout: "30s",
};

/**
 * Quick templates users can pick from the form's preset dropdown. Each
 * preset hydrates a *complete* form state so the operator can hand-tune
 * afterwards without cold-starting every field.
 */
export const PLUGIN_PRESETS: Record<string, { label: string; form: PluginFormState }> = {
  langfuse: {
    label: "Langfuse (cloud)",
    form: DEFAULT_PLUGIN_TEMPLATE,
  },
  langfuse_selfhost: {
    label: "Langfuse (self-host)",
    form: {
      ...DEFAULT_PLUGIN_TEMPLATE,
      service: {
        ...DEFAULT_PLUGIN_TEMPLATE.service,
        endpoint: "https://langfuse.example.internal",
        secretRef: "langfuse-creds",
        keyRef: "public_key|secret_key",
      },
    },
  },
  langsmith: {
    label: "LangSmith",
    form: {
      ...DEFAULT_PLUGIN_TEMPLATE,
      name: "langsmith-judge",
      service: {
        kind: "langsmith",
        endpoint: "https://api.smith.langchain.com",
        secretRef: "langsmith-api-key",
        keyRef: "value",
      },
      send: {
        ...DEFAULT_PLUGIN_TEMPLATE.send,
        payload: [
          {
            id: "p_in",
            key: "input",
            template: "{{ .trace.input_messages }}",
          },
          {
            id: "p_out",
            key: "output",
            template: "{{ .trace.output_messages }}",
          },
          {
            id: "p_ref",
            key: "reference",
            template: "{{ .trace.eval_reference }}",
          },
        ],
      },
      collect: {
        mode: "webhook",
        interval: "60s",
        mapping: [
          { id: "m1", target: "name", jpath: "$.key" },
          { id: "m2", target: "score", jpath: "$.score" },
          { id: "m3", target: "label", jpath: "$.value" },
          { id: "m4", target: "explanation", jpath: "$.comment" },
          { id: "m5", target: "trace_id", jpath: "$.trace_id" },
          { id: "m6", target: "metric", jpath: "$.metric" },
        ],
      },
    },
  },
  datadog: {
    label: "Datadog LLM Obs",
    form: {
      ...DEFAULT_PLUGIN_TEMPLATE,
      name: "datadog-judge",
      service: {
        kind: "datadog",
        endpoint: "https://api.datadoghq.com",
        secretRef: "datadog-api-key",
        keyRef: "value",
      },
      collect: {
        mode: "webhook",
        interval: "60s",
        mapping: [
          { id: "m1", target: "name", jpath: "label" },
          { id: "m2", target: "score", jpath: "value" },
          { id: "m3", target: "label", jpath: "metric_type" },
          { id: "m4", target: "explanation", jpath: "reasoning" },
          { id: "m5", target: "trace_id", jpath: "join_on.trace_id" },
        ],
      },
    },
  },
  webhook: {
    label: "Generic HTTPS webhook",
    form: {
      ...DEFAULT_PLUGIN_TEMPLATE,
      name: "custom-webhook",
      service: {
        kind: "webhook",
        endpoint: "https://hooks.example.internal/eval",
        secretRef: "webhook-token",
        keyRef: "value",
      },
    },
  },
};

/* ---------------------------------------------------------------------- */
/*                                helpers                                 */
/* ---------------------------------------------------------------------- */

let _idCounter = 0;
export function rid(prefix: string): string {
  _idCounter += 1;
  return `${prefix}_${Date.now().toString(36)}_${_idCounter}`;
}

/**
 * Round a number to at most 4 fractional digits so the YAML preview isn't
 * visually noisy (e.g. "0.10000000000000002" from a JS percent dance).
 */
export function roundFraction(n: number): number {
  if (!Number.isFinite(n)) return 0;
  return Math.round(n * 10_000) / 10_000;
}

export function pctToFraction(pct: number): number {
  if (!Number.isFinite(pct)) return 0;
  return Math.max(0, Math.min(100, pct)) / 100;
}
export function fractionToPct(frac: number): number {
  if (!Number.isFinite(frac)) return 0;
  return Math.max(0, Math.min(1, frac)) * 100;
}

/* ---------------------------------------------------------------------- */
/*                       form  <->  YAML serializer                       */
/* ---------------------------------------------------------------------- */

/**
 * Render the form state back to a YAML manifest line-for-line. This is
 * the structured inverse of `parseYamlToForm`: the indent levels are
 * unambiguous and the payload/mapping sub-trees are emitted as flow
 * (single-line `{}`) so a sibling field at the same indent level isn't
 * ever mistaken for a child of the previous bucket.
 */
export function serializeFormToYaml(form: PluginFormState): string {
  const lines: string[] = [];
  lines.push("apiVersion: nexus.io/v1alpha1");
  lines.push("kind: EvalPlugin");
  lines.push("metadata:");
  lines.push(`  name: ${form.name.trim()}`);
  lines.push("spec:");
  lines.push("  service:");
  lines.push(`    type: ${form.service.kind}`);
  lines.push(`    endpoint: ${form.service.endpoint.trim()}`);
  lines.push(`    secretRef: ${form.service.secretRef.trim()}`);
  lines.push(`    keyRef: ${form.service.keyRef.trim()}`);
  lines.push("  send:");
  lines.push(`    trigger: ${form.send.trigger}`);
  lines.push(`    sampling: ${roundFraction(pctToFraction(form.send.samplingPct))}`);
  lines.push(`    redact: [${form.send.redact || ""}]`);
  if (form.send.payload.length > 0) {
    const entries = form.send.payload
      .filter((p) => p.key.trim())
      .map((p) => `${p.key.trim()}: ${quoteTemplate(p.template.trim())}`)
      .join(", ");
    if (entries) lines.push(`    payload: { ${entries} }`);
  }
  lines.push("  collect:");
  lines.push(`    mode: ${form.collect.mode}`);
  if (form.collect.interval.trim()) {
    lines.push(`    interval: ${form.collect.interval.trim()}`);
  }
  if (form.collect.mapping.length > 0) {
    const entries = form.collect.mapping
      .filter((m) => m.jpath.trim())
      .map((m) => `${m.target}: ${m.jpath.trim().replace(/"/g, '\\"')}`)
      .join(", ");
    if (entries) lines.push(`    mapping: { ${entries} }`);
  }
  lines.push(`  timeout: ${form.timeout.trim()}`);
  return lines.join("\n") + "\n";
}

/**
 * Wrap a value in single quotes if the YAML parser might try to
 * interpret it (Go template syntax, ":" after a space, etc). The
 * payload templates in this form frequently contain `{{ ... }}`.
 */
function quoteTemplate(s: string): string {
  if (s.startsWith("'") && s.endsWith("'")) return s;
  return `'${s}'`;
}

/* ---------------------------------------------------------------------- */
/*                 Depth-aware YAML parser for round-tripping             */
/* ---------------------------------------------------------------------- */

type Frame = { key: string; depth: number };

/**
 * Loose parser that hydrates a form from existing YAML. Indent semantics:
 *
 *   0 spaces — top-level (`apiVersion`, `kind`, `metadata`, `spec`)
 *   2 spaces — spec children (`service`/`send`/`collect`) and the
 *              `name:` line under `metadata:`
 *   4 spaces — fields directly under service/send/collect (depth-1)
 *   6 spaces — entries inside block-style `payload`/`mapping` (depth-2)
 *
 * The form emits flow-style `payload: { ... }` and `mapping: { ... }`
 * directly on a single line; both forms are accepted, but the parser
 * never treats a 4-space sibling as a child of the previous bucket —
 * it tracks depth with a tiny stack.
 */
export function parseYamlToForm(yaml: string): PluginFormState {
  const out: PluginFormState = {
    ...DEFAULT_PLUGIN_TEMPLATE,
    name: "",
    send: {
      ...DEFAULT_PLUGIN_TEMPLATE.send,
      payload: [],
      redact: "",
    },
    collect: {
      ...DEFAULT_PLUGIN_TEMPLATE.collect,
      mapping: [],
    },
    timeout: "",
  };

  const stack: Frame[] = [];

  const topKey = (): string =>
    stack.length === 0 ? "" : stack[stack.length - 1].key;
  const indentOf = (line: string): number => {
    let i = 0;
    while (i < line.length && line[i] === " ") i++;
    return i;
  };

  const lines = yaml.split(/\r?\n/);
  for (const raw of lines) {
    const line = raw.replace(/\s+$/, "");
    if (line === "" || line.trim().startsWith("#")) continue;
    const indent = indentOf(line);
    const trimmed = line.trim();

    // Pop the stack back to a level strictly less than the current
    // indent. This is the critical bit that prevents a 4-space sibling
    // from being incorrectly scoped to a depth-2 frame inside the
    // previous bucket.
    while (stack.length > 0 && stack[stack.length - 1].depth >= indent) {
      stack.pop();
    }

    if (indent === 0) {
      stack.length = 0;
      if (/^metadata:/i.test(trimmed)) {
        stack.push({ key: "metadata", depth: 0 });
      } else if (/^spec:/i.test(trimmed)) {
        stack.push({ key: "spec", depth: 0 });
      }
      continue;
    }

    const key = topKey();

    if (key === "metadata" && indent === 2) {
      const m = /^name:\s*(.*)$/.exec(trimmed);
      if (m) out.name = cleanYamlScalar(m[1]);
      continue;
    }

    if (key === "spec" && indent === 2) {
      const m = /^([a-zA-Z]+):\s*(.*)$/.exec(trimmed);
      if (!m) continue;
      const k = m[1];
      const v = cleanYamlScalar(m[2]);
      if (
        k === "service" || k === "send" || k === "collect"
      ) {
        stack.push({ key: k, depth: indent });
      } else if (k === "timeout") {
        out.timeout = v;
      }
      continue;
    }

    if (key === "service" && indent === 4) {
      const m = /^([a-zA-Z]+):\s*(.*)$/.exec(trimmed);
      if (!m) continue;
      const k = m[1];
      const v = cleanYamlScalar(m[2]);
      if (k === "type" && isServiceKind(v)) out.service.kind = v;
      else if (k === "endpoint") out.service.endpoint = v;
      else if (k === "secretRef") out.service.secretRef = v;
      else if (k === "keyRef") out.service.keyRef = v;
      continue;
    }

    if (key === "send" && indent === 4) {
      const m = /^([a-zA-Z]+):\s*(.*)$/.exec(trimmed);
      if (!m) continue;
      const k = m[1];
      const v = cleanYamlScalar(m[2]);
      if (k === "trigger") {
        if (v === "on_trace" || v === "scheduled" || v === "manual") {
          out.send.trigger = v;
        }
      } else if (k === "sampling") {
        const n = Number(v);
        if (Number.isFinite(n)) out.send.samplingPct = fractionToPct(n);
      } else if (k === "redact") {
        out.send.redact = v.includes("pii") ? "pii" : "";
      } else if (k === "payload") {
        if (v.length > 0) {
          for (const e of splitFlowEntries(v)) {
            out.send.payload.push({
              id: rid("p"),
              key: e.k,
              template: e.v,
            });
          }
        } else {
          stack.push({ key: "payload", depth: indent });
        }
      }
      continue;
    }

    if (key === "payload" && indent === 6) {
      const m = /^([a-zA-Z_][a-zA-Z0-9_-]*):\s*(.*)$/.exec(trimmed);
      if (!m) continue;
      out.send.payload.push({
        id: rid("p"),
        key: m[1],
        template: cleanYamlScalar(m[2]),
      });
      continue;
    }

    if (key === "collect" && indent === 4) {
      const m = /^([a-zA-Z]+):\s*(.*)$/.exec(trimmed);
      if (!m) continue;
      const k = m[1];
      const v = cleanYamlScalar(m[2]);
      if (k === "mode") {
        if (v === "sync" || v === "webhook" || v === "poll") out.collect.mode = v;
      } else if (k === "interval") out.collect.interval = v;
      else if (k === "mapping") {
        if (v.length > 0) {
          for (const e of splitFlowEntries(v)) {
            pushMapping(out, e.k, e.v);
          }
        } else {
          stack.push({ key: "mapping", depth: indent });
        }
      }
      continue;
    }

    if (key === "mapping" && indent === 6) {
      const m = /^([a-zA-Z_]+):\s*(.*)$/.exec(trimmed);
      if (!m) continue;
      pushMapping(out, m[1], cleanYamlScalar(m[2]));
      continue;
    }
  }
  return out;
}

function pushMapping(out: PluginFormState, k: string, v: string): void {
  const target = k as
    | "name"
    | "score"
    | "label"
    | "explanation"
    | "trace_id"
    | "metric";
  if (
    target === "name" ||
    target === "score" ||
    target === "label" ||
    target === "explanation" ||
    target === "trace_id" ||
    target === "metric"
  ) {
    out.collect.mapping.push({ id: rid("m"), target, jpath: v });
  }
}

function cleanYamlScalar(s: string): string {
  let v = s.trim();
  const hashAt = v.indexOf(" #");
  if (hashAt >= 0) v = v.slice(0, hashAt).trim();
  if (
    (v.startsWith("'") && v.endsWith("'")) ||
    (v.startsWith('"') && v.endsWith('"'))
  ) {
    v = v.slice(1, -1);
  }
  return v;
}

/**
 * Split a flow-mapping scalar `{ k1: v1, k2: v2 }` into key/value pairs.
 * Permissive — anything it can't confidently parse is returned as
 * `{ k: key, v: raw }` so a partial hydration doesn't lose the rest.
 */
function splitFlowEntries(raw: string): Array<{ k: string; v: string }> {
  let s = raw.trim();
  if (s.startsWith("{") && s.endsWith("}")) s = s.slice(1, -1);
  const out: Array<{ k: string; v: string }> = [];
  let i = 0;
  while (i < s.length) {
    while (i < s.length && /[\s,]/.test(s[i])) i++;
    if (i >= s.length) break;
    let key = "";
    while (i < s.length && s[i] !== ":" && s[i] !== ",") {
      if (s[i] === '"' || s[i] === "'") {
        const quote = s[i];
        i++;
        while (i < s.length && s[i] !== quote) key += s[i++];
        if (i < s.length) i++;
      } else {
        key += s[i++];
      }
    }
    key = key.trim();
    if (i >= s.length || s[i] !== ":") break;
    i++;
    while (i < s.length && s[i] === " ") i++;
    let val = "";
    if (i < s.length && (s[i] === "'" || s[i] === '"')) {
      const quote = s[i];
      i++;
      while (i < s.length && s[i] !== quote) val += s[i++];
      if (i < s.length) i++;
    } else {
      while (i < s.length && s[i] !== ",") val += s[i++];
    }
    val = val.trim();
    out.push({ k: key, v: val });
  }
  return out;
}

function isServiceKind(s: string): s is ServiceKind {
  return (
    s === "langsmith" ||
    s === "langfuse" ||
    s === "datadog" ||
    s === "braintrust" ||
    s === "arize" ||
    s === "otel" ||
    s === "webhook"
  );
}
