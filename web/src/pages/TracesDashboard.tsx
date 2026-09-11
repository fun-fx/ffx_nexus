import { useCallback, useMemo, useState, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useSearchParams } from "react-router-dom";
import { Chip } from "../components/Chip";
import { DateTimeField } from "../components/DateTimeField";
import { TimeSeriesChart } from "../components/observability/TimeSeriesChart";
import { Icon } from "../components/icons";
import { dateInputToIso, isoToDateInput } from "../lib/dateInput";
import { formatTokens } from "../lib/format";
import {
  fetchTraceDashboard,
  type Stats,
  type TraceVolumePeriod,
  type VolumeSeriesQuery,
} from "../api";
import {
  parseCSVParam,
  parseVolumeChartKind,
  toggleCSVValue,
  type ChartSeries,
  type VolumeChartKind,
} from "../lib/volumeChart";

const PERIODS: { id: TraceVolumePeriod; label: string }[] = [
  { id: "1h", label: "1h" },
  { id: "24h", label: "24h" },
  { id: "7d", label: "7d" },
  { id: "30d", label: "30d" },
];

function parseStatusParam(raw: string | null): { ok: boolean; err: boolean } {
  if (!raw) return { ok: true, err: true };
  const parts = raw.split(",").map((s) => s.trim());
  return {
    ok: parts.includes("ok"),
    err: parts.includes("err"),
  };
}

function statusToParam(ok: boolean, err: boolean): string {
  const out: string[] = [];
  if (ok) out.push("ok");
  if (err) out.push("err");
  return out.join(",");
}

export function buildDashboardQuery(sp: URLSearchParams): VolumeSeriesQuery {
  const since = sp.get("since") ?? undefined;
  const before = sp.get("before") ?? undefined;
  const { ok, err } = parseStatusParam(sp.get("status"));
  const status: ("ok" | "err")[] = [];
  if (ok) status.push("ok");
  if (err) status.push("err");
  const providers = parseCSVParam(sp.get("provider"));
  const models = parseCSVParam(sp.get("model"));
  const extra = {
    status,
    providers: providers.length ? providers : undefined,
    models: models.length ? models : undefined,
  };
  if (since || before) {
    return { since, before, ...extra };
  }
  const period = (sp.get("period") || "1h") as TraceVolumePeriod;
  return { period, ...extra };
}

/** @deprecated use buildDashboardQuery */
export function buildVolumeSeriesQuery(sp: URLSearchParams): VolumeSeriesQuery {
  return buildDashboardQuery(sp);
}

export function TracesDashboard() {
  const [searchParams, setSearchParams] = useSearchParams();
  const queryClient = useQueryClient();
  const [refreshToken, setRefreshToken] = useState(0);

  const dashQuery = useMemo(
    () => buildDashboardQuery(searchParams),
    [searchParams],
  );

  const { data, isFetching, isLoading, isError, error } = useQuery({
    queryKey: ["trace-dashboard", dashQuery, refreshToken],
    queryFn: () => fetchTraceDashboard(dashQuery),
  });

  const period = searchParams.get("period") as TraceVolumePeriod | null;
  const customWindow = Boolean(searchParams.get("since") || searchParams.get("before"));
  const { ok: statusOk, err: statusErr } = parseStatusParam(searchParams.get("status"));
  const selectedProviders = parseCSVParam(searchParams.get("provider"));
  const selectedModels = parseCSVParam(searchParams.get("model"));

  const volumeKind = parseVolumeChartKind(searchParams.get("volume_chart"));
  const tokensKind = parseVolumeChartKind(searchParams.get("tokens_chart"));
  const costKind = parseVolumeChartKind(searchParams.get("cost_chart"));
  const latencyKind = parseVolumeChartKind(searchParams.get("latency_chart"));
  const cacheKind = parseVolumeChartKind(searchParams.get("cache_chart"));

  const sinceInput = isoToDateInput(searchParams.get("since") ?? "");
  const beforeInput = isoToDateInput(searchParams.get("before") ?? "");

  const patchParams = useCallback(
    (patch: Record<string, string | null>) => {
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev);
        for (const [key, value] of Object.entries(patch)) {
          if (value === null || value === "") next.delete(key);
          else next.set(key, value);
        }
        return next;
      });
    },
    [setSearchParams],
  );

  const setPeriod = (p: TraceVolumePeriod) => {
    patchParams({ period: p, since: null, before: null });
  };

  const setStatus = (ok: boolean, err: boolean) => {
    patchParams({ status: statusToParam(ok, err) || null });
  };

  const setChartKind = (param: string, kind: VolumeChartKind) => {
    patchParams({ [param]: kind === "line" ? null : kind });
  };

  const toggleProvider = (name: string) => {
    patchParams({ provider: toggleCSVValue(selectedProviders, name).join(",") || null });
  };

  const toggleModel = (name: string) => {
    patchParams({ model: toggleCSVValue(selectedModels, name).join(",") || null });
  };

  const clearFilters = () => {
    patchParams({
      provider: null,
      model: null,
      status: null,
      since: null,
      before: null,
      period: "1h",
    });
  };

  const stats: Stats = data?.stats ?? {
    total_requests: 0,
    error_rate: 0,
    avg_latency_ms: 0,
    p95_latency_ms: 0,
    total_tokens: 0,
    total_input_tokens: 0,
    total_output_tokens: 0,
    total_cost_usd: 0,
    cache_hits: 0,
    cache_hit_rate: 0,
    guardrail_events: 0,
  };

  const volume = data?.volume ?? [];
  const tokens = data?.tokens ?? [];
  const cost = data?.cost ?? [];
  const latency = data?.latency ?? [];
  const cache = data?.cache ?? [];
  const warehouseDown = !isError && data?.available === false;
  const facets = data?.facets ?? { providers: [], models: [] };

  const volumeSeries = useMemo<ChartSeries[]>(() => {
    const series: ChartSeries[] = [];
    if (statusOk) {
      series.push({
        label: "Success",
        colorVar: "--ok",
        values: volume.map((b) => b.ok),
      });
    }
    if (statusErr) {
      series.push({
        label: "Error",
        colorVar: "--err",
        values: volume.map((b) => b.err),
      });
    }
    return series;
  }, [volume, statusOk, statusErr]);

  const tokenSeries = useMemo<ChartSeries[]>(
    () => [
      { label: "Prompt", colorVar: "--accent", values: tokens.map((b) => b.input) },
      { label: "Completion", colorVar: "--accent-2", values: tokens.map((b) => b.output) },
    ],
    [tokens],
  );

  const costSeries = useMemo<ChartSeries[]>(
    () => [{ label: "Cost", colorVar: "--accent-3", values: cost.map((b) => b.usd) }],
    [cost],
  );

  const latencySeries = useMemo<ChartSeries[]>(
    () => [
      { label: "Avg", colorVar: "--info", values: latency.map((b) => b.avg_ms) },
      { label: "p95", colorVar: "--warn", values: latency.map((b) => b.p95_ms) },
    ],
    [latency],
  );

  const cacheSeries = useMemo<ChartSeries[]>(() => {
    return [
      {
        label: "Hit rate %",
        colorVar: "--ok",
        values: cache.map((b) =>
          b.requests > 0 ? (b.hits / b.requests) * 100 : 0,
        ),
      },
    ];
  }, [cache]);

  const totalOk = volume.reduce((n, b) => n + b.ok, 0);
  const totalErr = volume.reduce((n, b) => n + b.err, 0);
  const tokenIn = tokens.reduce((n, b) => n + b.input, 0);
  const tokenOut = tokens.reduce((n, b) => n + b.output, 0);
  const costSum = cost.reduce((n, b) => n + b.usd, 0);

  return (
    <div className="trace-dashboard">
      <header className="page-head">
        <div>
          <h1>Dashboard</h1>
          <p className="muted">
            Request volume, tokens, cost, latency, and cache over the same window.
            Filters apply to every panel.
          </p>
        </div>
        <div className="trace-dashboard__stats" aria-label="Window totals" data-testid="dashboard-kpis">
          <Kpi label="Requests" value={stats.total_requests.toLocaleString()} />
          <Kpi
            label="Error rate"
            value={`${(stats.error_rate * 100).toFixed(1)}%`}
            tone={stats.error_rate > 0.05 ? "err" : undefined}
          />
          <Kpi
            label="Tokens"
            value={formatTokens(stats.total_tokens)}
            title={`${stats.total_input_tokens.toLocaleString()} prompt · ${stats.total_output_tokens.toLocaleString()} completion`}
          />
          <Kpi label="Cost" value={`$${stats.total_cost_usd.toFixed(4)}`} />
          <Kpi
            label="Latency"
            value={`${Math.round(stats.avg_latency_ms)} / ${Math.round(stats.p95_latency_ms)} ms`}
            title="Average / p95"
          />
          <Kpi label="Cache hit" value={`${(stats.cache_hit_rate * 100).toFixed(1)}%`} />
        </div>
      </header>

      {warehouseDown ? <ClickHouseSetupNotice /> : null}

      <div className="filter-bar">
        <div className="filter-chips" role="group" aria-label="Time preset">
          {PERIODS.map((p) => (
            <Chip
              key={p.id}
              tone={!customWindow && (period || "1h") === p.id ? "accent" : "neutral"}
              active={!customWindow && (period || "1h") === p.id}
              data-testid={`dashboard-period-${p.id}`}
              onClick={() => setPeriod(p.id)}
            >
              {p.label}
            </Chip>
          ))}
        </div>
        <button
          type="button"
          className="btn-ghost"
          onClick={clearFilters}
          data-testid="dashboard-clear-filters"
        >
          Clear filters
        </button>
        <button
          type="button"
          className="btn-ghost trace-dashboard__refresh"
          onClick={() => {
            setRefreshToken((t) => t + 1);
            void queryClient.invalidateQueries({ queryKey: ["trace-dashboard"] });
          }}
          data-testid="dashboard-refresh"
          aria-label="Refresh chart"
        >
          <Icon.activity size={14} />
          {isFetching ? "Refreshing…" : "Refresh"}
        </button>
      </div>

      <div className="filter-bar filter-bar-window" role="group" aria-label="Custom time window">
        <DateTimeField
          label="Since"
          value={sinceInput}
          onChange={(next) => {
            const iso = dateInputToIso(next);
            patchParams({
              since: iso,
              period: null,
              ...(iso ? {} : { before: searchParams.get("before") }),
            });
          }}
          aria-label="Window start"
          data-testid="dashboard-window-since"
        />
        <DateTimeField
          label="Before"
          value={beforeInput}
          onChange={(next) => {
            const iso = dateInputToIso(next);
            patchParams({
              before: iso,
              period: null,
            });
          }}
          aria-label="Window end"
          data-testid="dashboard-window-before"
        />
        <button
          type="button"
          className="btn-ghost"
          onClick={() => patchParams({ since: null, before: null, period: period || "1h" })}
          disabled={!customWindow}
          data-testid="dashboard-window-clear"
        >
          Clear window
        </button>
        <span className="sr-only" data-testid="dashboard-query-bridge">
          {JSON.stringify(dashQuery)}
        </span>
        <span className="sr-only" data-testid="dashboard-refresh-token">
          {refreshToken}
        </span>
      </div>

      <div className="trace-dashboard__body">
        <aside className="trace-dashboard__sidebar panel" aria-label="Filters">
          <header className="panel-head">
            <h2>Filters</h2>
          </header>
          <div className="trace-dashboard__filter-section">
            <h3 className="trace-dashboard__filter-title">Status</h3>
            <label className="trace-dashboard__check">
              <input
                type="checkbox"
                checked={statusOk}
                onChange={(e) => setStatus(e.target.checked, statusErr)}
                data-testid="dashboard-status-ok"
              />
              <span>Success (2xx/3xx)</span>
            </label>
            <label className="trace-dashboard__check">
              <input
                type="checkbox"
                checked={statusErr}
                onChange={(e) => setStatus(statusOk, e.target.checked)}
                data-testid="dashboard-status-err"
              />
              <span>Error (4xx/5xx)</span>
            </label>
          </div>
          <div className="trace-dashboard__filter-section">
            <h3 className="trace-dashboard__filter-title">Provider</h3>
            {facets.providers.length === 0 ? (
              <p className="muted">None in this window</p>
            ) : (
              <div className="trace-dashboard__facet-chips" data-testid="dashboard-provider-facets">
                {facets.providers.map((name) => (
                  <Chip
                    key={name}
                    tone={selectedProviders.includes(name) ? "accent" : "neutral"}
                    active={selectedProviders.includes(name)}
                    data-testid={`dashboard-provider-${name}`}
                    onClick={() => toggleProvider(name)}
                  >
                    {name}
                  </Chip>
                ))}
              </div>
            )}
          </div>
          <div className="trace-dashboard__filter-section">
            <h3 className="trace-dashboard__filter-title">Model</h3>
            {facets.models.length === 0 ? (
              <p className="muted">None in this window</p>
            ) : (
              <div className="trace-dashboard__facet-chips" data-testid="dashboard-model-facets">
                {facets.models.map((name) => (
                  <Chip
                    key={name}
                    tone={selectedModels.includes(name) ? "accent" : "neutral"}
                    active={selectedModels.includes(name)}
                    data-testid={`dashboard-model-${name}`}
                    onClick={() => toggleModel(name)}
                  >
                    {name}
                  </Chip>
                ))}
              </div>
            )}
          </div>
        </aside>

        <div className="trace-dashboard__charts">
          <ChartPanel
            title="Request volume"
            ariaLabel="Request volume chart"
            kind={volumeKind}
            kindParam="volume_chart"
            onKind={(k) => setChartKind("volume_chart", k)}
            intervalLabel={
              isLoading || isFetching
                ? "loading…"
                : `${volume.length} buckets · ${data?.interval_seconds ?? 0}s interval`
            }
            header={`${totalOk.toLocaleString()} ok · ${totalErr.toLocaleString()} err`}
            available={data?.available ?? true}
            isError={isError}
            error={error}
            timestamps={volume.map((b) => b.timestamp)}
            series={volumeSeries}
            legend={
              warehouseDown
                ? null
                : (
                    <>
                      {statusOk ? (
                        <span className="trace-dashboard__legend-item trace-dashboard__legend-item--ok">
                          Success
                        </span>
                      ) : null}
                      {statusErr ? (
                        <span className="trace-dashboard__legend-item trace-dashboard__legend-item--err">
                          Error
                        </span>
                      ) : null}
                    </>
                  )
            }
            testId="volume"
          />
          <ChartPanel
            title="Token usage"
            ariaLabel="Token usage chart"
            kind={tokensKind}
            kindParam="tokens_chart"
            onKind={(k) => setChartKind("tokens_chart", k)}
            intervalLabel={`${formatTokens(tokenIn)} prompt · ${formatTokens(tokenOut)} completion`}
            available={data?.available ?? true}
            isError={isError}
            error={error}
            timestamps={tokens.map((b) => b.timestamp)}
            series={tokenSeries}
            legend={
              <>
                <span className="trace-dashboard__legend-item trace-dashboard__legend-item--accent">
                  Prompt
                </span>
                <span className="trace-dashboard__legend-item trace-dashboard__legend-item--cyan">
                  Completion
                </span>
              </>
            }
            testId="tokens"
          />
          <ChartPanel
            title="Cost"
            ariaLabel="Cost chart"
            kind={costKind}
            kindParam="cost_chart"
            onKind={(k) => setChartKind("cost_chart", k)}
            intervalLabel={`$${costSum.toFixed(4)}`}
            available={data?.available ?? true}
            isError={isError}
            error={error}
            timestamps={cost.map((b) => b.timestamp)}
            series={costSeries}
            legend={
              <span className="trace-dashboard__legend-item trace-dashboard__legend-item--violet">
                USD
              </span>
            }
            testId="cost"
          />
          <ChartPanel
            title="Latency"
            ariaLabel="Latency chart"
            kind={latencyKind}
            kindParam="latency_chart"
            onKind={(k) => setChartKind("latency_chart", k)}
            intervalLabel="avg / p95 (ms)"
            available={data?.available ?? true}
            isError={isError}
            error={error}
            timestamps={latency.map((b) => b.timestamp)}
            series={latencySeries}
            legend={
              <>
                <span className="trace-dashboard__legend-item trace-dashboard__legend-item--info">
                  Avg
                </span>
                <span className="trace-dashboard__legend-item trace-dashboard__legend-item--warn">
                  p95
                </span>
              </>
            }
            testId="latency"
          />
          <ChartPanel
            title="Cache hit rate"
            ariaLabel="Cache hit rate chart"
            kind={cacheKind}
            kindParam="cache_chart"
            onKind={(k) => setChartKind("cache_chart", k)}
            intervalLabel={`${(stats.cache_hit_rate * 100).toFixed(1)}% in window`}
            available={data?.available ?? true}
            isError={isError}
            error={error}
            timestamps={cache.map((b) => b.timestamp)}
            series={cacheSeries}
            legend={
              <span className="trace-dashboard__legend-item trace-dashboard__legend-item--ok">
                Hit rate %
              </span>
            }
            testId="cache"
          />
        </div>
      </div>
    </div>
  );
}

function Kpi({
  label,
  value,
  tone,
  title,
}: {
  label: string;
  value: string;
  tone?: "err";
  title?: string;
}) {
  return (
    <div className="trace-dashboard__stat" title={title}>
      <span className="trace-dashboard__stat-label">{label}</span>
      <span
        className={
          "trace-dashboard__stat-value" +
          (tone === "err" ? " trace-dashboard__stat-value--err" : "")
        }
      >
        {value}
      </span>
    </div>
  );
}

function ChartPanel({
  title,
  ariaLabel,
  kind,
  kindParam,
  onKind,
  intervalLabel,
  header,
  available,
  isError,
  error,
  timestamps,
  series,
  legend,
  testId,
}: {
  title: string;
  ariaLabel: string;
  kind: VolumeChartKind;
  kindParam: string;
  onKind: (k: VolumeChartKind) => void;
  intervalLabel: string;
  header?: string;
  available: boolean;
  isError: boolean;
  error: unknown;
  timestamps: string[];
  series: ChartSeries[];
  legend: ReactNode;
  testId: string;
}) {
  return (
    <section className="panel trace-dashboard__chart-panel" aria-label={ariaLabel}>
      <header className="panel-head">
        <h2>{title}</h2>
        <div className="trace-dashboard__chart-actions">
          <div className="trace-dashboard__chart-toggle" role="group" aria-label={`${title} chart type`}>
            <button
              type="button"
              className={"btn-ghost" + (kind === "line" ? " is-active" : "")}
              aria-pressed={kind === "line"}
              data-testid={testId === "volume" ? "dashboard-chart-line" : `dashboard-${testId}-chart-line`}
              onClick={() => onKind("line")}
            >
              <Icon.chart size={14} />
              Line
            </button>
            <button
              type="button"
              className={"btn-ghost" + (kind === "bar" ? " is-active" : "")}
              aria-pressed={kind === "bar"}
              data-testid={testId === "volume" ? "dashboard-chart-bar" : `dashboard-${testId}-chart-bar`}
              onClick={() => onKind("bar")}
            >
              <Icon.bars size={14} />
              Bar
            </button>
          </div>
          <span className="panel-link muted">{header ?? intervalLabel}</span>
        </div>
      </header>
      <ChartBody
        available={available}
        isError={isError}
        error={error}
        timestamps={timestamps}
        series={series}
        kind={kind}
        testId={`${testId}-chart`}
      />
      {available && !isError ? (
        <div className="trace-dashboard__legend" aria-hidden="true">
          {legend}
        </div>
      ) : null}
      <span className="sr-only" data-testid={`dashboard-${testId}-kind-param`}>
        {kindParam}
      </span>
    </section>
  );
}

function ChartBody({
  available,
  isError,
  error,
  timestamps,
  series,
  kind,
  testId,
}: {
  available: boolean;
  isError: boolean;
  error: unknown;
  timestamps: string[];
  series: ChartSeries[];
  kind: VolumeChartKind;
  testId: string;
}) {
  if (isError) {
    return (
      <div className="panel-empty" data-testid={`${testId}-error`}>
        {error instanceof Error ? error.message : "Failed to load chart."}
      </div>
    );
  }
  if (!available) {
    return (
      <div className="empty-card trace-dashboard__setup-card" data-testid={`${testId}-unavailable`}>
        <h3>ClickHouse required</h3>
        <p>
          Historical series live in ClickHouse. Set{" "}
          <code>NEXUS_CLICKHOUSE_URL</code> and restart to fill this chart.
        </p>
      </div>
    );
  }
  if (timestamps.length === 0) {
    return (
      <div className="panel-empty" data-testid={`${testId}-empty`}>
        No data available
      </div>
    );
  }
  return (
    <TimeSeriesChart
      timestamps={timestamps}
      series={series}
      kind={kind}
      testId={testId}
    />
  );
}

function ClickHouseSetupNotice() {
  return (
    <div className="callout trace-dashboard__setup" role="status" data-testid="dashboard-clickhouse-banner">
      <strong>ClickHouse is not connected</strong>
      <p>
        Request volume is stored in ClickHouse. This process has no{" "}
        <code>NEXUS_CLICKHOUSE_URL</code>, so there is nothing to chart yet.
        Live traces still stream without a warehouse.
      </p>
      <p>Start ClickHouse, set the URL, then restart Nexus:</p>
      <pre className="trace-dashboard__setup-cmd">{`docker compose -f deploy/docker-compose.yml up -d clickhouse
export NEXUS_CLICKHOUSE_URL="clickhouse://nexus:nexus@localhost:9000/nexus"`}</pre>
      <p>
        Same env var on Helm.{" "}
        <Link to="/develop/docs/configuration">Configuration docs</Link>
      </p>
    </div>
  );
}
