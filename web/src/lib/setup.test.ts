import { describe, expect, it } from "vitest";
import { buildSetupSteps, gatewayBase } from "./setup";
import type { AuthConfig, Credential, VirtualKey } from "../api";

const emptyCreds: Credential[] = [];
const emptyKeys: VirtualKey[] = [];

describe("gatewayBase", () => {
  it("prefers the explicit public gateway URL", () => {
    expect(
      gatewayBase({
        signup_enabled: false,
        sso_enabled: false,
        sso_label: "",
        gateway_url: "https://api.nexus.ffx.ai/",
      }),
    ).toBe("https://api.nexus.ffx.ai");
  });

  it("uses the console hostname on port 8080 in local mode when no public URL is set", () => {
    const base = gatewayBase({
      signup_enabled: false,
      sso_enabled: false,
      sso_label: "",
      local_mode: true,
    });
    expect(base).toMatch(/:8080$/);
  });
});

describe("buildSetupSteps", () => {
  const hosted: AuthConfig = {
    signup_enabled: false,
    sso_enabled: false,
    sso_label: "",
    key_mode: "strict_byok",
  };

  it("marks security steps done and provider steps open on a fresh cluster", () => {
    const steps = buildSetupSteps({
      cfg: hosted,
      credentials: emptyCreds,
      keys: emptyKeys,
    });
    expect(steps.find((s) => s.id === "cors")?.done).toBe(true);
    expect(steps.find((s) => s.id === "dashboard_auth")?.done).toBe(true);
    expect(steps.find((s) => s.id === "inference_auth")?.done).toBe(true);
    expect(steps.find((s) => s.id === "provider_key")?.done).toBe(false);
    expect(steps.find((s) => s.id === "virtual_key")?.done).toBe(false);
  });

  it("flags shared key mode as unfinished inference auth", () => {
    const steps = buildSetupSteps({
      cfg: { ...hosted, key_mode: "shared" },
      credentials: emptyCreds,
      keys: emptyKeys,
    });
    expect(steps.find((s) => s.id === "inference_auth")?.done).toBe(false);
  });

  it("completes provider steps once a credential and live key exist", () => {
    const steps = buildSetupSteps({
      cfg: hosted,
      credentials: [
        {
          id: "c1",
          provider: "openai",
          name: "prod",
          secret_last4: "abcd",
          enabled: true,
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
      keys: [
        {
          id: "k1",
          name: "ci",
          key_prefix: "nxs_live_",
          key_last4: "wxyz",
          allowed_models: [],
          rpm_limit: 0,
          monthly_budget_usd: 0,
          min_quality_score: 0,
          revoked: false,
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
    });
    expect(steps.every((s) => s.done)).toBe(true);
  });
});
