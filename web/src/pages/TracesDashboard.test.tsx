import { describe, expect, it, vi, afterEach, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ThemeProvider } from "../theme/ThemeProvider";
import { TracesDashboard, buildVolumeSeriesQuery } from "./TracesDashboard";

let callLog: string[] = [];
let fetchMock: ReturnType<typeof vi.fn>;

function stubSeriesResponse(
  buckets: { timestamp: string; ok: number; err: number }[] = [],
  extra: { available?: boolean; status?: number } = {},
) {
  fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    callLog.push(url);
    if (url.includes("/api/traces/series")) {
      if (extra.status && extra.status >= 400) {
        return new Response("query failed", { status: extra.status });
      }
      return new Response(
        JSON.stringify({
          buckets,
          interval_seconds: 60,
          since: "2026-01-01T00:00:00Z",
          before: "2026-01-01T01:00:00Z",
          available: extra.available ?? true,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }
    return new Response("{}", { status: 404 });
  });
  vi.stubGlobal("fetch", fetchMock);
}

function renderDashboard(initial = "/observability/dashboard?period=1h&status=ok,err") {
  callLog = [];
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={[initial]}>
          <Routes>
            <Route path="/observability/dashboard" element={<TracesDashboard />} />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>
    </ThemeProvider>,
  );
}

afterEach(() => vi.unstubAllGlobals());

describe("buildVolumeSeriesQuery", () => {
  it("defaults to 1h with both statuses", () => {
    const q = buildVolumeSeriesQuery(new URLSearchParams());
    expect(q).toEqual({ period: "1h", status: ["ok", "err"] });
  });

  it("prefers explicit window over period", () => {
    const sp = new URLSearchParams("since=2026-01-01T00:00:00Z&before=2026-01-02T00:00:00Z&period=7d");
    const q = buildVolumeSeriesQuery(sp);
    expect(q.period).toBeUndefined();
    expect(q.since).toBe("2026-01-01T00:00:00Z");
    expect(q.before).toBe("2026-01-02T00:00:00Z");
  });
});

describe("TracesDashboard", () => {
  beforeEach(() => stubSeriesResponse());

  it("loads series on mount with default query", async () => {
    renderDashboard();
    await waitFor(() => {
      expect(callLog.some((u) => u.includes("/api/traces/series"))).toBe(true);
    });
    const hit = callLog.find((u) => u.includes("/api/traces/series"))!;
    expect(hit).toContain("period=1h");
    expect(hit).toContain("status=ok%2Cerr");
  });

  it("switches period preset in the URL and refetches", async () => {
    renderDashboard();
    await waitFor(() => expect(callLog.length).toBeGreaterThan(0));
    callLog = [];
    fireEvent.click(screen.getByTestId("dashboard-period-24h"));
    await waitFor(() => {
      expect(callLog.some((u) => u.includes("period=24h"))).toBe(true);
    });
  });

  it("updates status filter in query string", async () => {
    renderDashboard();
    await waitFor(() => expect(callLog.length).toBeGreaterThan(0));
    callLog = [];
    fireEvent.click(screen.getByTestId("dashboard-status-ok"));
    await waitFor(() => {
      const hit = callLog.find((u) => u.includes("/api/traces/series"));
      expect(hit).toBeDefined();
      expect(hit).toContain("status=err");
      expect(hit).not.toContain("ok");
    });
  });

  it("refresh button bumps the manual refresh token and refetches", async () => {
    renderDashboard();
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    fireEvent.click(screen.getByTestId("dashboard-refresh"));
    await waitFor(() =>
      expect(screen.getByTestId("dashboard-refresh-token")).toHaveTextContent("1"),
    );
    await waitFor(() => expect(fetchMock.mock.calls.length).toBeGreaterThanOrEqual(2));
  });

  it("shows empty state when no volume", async () => {
    stubSeriesResponse([]);
    renderDashboard();
    expect(await screen.findByTestId("volume-chart-empty")).toBeInTheDocument();
  });

  it("shows a page-level ClickHouse banner when the warehouse is unavailable", async () => {
    stubSeriesResponse([], { available: false });
    renderDashboard();
    const banner = await screen.findByTestId("dashboard-clickhouse-banner");
    expect(banner).toBeVisible();
    expect(banner).toHaveTextContent(/ClickHouse is not connected/i);
    expect(banner).toHaveTextContent("NEXUS_CLICKHOUSE_URL");
    expect(await screen.findByTestId("volume-chart-unavailable")).toBeVisible();
    expect(screen.queryByTestId("volume-chart")).not.toBeInTheDocument();
  });

  it("shows an error when the series request fails", async () => {
    stubSeriesResponse([], { status: 500 });
    renderDashboard();
    expect(await screen.findByTestId("volume-chart-error")).toBeInTheDocument();
  });

  it("renders chart host when buckets have volume", async () => {
    stubSeriesResponse([
      { timestamp: "2026-01-01T00:00:00Z", ok: 5, err: 1 },
      { timestamp: "2026-01-01T00:01:00Z", ok: 3, err: 0 },
    ]);
    renderDashboard();
    expect(await screen.findByTestId("volume-chart")).toBeInTheDocument();
  });

  it("still mounts the chart for a zero-filled window", async () => {
    stubSeriesResponse([
      { timestamp: "2026-01-01T00:00:00Z", ok: 0, err: 0 },
      { timestamp: "2026-01-01T00:01:00Z", ok: 0, err: 0 },
    ]);
    renderDashboard();
    expect(await screen.findByTestId("volume-chart")).toBeInTheDocument();
  });

  it("toggles bar mode into the URL", async () => {
    stubSeriesResponse([{ timestamp: "2026-01-01T00:00:00Z", ok: 1, err: 0 }]);
    renderDashboard();
    await screen.findByTestId("volume-chart");
    fireEvent.click(screen.getByTestId("dashboard-chart-bar"));
    expect(screen.getByTestId("dashboard-chart-bar")).toHaveAttribute("aria-pressed", "true");
  });
});
