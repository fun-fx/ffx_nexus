import { describe, expect, it, vi, afterEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { ThemeProvider } from "../theme/ThemeProvider";
import { Observability } from "../pages/Observability";

const adminMe = {
  id: "u1",
  email: "admin@nexus.local",
  role: "admin" as const,
  org_id: "o1",
};

function renderPage() {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.endsWith("/api/me")) {
        return new Response(JSON.stringify(adminMe), { status: 200 });
      }
      if (url.endsWith("/api/ui/observability")) {
        return new Response(
          JSON.stringify({
            otlp: { enabled: false },
            prometheus: { enabled: true, listen: ":9100", path: "/metrics" },
            metabase: { configured: false },
            traces: { clickhouse: false },
            local_mode: true,
          }),
          { status: 200 },
        );
      }
      if (url.endsWith("/api/eval/plugins")) {
        return new Response(JSON.stringify({ plugins: [] }), { status: 200 });
      }
      return new Response("{}", { status: 200 });
    }),
  );
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={["/observability/connectors"]}>
          <Observability />
        </MemoryRouter>
      </QueryClientProvider>
    </ThemeProvider>,
  );
}

afterEach(() => vi.unstubAllGlobals());

describe("<Observability />", () => {
  it("lists gateway sinks and eval vendors", async () => {
    renderPage();
    expect(
      await screen.findByRole("heading", { name: /observability connectors/i }),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /opentelemetry/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /prometheus/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /langfuse/i })).toBeInTheDocument();
  });

  it("shows the Prometheus scrape URL in local mode when metrics are on", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /prometheus/i }));
    expect(await screen.findByText(/pull scrape/i)).toBeInTheDocument();
    expect(screen.getByText(/:9100\/metrics/)).toBeInTheDocument();
  });

  it("sends admins to Eval plugins for a vendor", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /langfuse/i }));
    const link = await screen.findByRole("link", { name: /open eval plugins/i });
    expect(link.getAttribute("href")).toBe("/eval/plugins");
  });
});
