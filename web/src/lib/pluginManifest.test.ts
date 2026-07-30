import { describe, expect, it } from "vitest";
import {
  DEFAULT_PLUGIN_TEMPLATE,
  HEURISTIC_PRESETS,
  PLUGIN_PRESETS,
  parseYamlToForm,
  serializeFormToYaml,
} from "./pluginManifest";

describe("pluginManifest round-trip", () => {
  it("Langfuse default round-trips without losing any field", () => {
    const yaml = serializeFormToYaml(DEFAULT_PLUGIN_TEMPLATE);
    const parsed = parseYamlToForm(yaml);
    expect(parsed.name).toBe(DEFAULT_PLUGIN_TEMPLATE.name);
    expect(parsed.service.kind).toBe(DEFAULT_PLUGIN_TEMPLATE.service.kind);
    expect(parsed.service.endpoint).toBe(DEFAULT_PLUGIN_TEMPLATE.service.endpoint);
    // secretRef is intentionally absent from the form ever since
    // PR-#172's console-only key model. The form state itself has
    // no secretRef field anymore — this assertion is the regression
    // guard against silently re-adding it.
    expect(
      (parsed.service as unknown as Record<string, unknown>).secretRef,
    ).toBeUndefined();
    expect(parsed.service.keyRef).toBe(DEFAULT_PLUGIN_TEMPLATE.service.keyRef);
    expect(parsed.send.trigger).toBe(DEFAULT_PLUGIN_TEMPLATE.send.trigger);
    expect(Math.abs(parsed.send.samplingPct - DEFAULT_PLUGIN_TEMPLATE.send.samplingPct))
      .toBeLessThanOrEqual(1);
    expect(parsed.send.payload.length).toBe(DEFAULT_PLUGIN_TEMPLATE.send.payload.length);
    expect(parsed.send.payload[0].key).toBe(DEFAULT_PLUGIN_TEMPLATE.send.payload[0].key);
    expect(parsed.send.payload[0].template).toBe(DEFAULT_PLUGIN_TEMPLATE.send.payload[0].template);
    expect(parsed.send.redact).toBe(DEFAULT_PLUGIN_TEMPLATE.send.redact);
    expect(parsed.collect.mode).toBe(DEFAULT_PLUGIN_TEMPLATE.collect.mode);
    expect(parsed.collect.mapping.length).toBe(DEFAULT_PLUGIN_TEMPLATE.collect.mapping.length);
    expect(parsed.timeout).toBe(DEFAULT_PLUGIN_TEMPLATE.timeout);
  });

  it("every preset round-trips its essentials", () => {
    for (const [key, p] of Object.entries(PLUGIN_PRESETS)) {
      const yaml = serializeFormToYaml(p.form);
      const parsed = parseYamlToForm(yaml);
      expect(parsed.service.kind, `preset ${key}`).toBe(p.form.service.kind);
      expect(parsed.service.endpoint, `preset ${key}`).toBe(p.form.service.endpoint);
      expect(parsed.send.payload.length, `preset ${key}`).toBe(p.form.send.payload.length);
      expect(parsed.collect.mapping.length, `preset ${key}`).toBe(p.form.collect.mapping.length);
    }
  });

  it("emits flow-style payload so sibling fields do not get mis-scoped", () => {
    const yaml = serializeFormToYaml(DEFAULT_PLUGIN_TEMPLATE);
    expect(yaml).toMatch(/^\s+payload:\s*\{/m);
    expect(yaml).toMatch(/^\s+mapping:\s*\{/m);
  });

  it("redact value survives the round-trip", () => {
    const yaml = serializeFormToYaml(DEFAULT_PLUGIN_TEMPLATE);
    const parsed = parseYamlToForm(yaml);
    expect(parsed.send.redact).toBe("pii");
  });

  it("parseYamlToForm does not mutate DEFAULT_PLUGIN_TEMPLATE", () => {
    // Pre-condition: capture the canonical defaults once.
    const before = JSON.stringify(DEFAULT_PLUGIN_TEMPLATE);
    // Throw a handful of plausibly-mutating parses at the parser.
    parseYamlToForm("");
    parseYamlToForm(serializeFormToYaml(PLUGIN_PRESETS.webhook.form));
    parseYamlToForm(serializeFormToYaml(PLUGIN_PRESETS.langsmith.form));
    parseYamlToForm(serializeFormToYaml(PLUGIN_PRESETS.datadog.form));
    // Post-condition: nothing in the default was clobbered.
    expect(JSON.stringify(DEFAULT_PLUGIN_TEMPLATE)).toBe(before);
  });

  it("emit wraps keyRef in a nested auth: block", () => {
    const yaml = serializeFormToYaml(DEFAULT_PLUGIN_TEMPLATE);
    expect(yaml).toContain("    auth:\n");
    // secretRef must NOT appear in the rendered output. If it does the
    // form has been re-introduced to a deserialised legacy secretRef.
    expect(yaml).not.toMatch(/^\s+secretRef:/m);
    expect(yaml).toContain("      keyRef: public_key|secret_key\n");
  });

  it("parser drops the legacy flat secretRef shape silently", () => {
    // Pre-PR-#172 manifests carry a flat `secretRef:` (no nested
    // `auth:` wrapper). The form must still hydrate without
    // crashing — but the dropped field becomes a no-op so the
    // saved manifest no longer carries the legacy reference.
    const yaml = [
      "apiVersion: nexus.io/v1alpha1",
      "kind: EvalPlugin",
      "metadata:",
      "  name: langfuse-judge",
      "spec:",
      "  service:",
      "    type: langfuse",
      "    endpoint: https://cloud.langfuse.com",
      "    secretRef: langfuse-creds",
      "    keyRef: public_key|secret_key",
      "  send:",
      "    trigger: on_trace",
      "    sampling: 0.1",
      "    redact: [pii]",
      "  collect:",
      "    mode: webhook",
      "    interval: 60s",
      "  timeout: 30s",
      "",
    ].join("\n");
    const parsed = parseYamlToForm(yaml);
    expect(
      (parsed.service as unknown as Record<string, unknown>).secretRef,
    ).toBeUndefined();
    expect(parsed.service.keyRef).toBe("public_key|secret_key");
  });

  it("parser drops the legacy nested auth.secretRef shape silently", () => {
    const yaml = [
      "apiVersion: nexus.io/v1alpha1",
      "kind: EvalPlugin",
      "metadata:",
      "  name: langfuse-judge",
      "spec:",
      "  service:",
      "    type: langfuse",
      "    endpoint: https://cloud.langfuse.com",
      "    auth:",
      "      secretRef: langfuse-creds",
      "      keyRef: public_key|secret_key",
      "  send:",
      "    trigger: on_trace",
      "  collect:",
      "    mode: webhook",
      "  timeout: 30s",
      "",
    ].join("\n");
    const parsed = parseYamlToForm(yaml);
    expect(
      (parsed.service as unknown as Record<string, unknown>).secretRef,
    ).toBeUndefined();
    expect(parsed.service.keyRef).toBe("public_key|secret_key");
  });

  it("accepts a 4-space sibling even when `payload:` is mixed in", () => {
    // Simulate a hand-edited YAML where someone moved redact *before*
    // payload — the parser still routes to the form correctly.
    const yaml = [
      "apiVersion: nexus.io/v1alpha1",
      "kind: EvalPlugin",
      "metadata:",
      "  name: langfuse-judge",
      "spec:",
      "  service:",
      "    type: langfuse",
      "    endpoint: https://cloud.langfuse.com",
      "    secretRef: langfuse-creds",
      "    keyRef: public_key|secret_key",
      "  send:",
      "    trigger: on_trace",
      "    sampling: 0.1",
      "    redact: [pii]",
      "    payload: { input: 'foo', output: 'bar' }",
      "  collect:",
      "    mode: webhook",
      "    interval: 60s",
      "    mapping: { name: name, score: value }",
      "  timeout: 30s",
      "",
    ].join("\n");
    const parsed = parseYamlToForm(yaml);
    expect(parsed.send.redact).toBe("pii");
    expect(parsed.send.payload.length).toBe(2);
    expect(parsed.send.payload[0].key).toBe("input");
    expect(parsed.send.payload[1].key).toBe("output");
    expect(parsed.collect.mapping.length).toBe(2);
    expect(parsed.collect.mapping[0].target).toBe("name");
    expect(parsed.collect.mapping[1].target).toBe("score");
  });
});

describe("pluginManifest survey-driven presets", () => {
  it("PLUGIN_PRESETS exposes every survey target", () => {
    // The TechSy / PrimeIntellect / HF survey additions (PR #172/#174)
    // are useless if the form dropdown can't reach them. Each must
    // round-trip through serialize → parse cleanly. Every ServiceKind
    // exposed in the dropdown has a matching PLUGIN_PRESETS entry.
    for (const kind of [
      "langfuse",
      "langsmith",
      "confident_ai",
      "arize_phoenix",
      "otel_collector",
      "datadog",
      "braintrust",
      "arize",
      "webhook",
    ]) {
      expect(PLUGIN_PRESETS[kind], `preset missing: ${kind}`).toBeDefined();
      const p = PLUGIN_PRESETS[kind];
      expect(p.form.service.kind).toBe(kind);
      const yaml = serializeFormToYaml(p.form);
      const parsed = parseYamlToForm(yaml);
      expect(parsed.service.kind).toBe(kind);
    }
  });

  it("HEURISTIC_PRESETS covers the in-process metric kind names", () => {
    // The internal/evaluators/heuristic backend ships these metric
    // names today. The console advertises the same set so an
    // operator who reads the docs sees a matching tile.
    for (const k of [
      "contains",
      "pii",
      "exact_match",
      "rouge_l",
      "hf_evaluate",
      "lighteval",
      "ragas",
    ]) {
      expect(HEURISTIC_PRESETS[k], `heuristic missing: ${k}`).toBeDefined();
      expect(HEURISTIC_PRESETS[k].metric).toBe(k);
    }
  });

  it("otel_collector preset round-trips with collect.transport set", () => {
    // `transport: otel` is what the v1alpha1 otel_collector adapter
    // dispatcher reads; we have to make sure the form→yaml→form
    // round trip keeps the field or the created plugin silently
    // regresses to default behaviour.
    const preset = PLUGIN_PRESETS["otel_collector"];
    expect(preset).toBeDefined();
    expect(preset.form.collect.transport).toBe("otel");
    const yaml = serializeFormToYaml(preset.form);
    expect(yaml).toMatch(/transport:\s*otel/);
    const parsed = parseYamlToForm(yaml);
    expect(parsed.collect.transport).toBe("otel");
    expect(parsed.service.kind).toBe("otel_collector");
  });
});
