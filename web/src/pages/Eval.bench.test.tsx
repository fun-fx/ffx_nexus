import { describe, expect, it, beforeEach, afterEach, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "../theme/ThemeProvider";
import { Eval } from "../pages/Eval";

// The Eval page wires the bench blend visibility into its page-head;
// add a tiny dedicated suite so the assertions live next to the
// configuration model in api.ts rather than getting tangled in the
// 800+ lines of Eval.test.tsx that already exist. The =page= constant
// fetch URL is the same one Routing.test.tsx uses, which keeps
// operators reading these suites finding the same shape from both
// directions.

function WithProviders({ children }: { children: React.ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return (
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={["/eval"]}>{children}</MemoryRouter>
      </QueryClientProvider>
    </ThemeProvider>
  );
}

const baseEval = {
  pii_enabled: true,
  completeness_enabled: true,
  sample_rate: 1,
  workers: 1,
  judge: { enabled: false, base_url: "", model: "", api_key_set: false },
  remote: { enabled: false, url: "", metrics: [], timeout: "" },
};

function snapshotRouting(bench: {
  enabled: boolean;
  weight: number;
  decay: string;
}) {
  return {
    weights: { quality: 0.6, cost: 0.2, latency: 0.2 },
    window: "1h",
    refresh: "5m",
    groups: { fast: ["gpt-4o-mini"] },
    groups_spec: "NEXUS_ROUTE_GROUPS",
    load_balance: true,
    bench_enabled: bench.enabled,
    bench_weight: bench.weight,
    bench_decay: bench.decay,
  };
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("Eval page bench blend visibility", () => {
  beforeEach(() => {
    // Default mock: bench is active. The off-state suite below
    // overwrites only the bench_* fields, leaving the rest of
    // the surface intact.
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.endsWith("/api/eval/config")) {
        return new Response(
          JSON.stringify({
            eval_enabled: true,
            routing_enabled: true,
            score_store: "postgres",
            trace_store: "clickhouse",
            score_persisted: true,
            routing_stats_store: "postgres",
            eval: baseEval,
            routing: snapshotRouting({ enabled: true, weight: 0.5, decay: "168h" }),
            plugin_only: true,
            purge_legacy_profiles_on_boot: false,
            restart_required: [],
          }),
          { headers: { "content-type": "application/json" } },
        );
      }
      if (url.endsWith("/api/me")) {
        return new Response(
          JSON.stringify({
            id: "u1",
            email: "admin@nexus.local",
            role: "admin",
            org_id: "o1",
          }),
          { status: 200 },
        );
      }
      if (url.endsWith("/api/auth/me")) {
        return new Response(JSON.stringify({}), { status: 401 });
      }
      if (url.endsWith("/api/eval/profiles")) {
        return new Response(JSON.stringify({ profiles: [] }), {
          headers: { "content-type": "application/json" },
        });
      }
      if (url.endsWith("/api/eval/plugins")) {
        return new Response(JSON.stringify([]), {
          headers: { "content-type": "application/json" },
        });
      }
      return new Response(JSON.stringify({}), { status: 404 });
    });
  });

  it("renders the active bench-blend row when bench_enabled is true", async () => {
    render(
      <WithProviders>
        <Eval />
      </WithProviders>,
    );
    await waitFor(() => {
      const row = screen.getByTestId("eval-bench-row");
      expect(row.textContent).toMatch(/bench blend active/);
      expect(row.textContent).toMatch(/50%/);
      expect(row.textContent).toMatch(/168h/);
    });
  });

  it("falls back to the inactive hint when bench_enabled is false", async () => {
    // Override the bench fields while keeping the rest of the
    // surface intact. We delete and re-spy because vi.spyOn on
    // the same global a second time stacks onto the first mock
    // rather than replacing it — vitest 3.x returns the most
    // recent mockImplementation, but the active-state branch is
    // left reachable. A fresh spy is the path of least surprise.
    vi.restoreAllMocks();
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.endsWith("/api/eval/config")) {
        return new Response(
          JSON.stringify({
            eval_enabled: true,
            routing_enabled: true,
            score_store: "clickhouse",
            trace_store: "clickhouse",
            score_persisted: true,
            routing_stats_store: "clickhouse",
            eval: baseEval,
            routing: snapshotRouting({ enabled: false, weight: 0, decay: "" }),
            plugin_only: true,
            purge_legacy_profiles_on_boot: false,
            restart_required: [],
          }),
          { headers: { "content-type": "application/json" } },
        );
      }
      if (url.endsWith("/api/me")) {
        return new Response(
          JSON.stringify({
            id: "u1",
            email: "admin@nexus.local",
            role: "admin",
            org_id: "o1",
          }),
          { status: 200 },
        );
      }
      if (url.endsWith("/api/eval/profiles")) {
        return new Response(JSON.stringify({ profiles: [] }), {
          headers: { "content-type": "application/json" },
        });
      }
      if (url.endsWith("/api/eval/plugins")) {
        return new Response(JSON.stringify([]), {
          headers: { "content-type": "application/json" },
        });
      }
      return new Response(JSON.stringify({}), { status: 404 });
    });

    render(
      <WithProviders>
        <Eval />
      </WithProviders>,
    );
    await waitFor(() => {
      const row = screen.getByTestId("eval-bench-row");
      expect(row.textContent).toMatch(/bench blend inactive/);
      expect(row.textContent).toMatch(/NEXUS_ROUTE_W_BENCH/);
    });
  });
});
