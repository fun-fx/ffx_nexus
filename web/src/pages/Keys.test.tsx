import { describe, expect, it, beforeEach, afterEach, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "../theme/ThemeProvider";
import { Keys } from "../pages/Keys";

function WithProviders({ children }: { children: React.ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return (
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={["/keys"]}>{children}</MemoryRouter>
      </QueryClientProvider>
    </ThemeProvider>
  );
}

beforeEach(() => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    if (url.endsWith("/api/auth/me")) {
      return new Response(
        JSON.stringify({
          id: "u1",
          org_id: "default",
          email: "r@nexus.ai",
          role: "admin",
          enforce_limits: false,
          created_at: "2026-07-01T00:00:00Z",
        }),
        { headers: { "content-type": "application/json" } },
      );
    }
    if (url.endsWith("/api/me/keys")) {
      return new Response(
        JSON.stringify([
          {
            id: "k1",
            name: "cb-playground",
            key_prefix: "nxs_live_a1b2",
            key_last4: "a1b2",
            allowed_models: [],
            rpm_limit: 0,
            monthly_budget_usd: 0,
            min_quality_score: 0,
            revoked: false,
            created_at: "2026-07-15T00:00:00Z",
          },
        ]),
        { headers: { "content-type": "application/json" } },
      );
    }
    return new Response(JSON.stringify({}), { status: 404 });
  });
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("Keys page", () => {
  it("renders the keys table with one row", async () => {
    render(
      <WithProviders>
        <Keys />
      </WithProviders>,
    );
    await waitFor(() => {
      expect(screen.getByText("cb-playground")).toBeInTheDocument();
    });
  });

  it("shows the New key button", async () => {
    render(
      <WithProviders>
        <Keys />
      </WithProviders>,
    );
    expect(screen.getByRole("button", { name: /new key/i })).toBeInTheDocument();
  });
});
