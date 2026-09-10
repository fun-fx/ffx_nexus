import { describe, expect, it } from "vitest";
import { applyMcpOrgDefaults, MCP_PRESETS } from "./mcpPresets";

describe("MCP_PRESETS", () => {
  it("every preset has connection type stdio or http", () => {
    for (const p of MCP_PRESETS) {
      expect(p.specYaml).toMatch(/connection:\s*\n\s*type:\s*(stdio|http)/);
      expect(p.defaultName.trim().length).toBeGreaterThan(0);
    }
  });
});

describe("applyMcpOrgDefaults", () => {
  it("appends timeout and sticky when missing", () => {
    const merged = applyMcpOrgDefaults(
      "connection:\n  type: http\n  url: http://x",
      { default_timeout_ms: 45000, default_sticky_http: false },
    );
    expect(merged).toContain("timeout_ms: 45000");
    expect(merged).toContain("sticky_http: false");
  });

  it("does not overwrite existing timeout_ms", () => {
    const merged = applyMcpOrgDefaults(
      "connection:\n  type: http\n  url: http://x\ntimeout_ms: 120000",
      { default_timeout_ms: 45000, default_sticky_http: true },
    );
    expect(merged).not.toContain("timeout_ms: 45000");
    expect(merged).toContain("timeout_ms: 120000");
  });
});
