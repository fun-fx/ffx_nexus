/* @vitest-environment jsdom */
import "@testing-library/jest-dom";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { ThemeProvider } from "../theme/ThemeProvider";
import { vi } from "vitest";
import { McpSettings } from "./McpSettings";

function makeClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 0, gcTime: 0 } },
  });
}

function wrap(node: React.ReactNode) {
  return render(
    <QueryClientProvider client={makeClient()}>
      <ThemeProvider>
        <MemoryRouter>{node}</MemoryRouter>
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

const settingsPayload = {
  org_id: "default",
  default_timeout_ms: 60000,
  default_sticky_http: true,
  gateway_base_url: "https://gw.example",
  oauth_sessions_enabled: false,
  mcp_routes: {
    list_servers: "GET /v1/mcp/servers",
    list_tools: "POST /v1/mcp/servers/{id}/tools/list",
    call_tool: "POST /v1/mcp/servers/{id}/tools/call",
  },
};

describe("<McpSettings />", () => {
  it("renders gateway reference snippet", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url.endsWith("/api/mcp/settings")) {
          return new Response(JSON.stringify(settingsPayload), { status: 200 });
        }
        return new Response("{}", { status: 404 });
      }),
    );
    wrap(<McpSettings />);
    await waitFor(() => {
      expect(screen.getByText("GET /v1/mcp/servers")).toBeInTheDocument();
    });
    expect(screen.getAllByText("https://gw.example").length).toBeGreaterThan(0);
  });

  it("PATCHes org defaults on save", async () => {
    const patches: unknown[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init?: RequestInit) => {
        if (url.endsWith("/api/mcp/settings") && init?.method === "PATCH") {
          patches.push(JSON.parse(String(init.body)));
          return new Response(
            JSON.stringify({ ...settingsPayload, default_timeout_ms: 90000 }),
            { status: 200 },
          );
        }
        if (url.endsWith("/api/mcp/settings")) {
          return new Response(JSON.stringify(settingsPayload), { status: 200 });
        }
        return new Response("{}", { status: 404 });
      }),
    );

    const user = userEvent.setup();
    wrap(<McpSettings />);
    const input = await screen.findByLabelText(/default timeout/i);
    await user.clear(input);
    await user.type(input, "90000");
    await user.click(screen.getByRole("button", { name: /save defaults/i }));

    await waitFor(() => expect(patches).toHaveLength(1));
    expect(patches[0]).toEqual({
      default_timeout_ms: 90000,
      default_sticky_http: true,
    });
  });

  it("shows OAuth stub without save action", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url.endsWith("/api/mcp/settings")) {
          return new Response(JSON.stringify(settingsPayload), { status: 200 });
        }
        return new Response("{}", { status: 404 });
      }),
    );
    wrap(<McpSettings />);
    expect(await screen.findByText(/OAuth & sessions/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /save oauth/i })).not.toBeInTheDocument();
  });
});
