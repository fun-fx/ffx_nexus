import { describe, expect, it, vi, afterEach, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ThemeProvider } from "../theme/ThemeProvider";
import { TracesDashboard, buildDashboardQuery, buildVolumeSeriesQuery } from "./TracesDashboard";

let callLog: string[] = [];
let fetchMock: ReturnType<typeof vi.fn>;

function stubDashboardResponse(
  extra: {
    available?: boolean;
    status?: number;
    volume?: { timestamp: string; ok: number; err: number }[];
    providers?: string[];
    models?: string[];
  } = {},
) {
  fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    callLog.push(url);
    if (url.includes("/api/traces/dashboard")) {
      if (extra.status && extra.status >= 400) {
        return new Response("query failed", { status: extra.status });
      }
      return new Response(
        JSON.stringify({
          available: extra.available ?? true,
          interval_seconds: 60,
          since: "2026-01-01T00:00:00Z",
          before: "2026-01-01T01:00:00Z",
          stats: {
            total_requests: 12,
            error_rate: 0.1,
            avg_latency_ms: 80,
            p95_latency_ms: 140,
            total_tokens: 400,
            total_input_tokens: 250,
            total_output_tokens: 150,
            total_cost_usd: 0.0123,
            cache_hits: 2,
            cache_hit_rate: 0.25,
            guardrail_events: 0,
          },
          volume: extra.volume ?? [],
          tokens: extra.volume?.map((b) => ({
            timestamp: b.timestamp,
            input: b.ok,
            output: b.err,
          })) ?? [],
          cost: extra.volume?.map((b) => ({ timestamp: b.timestamp, usd: 0.01 })) ?? [],
          latency: extra.volume?.map((b) => ({
            timestamp: b.timestamp,
            avg_ms: 50,
            p95_ms: 90,
          })) ?? [],
          cache: extra.volume?.map((b) => ({
            timestamp: b.timestamp,
            hits: 1,
            requests: 4,
          })) ?? [],
          facets: {
            providers: extra.providers ?? ["openai", "gemini"],
            models: extra.models ?? ["gpt-4o-mini"],
          },
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

describe("buildDashboardQuery", () => {
  it("defaults to 1h with both statuses", () => {
    const q = buildDashboardQuery(new URLSearchParams());
    expect(q).toEqual({ period: "1h", status: ["ok", "err"] });
  });

  it("prefers explicit window over period", () => {
    const sp = new URLSearchParams("since=2026-01-01T00:00:00Z&before=2026-01-02T00:00:00Z&period=7d");
    const q = buildDashboardQuery(sp);
    expect(q.period).toBeUndefined();
    expect(q.since).toBe("2026-01-01T00:00:00Z");
    expect(q.before).toBe("2026-01-02T00:00:00Z");
  });

  it("parses provider and model comma lists", () => {
    const sp = new URLSearchParams("provider=openai,gemini&model=gpt-4o-mini");
    const q = buildDashboardQuery(sp);
    expect(q.providers).toEqual(["openai", "gemini"]);
    expect(q.models).toEqual(["gpt-4o-mini"]);
  });
});

describe("buildVolumeSeriesQuery", () => {
  it("is an alias of buildDashboardQuery", () => {
    const sp = new URLSearchParams("period=24h&provider=openai");
    expect(buildVolumeSeriesQuery(sp)).toEqual(buildDashboardQuery(sp));
  });
});

describe("TracesDashboard", () => {
  beforeEach(() => stubDashboardResponse());

  it("loads dashboard on mount with default query", async () => {
    renderDashboard();
    await waitFor(() => {
      expect(callLog.some((u) => u.includes("/api/traces/dashboard"))).toBe(true);
    });
    const hit = callLog.find((u) => u.includes("/api/traces/dashboard"))!;
    expect(hit).toContain("period=1h");
    expect(hit).toContain("status=ok%2Cerr");
  });

  it("renders KPI cards", async () => {
    renderDashboard();
    const kpis = await screen.findByTestId("dashboard-kpis");
    await waitFor(() => expect(kpis).toHaveTextContent("12"));
    expect(kpis).toHaveTextContent("10.0%");
    expect(kpis).toHaveTextContent("$0.0123");
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
      const hit = callLog.find((u) => u.includes("/api/traces/dashboard"));
      expect(hit).toBeDefined();
      expect(hit).toContain("status=err");
      expect(hit).not.toContain("ok");
    });
  });

  it("toggles a provider facet into the query string", async () => {
    renderDashboard();
    const chip = await screen.findByTestId("dashboard-provider-openai");
    callLog = [];
    fireEvent.click(chip);
    await waitFor(() => {
      expect(callLog.some((u) => u.includes("provider=openai"))).toBe(true);
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
    stubDashboardResponse({ volume: [] });
    renderDashboard();
    expect(await screen.findByTestId("volume-chart-empty")).toBeInTheDocument();
    expect(await screen.findByTestId("tokens-chart-empty")).toBeInTheDocument();
    expect(await screen.findByTestId("cost-chart-empty")).toBeInTheDocument();
    expect(await screen.findByTestId("latency-chart-empty")).toBeInTheDocument();
    expect(await screen.findByTestId("cache-chart-empty")).toBeInTheDocument();
  });

  it("shows a page-level ClickHouse banner when the warehouse is unavailable", async () => {
    stubDashboardResponse({ available: false });
    renderDashboard();
    const banner = await screen.findByTestId("dashboard-clickhouse-banner");
    expect(banner).toBeVisible();
    expect(banner).toHaveTextContent(/ClickHouse is not connected/i);
    expect(banner).toHaveTextContent("NEXUS_CLICKHOUSE_URL");
    expect(await screen.findByTestId("volume-chart-unavailable")).toBeVisible();
    expect(screen.queryByTestId("volume-chart")).not.toBeInTheDocument();
  });

  it("shows an error when the dashboard request fails", async () => {
    stubDashboardResponse({ status: 500 });
    renderDashboard();
    expect(await screen.findByTestId("volume-chart-error")).toBeInTheDocument();
  });

  it("renders chart host when buckets have volume", async () => {
    stubDashboardResponse({
      volume: [
        { timestamp: "2026-01-01T00:00:00Z", ok: 5, err: 1 },
        { timestamp: "2026-01-01T00:01:00Z", ok: 3, err: 0 },
      ],
    });
    renderDashboard();
    expect(await screen.findByTestId("volume-chart")).toBeInTheDocument();
    expect(await screen.findByTestId("tokens-chart")).toBeInTheDocument();
    expect(await screen.findByTestId("cost-chart")).toBeInTheDocument();
    expect(await screen.findByTestId("latency-chart")).toBeInTheDocument();
    expect(await screen.findByTestId("cache-chart")).toBeInTheDocument();
  });

  it("still mounts the chart for a zero-filled window", async () => {
    stubDashboardResponse({
      volume: [
        { timestamp: "2026-01-01T00:00:00Z", ok: 0, err: 0 },
        { timestamp: "2026-01-01T00:01:00Z", ok: 0, err: 0 },
      ],
    });
    renderDashboard();
    expect(await screen.findByTestId("volume-chart")).toBeInTheDocument();
  });

  it("toggles bar mode into the URL", async () => {
    stubDashboardResponse({
      volume: [{ timestamp: "2026-01-01T00:00:00Z", ok: 1, err: 0 }],
    });
    renderDashboard();
    await screen.findByTestId("volume-chart");
    fireEvent.click(screen.getByTestId("dashboard-chart-bar"));
    expect(screen.getByTestId("dashboard-chart-bar")).toHaveAttribute("aria-pressed", "true");
  });
});
