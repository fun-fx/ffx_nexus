import { describe, expect, it, vi, beforeEach } from "vitest";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { MCPLogs } from "./MCPLogs";

describe("MCPLogs", () => {
  beforeEach(() => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url.includes("/api/mcp-logs/stats")) {
          return new Response(
            JSON.stringify({
              total: 0,
              success: 0,
              errors: 0,
              success_rate: 0,
              avg_latency_ms: 0,
              p50_latency_ms: 0,
              p95_latency_ms: 0,
            }),
            { status: 200 },
          );
        }
        if (url.includes("/api/mcp-logs/filterdata")) {
          return new Response(JSON.stringify({ tool_names: [], server_labels: [], statuses: [] }), {
            status: 200,
          });
        }
        if (url.includes("/api/mcp-logs")) {
          return new Response(JSON.stringify({ items: [], next_cursor: { before: "", since: "" } }), {
            status: 200,
          });
        }
        return new Response("{}", { status: 404 });
      }),
    );
  });

  it("renders stats and empty table", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <MCPLogs />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(await screen.findByRole("heading", { name: /mcp logs/i })).toBeTruthy();
    expect(await screen.findByText(/no mcp logs yet/i)).toBeTruthy();
  });
});
