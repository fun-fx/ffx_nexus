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

afterEach(() => vi.unstubAllGlobals());

describe("<Overview /> hero CTAs", () => {
  it("'View Traces' is a Link that navigates to /traces", async () => {
    renderOverview();
    const link = await screen.findByRole("link", { name: /view traces/i });
    expect(link).toBeInTheDocument();
    expect(link.getAttribute("href")).toBe("/traces");
  });

  it("'Open Playground' is a Link that navigates to /playground", async () => {
    renderOverview();
    const link = await screen.findByRole("link", { name: /open playground/i });
    expect(link.getAttribute("href")).toBe("/playground");
  });
});
