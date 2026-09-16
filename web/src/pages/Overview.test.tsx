import { describe, expect, it, vi, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { ThemeProvider } from "../theme/ThemeProvider";
import { Overview } from "../pages/Overview";

const adminMe = {
  id: "u1",
  email: "admin@nexus.local",
  role: "admin" as const,
  org_id: "o1",
};

function renderOverview() {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.endsWith("/api/me")) {
        return new Response(JSON.stringify(adminMe), { status: 200 });
      }
      // Stats / eval config / traces / etc. all return zero-filled shapes so
      // the page renders without throwing on the index Dashboard layout.
      const empty = "{}";
      return new Response(empty, { status: 200 });
    }),
  );
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={["/"]}>
          <Overview />
        </MemoryRouter>
      </QueryClientProvider>
    </ThemeProvider>,
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  window.localStorage.removeItem("nexus:setup-checklist:dismissed");
  window.localStorage.removeItem("nexus:setup-checklist:dismissed:u1");
});

describe("<Overview /> hero CTAs", () => {
  it("'View Traces' is a Link that navigates to /observability/traces", async () => {
    renderOverview();
    const link = await screen.findByRole("link", { name: /view traces/i });
    expect(link).toBeInTheDocument();
    expect(link.getAttribute("href")).toBe("/observability/traces");
  });

  it("'Open Playground' is a Link that navigates to /develop/playground", async () => {
    renderOverview();
    const link = await screen.findByRole("link", { name: /open playground/i });
    expect(link.getAttribute("href")).toBe("/develop/playground");
  });
});

describe("<Overview /> first-run empty states", () => {
  it("shows the setup checklist and first-request snippets when nothing is configured", async () => {
    renderOverview();
    expect(await screen.findByTestId("setup-checklist")).toBeInTheDocument();
    expect(await screen.findByRole("heading", { name: /setup checklist/i })).toBeInTheDocument();
    expect(await screen.findByTestId("first-request-snippets")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: /no traffic yet/i })).toBeInTheDocument();
  });
});

describe("<Overview /> spend by provider window", () => {
  it("requests a 30-day provider rollup and keeps live stats on 1h", async () => {
    renderOverview();
    await screen.findByRole("heading", { name: /spend by provider/i });
    const fetchMock = fetch as unknown as ReturnType<typeof vi.fn>;
    const urls = fetchMock.mock.calls.map((c) => String(c[0]));
    expect(urls.some((u) => u.includes("/api/stats/providers?window=30d"))).toBe(
      true,
    );
    expect(urls.some((u) => u.includes("/api/stats?window=1h"))).toBe(true);
    expect(
      await screen.findByText(/no spend in the last 30 days/i),
    ).toBeInTheDocument();
  });
});

describe("<Overview /> pricing drift banner", () => {
  it("surfaces published-rate drift without implying billing changed", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === "string" ? input : input.toString();
        if (url.endsWith("/api/me")) {
          return new Response(JSON.stringify(adminMe), { status: 200 });
        }
        if (url.endsWith("/api/stats/pricing-drift")) {
          return new Response(
            JSON.stringify({
              enabled: true,
              billing_unchanged: true,
              drifts: [
                {
                  model: "gpt-4o-mini",
                  ours_in_per_m: 0.15,
                  ours_out_per_m: 0.6,
                  catalog_in_per_m: 1.5,
                  catalog_out_per_m: 0.6,
                },
              ],
            }),
            { status: 200 },
          );
        }
        return new Response("{}", { status: 200 });
      }),
    );
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <ThemeProvider>
        <QueryClientProvider client={qc}>
          <MemoryRouter initialEntries={["/"]}>
            <Overview />
          </MemoryRouter>
        </QueryClientProvider>
      </ThemeProvider>,
    );
    expect(await screen.findByTestId("pricing-drift-banner")).toBeInTheDocument();
    expect(screen.getByText(/static table/i)).toBeInTheDocument();
  });
});
