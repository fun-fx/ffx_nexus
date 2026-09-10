import { describe, expect, it, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { SetupChecklist } from "./SetupChecklist";
import type { AuthConfig, Credential, VirtualKey } from "../api";

const hosted: AuthConfig = {
  signup_enabled: false,
  sso_enabled: true,
  sso_label: "Keycloak",
  key_mode: "strict_byok",
  cors_configured: true,
  gateway_url: "https://api.ffx.ai",
};

const credential: Credential = {
  id: "c1",
  provider: "openai",
  name: "prod",
  secret_last4: "abcd",
  enabled: true,
  created_at: "2026-01-01T00:00:00Z",
};

const key: VirtualKey = {
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
};

afterEach(() => {
  window.localStorage.removeItem("nexus:setup-checklist:dismissed");
  window.localStorage.removeItem("nexus:setup-checklist:dismissed:u1");
});

describe("<SetupChecklist />", () => {
  it("stays visible when every step is already complete", () => {
    render(
      <MemoryRouter>
        <SetupChecklist
          cfg={hosted}
          credentials={[credential]}
          keys={[key]}
          userId="u1"
        />
      </MemoryRouter>,
    );
    expect(screen.getByTestId("setup-checklist")).toBeInTheDocument();
    expect(
      screen.getByText(/all steps complete/i),
    ).toBeInTheDocument();
  });

  it("hides after Remind me later is clicked", async () => {
    const { user } = await import("@testing-library/user-event").then((m) => ({
      user: m.default.setup(),
    }));
    render(
      <MemoryRouter>
        <SetupChecklist cfg={hosted} credentials={[]} keys={[]} userId="u1" />
      </MemoryRouter>,
    );
    await user.click(screen.getByRole("button", { name: /remind me later/i }));
    expect(screen.queryByTestId("setup-checklist")).not.toBeInTheDocument();
    expect(
      window.localStorage.getItem("nexus:setup-checklist:dismissed:u1"),
    ).toBe("1");
  });
});
