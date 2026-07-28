import { describe, expect, it } from "vitest";
import {
  DEFAULT_PLUGIN_TEMPLATE,
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
    expect(parsed.service.secretRef).toBe(DEFAULT_PLUGIN_TEMPLATE.service.secretRef);
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
