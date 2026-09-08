import { describe, expect, it, vi, beforeEach } from "vitest";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { MCPRegistry } from "./MCPRegistry";

describe("MCPRegistry", () => {
  beforeEach(() => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url.endsWith("/api/mcp/servers")) {
          return new Response(JSON.stringify({ servers: [], statuses: [] }), { status: 200 });
        }
        return new Response("{}", { status: 404 });
      }),
    );
  });

  it("renders empty state", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <MCPRegistry />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(await screen.findByRole("heading", { name: /mcp registry/i })).toBeTruthy();
    expect(await screen.findByText(/no mcp servers yet/i)).toBeTruthy();
  });
});
