import { describe, expect, it } from "vitest";
import { applyMcpOrgDefaults, MCP_PRESETS } from "./mcpPresets";

describe("MCP_PRESETS", () => {
  it("every preset has connection type stdio or http and a runtime", () => {
    for (const p of MCP_PRESETS) {
      expect(p.specYaml).toMatch(/connection:\s*\n\s*type:\s*(stdio|http)/);
      expect(p.defaultName.trim().length).toBeGreaterThan(0);
      expect(p.runtime === "local" || p.runtime === "hosted").toBe(true);
    }
  });

  it("local presets are stdio and hosted presets are http", () => {
    for (const p of MCP_PRESETS) {
      if (p.runtime === "local") {
        expect(p.specYaml).toMatch(/type:\s*stdio/);
      } else {
        expect(p.specYaml).toMatch(/type:\s*http/);
      }
    }
  });

  it("GitHub is hosted HTTP against the GitHub MCP endpoint", () => {
    const github = MCP_PRESETS.find((p) => p.id === "github");
    expect(github?.runtime).toBe("hosted");
    expect(github?.specYaml).toMatch(/type:\s*http/);
    expect(github?.specYaml).toContain("https://api.githubcopilot.com/mcp/");
    expect(github?.specYaml).not.toMatch(/command:\s*npx/);
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
