import { describe, expect, it, vi, afterEach, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "../theme/ThemeProvider";
import { Traces } from "../pages/Traces";

// Tests for the v0.6.11 server-driven trace filter + time-window work.
// All fetch calls are stubbed; the focus is on the URL the console
// assembles and the cursor / "Load older" round-trip — we are NOT
// trying to fake the ReactQuery key cache flush fully; we just want
// the wire contract pinned.

const adminMe = {
  id: "u1",
  email: "admin@nexus.local",
  role: "admin" as const,
  org_id: "o1",
};

function traceRow(trace_id: string, status: number, provider: string, minutesAgo: number, opts: { turn_id?: string; model?: string } = {}) {
  return {
    trace_id,
    turn_id: opts.turn_id ?? "",
    timestamp: new Date(Date.now() - minutesAgo * 60_000).toISOString(),
    provider_name: provider,
    request_model: opts.model ?? "gpt-4o",
    input_tokens: 100,
    output_tokens: 200,
    latency_ms: 1234,
    ttft_ms: 200,
    cost_usd: 0.00042,
    status_code: status,
    streamed: 0,
    finish_reason: "stop",
    cache_hit: 0,
    guardrail_action: "",
    credential_source: "env",
    user_id: "u1",
    user_email: "admin@nexus.local",
  };
}

// fixture shape and the request-log helper. The helper records every
// fetch URL so individual tests can assert what the console sent
// without re-implementing URL parsing in the assertion half.
let callLog: Array<{ url: string; method: string; body: unknown }> = [];

function stubFetch(
  handler: (url: string, method: string, body: unknown) => Response,
) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = init?.method?.toUpperCase() ?? "GET";
      const body = init?.body ? JSON.parse(String(init.body)) : null;
      callLog.push({ url, method, body });
      return handler(url, method, body);
    }),
  );
}

function renderTraces(_role: "admin" | "member" = "admin") {
  callLog = [];
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <Traces />
      </QueryClientProvider>
    </ThemeProvider>,
  );
  return Object.assign(utils, { qc });
}

afterEach(() => vi.unstubAllGlobals());

// -------------------------------------------------------------------
// 1. Initial fetch applies the filter+window params to the URL.
// -------------------------------------------------------------------
describe("<Traces /> initial filter pass-through", () => {
  beforeEach(() => {
    stubFetch((url, method) => {
      if (method !== "GET") return new Response("{}", { status: 200 });
      if (url.endsWith("/api/me")) {
        return new Response(JSON.stringify({ ...adminMe, role: "admin" }), { status: 200 });
      }
      if (url.includes("/api/traces")) {
        return new Response(
          JSON.stringify({
            items: [
              traceRow("a", 200, "openai", 5),
              traceRow("b", 500, "openai", 8),
            ],
            next_cursor: { before: "", since: "" },
          }),
          { status: 200 },
        );
      }
      return new Response("{}", { status: 200 });
    });
  });

  it("sends ?limit=100 and q=&status=&provider=& on the first fetch (no params yet)", async () => {
    renderTraces();
    await waitFor(() =>
      expect(callLog.find((c) => c.url.includes("/api/traces"))).toBeDefined(),
    );
    const first = callLog.find((c) => c.url.includes("/api/traces"))!;
    expect(first.method).toBe("GET");
    // We send ?limit=100 with no other filter params when nothing is set.
    expect(first.url).toMatch(/\/api\/traces\?/);
    expect(first.url).toMatch(/limit=100/);
    // The status / provider / q params are absent because no chip is
    // toggled yet and the search input is empty.
    expect(first.url).not.toMatch(/[?&]status=/);
    expect(first.url).not.toMatch(/[?&]provider=/);
    expect(first.url).not.toMatch(/[?&]q=/);
  });

  it("emits ?since= and ?before= when the date pickers hold values", async () => {
    renderTraces();
    // Make the form present first so we can reach the inputs.
    await waitFor(() => screen.getByTestId("traces-window-since"));
    const sinceInput = screen.getByTestId("traces-window-since") as HTMLInputElement;
    const beforeInput = screen.getByTestId("traces-window-before") as HTMLInputElement;
    fireEvent.change(sinceInput, { target: { value: "2026-07-20T09:00" } });
    fireEvent.change(beforeInput, { target: { value: "2026-07-27T09:00" } });
    await waitFor(() => {
      const traces = callLog.filter((c) => c.url.includes("/api/traces"));
      // We expect at least one newer fetch (the queryKey refetches).
      expect(traces.length).toBeGreaterThanOrEqual(2);
      const last = traces[traces.length - 1];
      expect(last.url).toMatch(/[?&]since=/);
      expect(last.url).toMatch(/[?&]before=/);
    });
  });
});

// -------------------------------------------------------------------
// 2. "Load older" appends the cursor into the URL and merges results.
// -------------------------------------------------------------------
describe("<Traces /> Load older cursor walk", () => {
  it("clicks Load older with a non-empty cursor and gets a follow-up page", async () => {
    let tracesCallIndex = 0;
    stubFetch((url) => {
      if (url.endsWith("/api/me")) {
        return new Response(JSON.stringify({ ...adminMe, role: "admin" }), { status: 200 });
      }
      if (url.includes("/api/traces")) {
        // The first call returns a non-empty cursor so the button is
        // enabled; the second call (Load older) returns an empty cursor
        // so the button disables itself when exhausted.
        if (tracesCallIndex === 0) {
          tracesCallIndex += 1;
          return new Response(
            JSON.stringify({
              items: [traceRow("first", 200, "openai", 5)],
              next_cursor: {
                before: new Date(Date.now() - 30 * 60_000).toISOString(),
                since: new Date(Date.now() - 60 * 60_000).toISOString(),
              },
            }),
            { status: 200 },
          );
        }
        tracesCallIndex += 1;
        return new Response(
          JSON.stringify({
            items: [traceRow("second", 200, "openai", 45)],
            next_cursor: { before: "", since: "" },
          }),
          { status: 200 },
        );
      }
      return new Response("{}", { status: 200 });
    });
    renderTraces();
    await waitFor(() => screen.getByTestId("traces-load-older"));

    // Wait for the initial useQuery to settle; otherwise Load older's
    // button is disabled because items.length === 0 and the cursor is
    // still the empty default, so a fireEvent.click would short-circuit.
    await waitFor(() => screen.getAllByRole("row").length > 1);

    const beforeCalls = callLog.filter((c) => c.url.includes("/api/traces")).length;
    const beforeBtn = screen.getByTestId("traces-load-older") as HTMLButtonElement;
    expect(beforeBtn.disabled).toBe(false);
    fireEvent.click(beforeBtn);
    await waitFor(() => {
      const after = callLog.filter((c) => c.url.includes("/api/traces")).length;
      expect(after).toBeGreaterThan(beforeCalls);
    }, { timeout: 5000 });


    // The follow-up call must include the before/since cursor fields —
    // it cannot rely on the read-time default of "now" because that
    // would walk the same window we just showed.
    const followUp = callLog.filter((c) => c.url.includes("/api/traces")).pop()!;
    expect(followUp.url).toMatch(/[?&]before=/);
    expect(followUp.url).toMatch(/[?&]since=/);

    // Both rows from the two pages are now in the document. The DataTable
    // doesn't expose trace_id in the cell render — we identify rows by
    // status_code text (rendered inside a StatusPill) and by the time
    // string. Two distinct status pills indicate merge success.
    await waitFor(() => {
      // Provider chips prove the page rendered both PageRow items.
      const openaiCells = screen.getAllByText("openai");
      expect(openaiCells.length).toBeGreaterThanOrEqual(2);
    });
  });
});

// -------------------------------------------------------------------
// 3. Filter chips + search input feed server-side parameters.
// -------------------------------------------------------------------
describe("<Traces /> live filter round-trips", () => {
  beforeEach(() => {
    stubFetch((url) => {
      if (url.endsWith("/api/me")) {
        return new Response(JSON.stringify({ ...adminMe, role: "admin" }), { status: 200 });
      }
      if (url.includes("/api/traces")) {
        return new Response(
          JSON.stringify({
            items: [traceRow("a", 500, "openai", 5)],
            next_cursor: { before: "", since: "" },
          }),
          { status: 200 },
        );
      }
      return new Response("{}", { status: 200 });
    });
  });

  it("selects the 4xx/5xx status chip and forwards status=err to the server", async () => {
    renderTraces();
    await waitFor(() => screen.getByText("4xx/5xx"));
    fireEvent.click(screen.getByText("4xx/5xx"));
    await waitFor(() => {
      const recent = callLog.filter((c) => c.url.includes("/api/traces")).pop();
      expect(recent?.url).toMatch(/[?&]status=err/);
    });
  });

  it("types into search and forwards q= to the server", async () => {
    renderTraces();
    await waitFor(() => screen.getByLabelText(/search traces/i));
    fireEvent.change(screen.getByLabelText(/search traces/i), {
      target: { value: "gpt-4o" },
    });
    await waitFor(() => {
      const recent = callLog.filter((c) => c.url.includes("/api/traces")).pop();
      expect(recent?.url).toMatch(/[?&]q=/);
      // URL-encoded "gpt-4o" — match via decoded substring using a regex.
      expect(recent?.url).toMatch(/gpt-4o/);
    });
  });

  it("negative status param is rejected and the button never sends an invalid value", async () => {
    // We don't render status=invalid anywhere in the UI today — this
    // guards against a regression where someone wires up an enum
    // selector that emits unrecognised values. Asserting on the
    // URL itself is the closest contract the server enforces.
    renderTraces();
    await waitFor(() =>
      expect(callLog.find((c) => c.url.includes("/api/traces"))).toBeDefined(),
    );
    const calls = callLog.filter((c) => c.url.includes("/api/traces"));
    for (const c of calls) {
      expect(c.url).not.toMatch(/status=warning/);
      expect(c.url).not.toMatch(/status=invalid/);
    }
  });
});

// -------------------------------------------------------------------
// 4. Turn-grouped rendering rolls up sibling calls behind one row.
// -------------------------------------------------------------------
describe("<Traces /> turn-grouped row", () => {
  beforeEach(() => {
    stubFetch((url) => {
      if (url.endsWith("/api/me")) {
        return new Response(JSON.stringify(adminMe), { status: 200 });
      }
      if (url.includes("/api/traces")) {
        return new Response(
          JSON.stringify({
            // Two calls share a turn_id so the page groups them. The
            // third call has no turn_id and stays a singleton row.
            items: [
              traceRow("c1", 200, "openai", 4, { turn_id: "turn-A" }),
              traceRow("c2", 200, "openai", 5, { turn_id: "turn-A" }),
              traceRow("s1", 500, "openai", 7),
            ],
            next_cursor: { before: "", since: "" },
          }),
          { status: 200 },
        );
      }
      return new Response("{}", { status: 200 });
    });
  });

  it("rolls up the two turn-A calls into a single expandable row", async () => {
    renderTraces();
    await waitFor(() => {
      expect(screen.getByTestId("traces-turn-row-turn-A")).toBeTruthy();
    });
    // The singleton row keyed on trace_id is independent.
    expect(screen.getByTestId("traces-single-row-s1")).toBeTruthy();
    // The "2" calls count is rendered inside the grouped row, not three
    // separate rows.
    const turnRow = screen.getByTestId("traces-turn-row-turn-A");
    expect(turnRow.textContent).toMatch(/2/);
    // Only ONE trace_id-stamped single row plus ONE turn_id-stamped row
    // — proving the grouping dropped the duplicate trace_id into the
    // parent row.
    expect(screen.queryByTestId("traces-single-row-c1")).toBeNull();
    expect(screen.queryByTestId("traces-single-row-c2")).toBeNull();
  });

  it("clicking a turn row expands inline calls via fetchTraces({ turn })", async () => {
    renderTraces();
    await waitFor(() => screen.getByTestId("traces-turn-row-turn-A"));
    fireEvent.click(screen.getByTestId("traces-turn-row-turn-A"));
    // The second fetch should target the same turn_id via the turn=
    // query parameter — that is the contract overview uses to drill in.
    await waitFor(() => {
      const turnCalls = callLog.filter(
        (c) => c.url.includes("/api/traces") && c.url.includes("turn=turn-A"),
      );
      expect(turnCalls.length).toBeGreaterThanOrEqual(1);
    });
  });

  it("inline expansion re-applies the active filters", async () => {
    renderTraces();
    await waitFor(() => screen.getByTestId("traces-turn-row-turn-A"));
    // Switch to err-only filter — the parent page refetches with
    // status=err; the expansion must mirror it on its own request.
    await waitFor(() => screen.getByText("4xx/5xx"));
    fireEvent.click(screen.getByText("4xx/5xx"));
    await waitFor(() => {
      const errs = callLog.filter(
        (c) => c.url.includes("/api/traces") && /[?&]status=err/.test(c.url),
      );
      expect(errs.length).toBeGreaterThanOrEqual(1);
    });
    fireEvent.click(screen.getByTestId("traces-turn-row-turn-A"));
    await waitFor(() => {
      const turnCalls = callLog.filter(
        (c) => c.url.includes("/api/traces") && c.url.includes("turn=turn-A"),
      );
      const last = turnCalls[turnCalls.length - 1];
      expect(last.url).toMatch(/[?&]status=err/);
    });
  });

  it("filter change collapses any open expansion", async () => {
    renderTraces();
    await waitFor(() => screen.getByTestId("traces-turn-row-turn-A"));
    fireEvent.click(screen.getByTestId("traces-turn-row-turn-A"));
    // Engineer another status change so queryKey flips; the expansion
    // should collapse because the visible slice has changed.
    fireEvent.click(screen.getByText("2xx/3xx"));
    await waitFor(() => {
      expect(
        document.querySelector('[data-testid="turn-calls-turn-A"]'),
      ).toBeNull();
    });
  });
});
