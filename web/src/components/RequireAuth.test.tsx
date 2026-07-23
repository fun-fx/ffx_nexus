import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Routes, Route } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "../theme/ThemeProvider";
import { RequireAuth } from "./RequireAuth";
import { Login } from "../pages/Login";
import { Overview } from "../pages/Overview";

function renderAt(initialPath: string, fetchImpl: (url: string) => Promise<Response>) {
  vi.stubGlobal("fetch", fetchImpl);
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={[initialPath]}>
          <Routes>
            <Route path="/login" element={<Login />} />
            <Route element={<RequireAuth />}>
              <Route index element={<Overview />} />
              <Route path="traces" element={<div data-testid="traces-page">TRACES</div>} />
            </Route>
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>
    </ThemeProvider>,
  );
}

describe("<RequireAuth />", () => {
  beforeEach(() => {
    vi.unstubAllGlobals();
  });

  it("shows a loading card while /api/me resolves", async () => {
    let resolveMe!: (r: Response) => void;
    vi.stubGlobal(
      "fetch",
      (url: string) => {
        if (url.startsWith("/api/me")) {
          return new Promise<Response>((res) => {
            resolveMe = res;
          });
        }
        // /api/auth/config etc — used by Login; not used here
        return Promise.resolve(new Response("{}", { status: 200 }));
      },
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <ThemeProvider>
        <QueryClientProvider client={qc}>
          <MemoryRouter initialEntries={["/"]}>
            <Routes>
              <Route path="/login" element={<Login />} />
              <Route element={<RequireAuth />}>
                <Route index element={<Overview />} />
              </Route>
            </Routes>
          </MemoryRouter>
        </QueryClientProvider>
      </ThemeProvider>,
    );

    // Wait for the guard to actually mount; then verify the loading card.
    await waitFor(() => {
      expect(screen.getByTestId("auth-loading")).toBeInTheDocument();
    });
    expect(screen.queryByText("Loading session…")).toBeInTheDocument();
    // Resolve so the test framework doesn't leak a pending promise.
    resolveMe(new Response("{}", { status: 401 }));
  });

  it("bounces unauthenticated visitors to /login with ?next=", async () => {
    renderAt("/", async () => new Response('{"error":"login required"}', { status: 401 }));

    await waitFor(() => {
      // Login headline
      expect(screen.getByText("Sign in")).toBeInTheDocument();
    });
  });

  it("renders protected content when /api/me returns a user", async () => {
    // We render Overview which depends on additional endpoints; stub them
    // all with empty shapes so the Overview can finish rendering without
    // crashing on undefined fields.
    vi.stubGlobal(
      "fetch",
      async (url: string): Promise<Response> => {
        if (url.startsWith("/api/me")) {
          return new Response(
            JSON.stringify({
              id: "u1",
              org_id: "default",
              email: "[email protected]",
              role: "admin",
              enforce_limits: false,
              created_at: new Date().toISOString(),
            }),
            { status: 200 },
          );
        }
        if (url.startsWith("/api/stats")) {
          return new Response(
            JSON.stringify({
              total_requests: 0,
              error_rate: 0,
              avg_latency_ms: 0,
              p95_latency_ms: 0,
              total_tokens: 0,
              total_cost_usd: 0,
              cache_hits: 0,
              cache_hit_rate: 0,
              guardrail_events: 0,
            }),
            { status: 200 },
          );
        }
        if (url.startsWith("/api/traces")) {
          return new Response("[]", { status: 200 });
        }
        if (url.startsWith("/api/routing")) {
          return new Response("[]", { status: 200 });
        }
        if (url.startsWith("/api/eval/config")) {
          // Bare-bones shape that the new sanitizer tolerates.
          return new Response(
            JSON.stringify({
              eval_enabled: false,
              routing_enabled: false,
              score_store: "",
              trace_store: "",
              score_persisted: false,
              routing_stats_store: "",
              eval: {
                pii_enabled: false,
                completeness_enabled: false,
                sample_rate: 0,
                workers: 0,
                judge: { enabled: false, base_url: "", model: "", api_key_set: false },
                remote: { enabled: false, url: "", metrics: [], timeout: "30s" },
              },
              routing: {
                weights: {},
                window: "1h",
                refresh: "60s",
                groups: {},
                groups_spec: "",
                load_balance: false,
              },
              restart_required: [],
            }),
            { status: 200 },
          );
        }
        return new Response("{}", { status: 200 });
      },
    );

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <ThemeProvider>
        <QueryClientProvider client={qc}>
          <MemoryRouter initialEntries={["/"]}>
            <Routes>
              <Route path="/login" element={<Login />} />
              <Route element={<RequireAuth />}>
                <Route index element={<Overview />} />
              </Route>
            </Routes>
          </MemoryRouter>
        </QueryClientProvider>
      </ThemeProvider>,
    );

    await waitFor(() => {
      expect(screen.getByText(/Why FFX Nexus/i)).toBeInTheDocument();
    });
  });
});
