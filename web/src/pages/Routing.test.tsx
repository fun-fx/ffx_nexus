import { describe, expect, it, beforeEach, afterEach, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import axe from "axe-core";
import { ThemeProvider } from "../theme/ThemeProvider";
import { Routing } from "../pages/Routing";

function WithProviders({ children }: { children: React.ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return (
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={["/routing"]}>{children}</MemoryRouter>
      </QueryClientProvider>
    </ThemeProvider>
  );
}

beforeEach(() => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    if (url.endsWith("/api/routing")) {
      return new Response(
        JSON.stringify([
          {
            model: "gemini-2.5-pro",
            samples: 1000,
            eff_quality: 0.92,
            quality: 0.93,
            safety_pass_rate: 0.99,
            avg_latency_ms: 600,
            avg_cost_usd: 0.012,
          },
          {
            model: "gpt-4o-mini",
            samples: 800,
            eff_quality: 0.7,
            quality: 0.7,
            safety_pass_rate: 0.95,
            avg_latency_ms: 400,
            avg_cost_usd: 0.0004,
          },
        ]),
        { headers: { "content-type": "application/json" } },
      );
    }
    if (url.endsWith("/api/eval/config")) {
      return new Response(
        JSON.stringify({
          eval_enabled: true,
          routing_enabled: true,
          score_store: "clickhouse",
          trace_store: "clickhouse",
          score_persisted: true,
          routing_stats_store: "clickhouse",
          eval: {
            pii_enabled: true,
            completeness_enabled: true,
            sample_rate: 1,
            workers: 1,
            judge: {
              enabled: false,
              base_url: "",
              model: "",
              api_key_set: false,
            },
            remote: { enabled: false, url: "", metrics: [], timeout: "" },
          },
          routing: {
            weights: { quality: 0.6, cost: 0.2, latency: 0.2 },
            window: "1h",
            refresh: "5m",
            groups: { fast: ["gpt-4o-mini"], smart: ["gemini-2.5-pro"] },
            groups_spec: "NEXUS_ROUTE_GROUPS",
            load_balance: true,
          },
          restart_required: [],
        }),
        { headers: { "content-type": "application/json" } },
      );
    }
    if (url.endsWith("/api/auth/me")) {
      return new Response(JSON.stringify({}), { status: 401 });
    }
    return new Response(JSON.stringify({}), { status: 404 });
  });
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("Routing page", () => {
  it("renders alias tier cards", async () => {
    render(
      <WithProviders>
        <Routing />
      </WithProviders>,
    );
    await waitFor(() => {
      expect(screen.getByText("auto")).toBeInTheDocument();
      expect(screen.getByText("fast")).toBeInTheDocument();
      expect(screen.getByText("smart")).toBeInTheDocument();
    });
  });

  it("renders a ranked candidate row", async () => {
    render(
      <WithProviders>
        <Routing />
      </WithProviders>,
    );
    await waitFor(() => {
      // model name appears in both the candidate table and the "top
      // models" tier card. Use getAllByText to bypass the strict-mode
      // duplicate guard.
      expect(screen.getAllByText(/gemini-2\.5-pro/).length).toBeGreaterThan(0);
      expect(screen.getAllByText(/gpt-4o-mini/).length).toBeGreaterThan(0);
    });
  });

  it("no axe violations", async () => {
    const { container } = render(
      <WithProviders>
        <Routing />
      </WithProviders>,
    );
    await waitFor(() => screen.getByText("auto"));
    const violations = await axe.run(container, {
      rules: { "color-contrast": { enabled: false } },
    });
    expect(violations.violations).toEqual([]);
  });
});
