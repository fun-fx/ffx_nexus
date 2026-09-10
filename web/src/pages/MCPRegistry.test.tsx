/* @vitest-environment jsdom */
import "@testing-library/jest-dom";
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
        if (url.endsWith("/api/mcp/settings")) {
          return new Response(
            JSON.stringify({
              org_id: "default",
              default_timeout_ms: 60000,
              default_sticky_http: true,
              gateway_base_url: "http://127.0.0.1:8080",
              oauth_sessions_enabled: false,
              mcp_routes: {
                list_servers: "GET /v1/mcp/servers",
                list_tools: "POST /v1/mcp/servers/{id}/tools/list",
                call_tool: "POST /v1/mcp/servers/{id}/tools/call",
              },
            }),
            { status: 200 },
          );
        }
        return new Response("{}", { status: 404 });
      }),
    );
  });

  it("renders empty state with Library CTA", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <MCPRegistry />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(await screen.findByRole("heading", { name: /mcp registry/i })).toBeInTheDocument();
    expect(await screen.findByText(/no mcp servers yet/i)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /browse library/i })).toHaveAttribute("href", "/mcp/library");
  });
});
