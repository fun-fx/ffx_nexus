import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { MemoryRouter } from "react-router-dom";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import {
  QueryClient,
  QueryClientProvider,
} from "@tanstack/react-query";
import { ThemeProvider } from "../theme/ThemeProvider";
import { PluginListCard } from "../pages/EvalPlugins";

// Two-row fixture: one manual-trigger plugin and one on-trace plugin.
// The "Run now" button must appear only on the manual row, and the
// POST must hit /api/eval/plugins/{name}/fire with a JSON body —
// never /fire/test or similar drift.
const manualPlugin = {
  id: "p-manual",
  name: "manual-judge",
  enabled: true,
  org_id: "o1",
  spec_yaml: [
    "type: webhook",
    "spec:",
    "  service:",
    "    kind: langfuse",
    "  send:",
    "    trigger: manual",
    "    sampling: 1.0",
].join("\n"),
};

const inlinePlugin = {
  id: "p-inline",
  name: "live-judge",
  enabled: true,
  org_id: "o1",
  spec_yaml: [
    "type: webhook",
    "spec:",
    "  send:",
    "    trigger: on_trace",
    "    sampling: 1.0",
].join("\n"),
};

function setup() {
  const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === "/api/eval/plugins") {
      return new Response(JSON.stringify({ plugins: [manualPlugin, inlinePlugin] }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    }
    if (url.startsWith("/api/eval/plugins/") && url.endsWith("/fire")) {
      // Decode body so the test can assert on the audit-tag flow.
      const body = init?.body ? JSON.parse(String(init.body)) : {};
      return new Response(
        JSON.stringify({
          ok: true,
          count: 3,
          message: `drained (trigger=${body?.trigger ?? "default"})`,
        }),
        { status: 200, headers: { "content-type": "application/json" } },
      );
    }
    return new Response("{}", { status: 200 });
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function renderList() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <MemoryRouter>
          <PluginListCard onCreate={() => {}} onEdit={() => {}} />
        </MemoryRouter>
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

describe("PluginListCard — manual trigger UX", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = setup();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("hides 'Run now' from non-manual plugins", async () => {
    renderList();
    await waitFor(() =>
      expect(screen.getByTestId("plugin-row-p-manual")).toBeTruthy(),
    );
    expect(screen.getByTestId("plugin-row-p-inline")).toBeTruthy();
    // Manual row carries the neon button:
    const manualBtn = screen.getByTestId("plugin-fire-manual-judge");
    expect(manualBtn).toBeTruthy();
    expect(manualBtn.textContent).toMatch(/Run now/);
    // Inline row does NOT have one — Query DOM by absence:
    expect(screen.queryByTestId("plugin-fire-live-judge")).toBeNull();
  });

  it("drains the buffer when 'Run now' is clicked", async () => {
    renderList();
    const btn = await waitFor(() =>
      screen.getByTestId("plugin-fire-manual-judge"),
    );
    fireEvent.click(btn);
    await waitFor(() => {
      const calls = fetchMock.mock.calls.filter(
        (c) => typeof c[0] === "string" && c[0].endsWith("/fire"),
      );
      expect(calls.length).toBeGreaterThan(0);
    });
    const [url, init] = fetchMock.mock.calls.find(
      (c) => typeof c[0] === "string" && c[0].endsWith("/fire"),
    )!;
    expect(url).toBe("/api/eval/plugins/manual-judge/fire");
    expect(init?.method).toBe("POST");
    // Body parses without throwing — server defaults the audit tag
    // when the operator passes no trigger.
    expect(() =>
      JSON.parse(String((init as RequestInit | undefined)?.body ?? "{}")),
    ).not.toThrow();
    // Success line prints on the row so the operator gets feedback
    // beyond the network round-trip.
    await waitFor(() =>
      expect(screen.getByTestId("plugin-fire-result-manual-judge")).toBeTruthy(),
    );
  });

  it("surfaces server errors on the row", async () => {
    vi.unstubAllGlobals();
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url === "/api/eval/plugins") {
          return new Response(JSON.stringify({ plugins: [manualPlugin] }), {
            status: 200,
          });
        }
        if (url.endsWith("/fire")) {
          return new Response(
            JSON.stringify({ ok: false, message: "plugin not found" }),
            { status: 404 },
          );
        }
        return new Response("{}", { status: 200 });
      }),
    );
    renderList();
    const btn = await waitFor(() =>
      screen.getByTestId("plugin-fire-manual-judge"),
    );
    fireEvent.click(btn);
    await waitFor(() => {
      const node = screen.getByTestId("plugin-fire-result-manual-judge");
      expect(node.textContent).toMatch(/plugin not found/);
    });
  });
});
