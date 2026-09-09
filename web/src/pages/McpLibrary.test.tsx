/* @vitest-environment jsdom */
import "@testing-library/jest-dom";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { ThemeProvider } from "../theme/ThemeProvider";
import { vi } from "vitest";
import { McpLibrary } from "./McpLibrary";

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
  default_timeout_ms: 45000,
  default_sticky_http: false,
  gateway_base_url: "http://127.0.0.1:8080",
  oauth_sessions_enabled: false,
  mcp_routes: {
    list_servers: "GET /v1/mcp/servers",
    list_tools: "POST /v1/mcp/servers/{id}/tools/list",
    call_tool: "POST /v1/mcp/servers/{id}/tools/call",
  },
};

describe("<McpLibrary />", () => {
  it("renders preset tiles", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url.endsWith("/api/mcp/servers")) {
          return new Response(JSON.stringify({ servers: [], statuses: [] }), { status: 200 });
        }
        if (url.endsWith("/api/mcp/settings")) {
          return new Response(JSON.stringify(settingsPayload), { status: 200 });
        }
        return new Response("{}", { status: 404 });
      }),
    );
    wrap(<McpLibrary />);
    expect(await screen.findByTestId("mcp-library")).toBeInTheDocument();
    expect(screen.getByTestId("mcp-preset-filesystem")).toBeInTheDocument();
    expect(screen.getByTestId("mcp-preset-github")).toBeInTheDocument();
  });

  it("installs preset with org defaults merged", async () => {
    const posts: unknown[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init?: RequestInit) => {
        if (url.endsWith("/api/mcp/servers") && init?.method === "POST") {
          posts.push(JSON.parse(String(init.body)));
          return new Response(
            JSON.stringify({ id: "srv-1", name: "filesystem", spec_yaml: "", enabled: true }),
            { status: 201 },
          );
        }
        if (url.endsWith("/api/mcp/servers/srv-1/test") && init?.method === "POST") {
          return new Response(JSON.stringify({ ok: true, message: "ok" }), { status: 200 });
        }
        if (url.endsWith("/api/mcp/servers")) {
          return new Response(JSON.stringify({ servers: [], statuses: [] }), { status: 200 });
        }
        if (url.endsWith("/api/mcp/settings")) {
          return new Response(JSON.stringify(settingsPayload), { status: 200 });
        }
        return new Response("{}", { status: 404 });
      }),
    );

    const user = userEvent.setup();
    wrap(<McpLibrary />);
    await screen.findByTestId("mcp-preset-filesystem");
    await user.click(screen.getByTestId("mcp-preset-filesystem"));
    const drawer = await screen.findByRole("dialog");
    await user.click(within(drawer).getByRole("button", { name: /install/i }));

    await waitFor(() => expect(posts).toHaveLength(1));
    const body = posts[0] as { name: string; spec_yaml: string; enabled: boolean };
    expect(body.name).toBe("filesystem");
    expect(body.enabled).toBe(true);
    expect(body.spec_yaml).toContain("timeout_ms: 60000");
    expect(body.spec_yaml).toContain("sticky_http: false");
  });

  it("blocks install when server name already exists", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url.endsWith("/api/mcp/servers")) {
          return new Response(
            JSON.stringify({
              servers: [{ id: "1", name: "filesystem", spec_yaml: "", enabled: true }],
              statuses: [],
            }),
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
    wrap(<McpLibrary />);
    await screen.findByTestId("mcp-preset-filesystem");
    await user.click(screen.getByTestId("mcp-preset-filesystem"));
    expect(await screen.findByText(/already exists/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /install/i })).toBeDisabled();
  });
});
