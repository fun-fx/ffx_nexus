import { useCallback, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useSearchParams } from "react-router-dom";
import { Chip } from "../components/Chip";
import { TimeSeriesChart } from "../components/observability/TimeSeriesChart";
import { Icon } from "../components/icons";
import {
  fetchTraceVolumeSeries,
  type TraceVolumePeriod,
  type VolumeSeriesQuery,
} from "../api";
import {
  parseVolumeChartKind,
  type VolumeChartKind,
} from "../lib/volumeChart";

const PERIODS: { id: TraceVolumePeriod; label: string }[] = [
  { id: "1h", label: "1h" },
  { id: "24h", label: "24h" },
  { id: "7d", label: "7d" },
  { id: "30d", label: "30d" },
];

function dateInputToIso(value: string): string | null {
  if (!value) return null;
  const d = new Date(value);
  if (isNaN(d.getTime())) return null;
  return d.toISOString();
}

function isoToDateInput(iso: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

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

export function buildVolumeSeriesQuery(sp: URLSearchParams): VolumeSeriesQuery {
  const since = sp.get("since") ?? undefined;
  const before = sp.get("before") ?? undefined;
  const { ok, err } = parseStatusParam(sp.get("status"));
  const status: ("ok" | "err")[] = [];
  if (ok) status.push("ok");
  if (err) status.push("err");

  if (since || before) {
    return { since, before, status };
  }
  const period = (sp.get("period") || "1h") as TraceVolumePeriod;
  return { period, status };
}

export function TracesDashboard() {
  const [searchParams, setSearchParams] = useSearchParams();
  const queryClient = useQueryClient();
  const [refreshToken, setRefreshToken] = useState(0);

  const seriesQuery = useMemo(
    () => buildVolumeSeriesQuery(searchParams),
    [searchParams],
  );

  const { data, isFetching, isLoading, isError, error } = useQuery({
    queryKey: ["trace-volume", seriesQuery, refreshToken],
    queryFn: () => fetchTraceVolumeSeries(seriesQuery),
  });

  const period = searchParams.get("period") as TraceVolumePeriod | null;
  const customWindow = Boolean(searchParams.get("since") || searchParams.get("before"));
  const { ok: statusOk, err: statusErr } = parseStatusParam(searchParams.get("status"));
  const chartKind = parseVolumeChartKind(searchParams.get("volume_chart"));

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

  const setChartKind = (kind: VolumeChartKind) => {
    patchParams({ volume_chart: kind === "line" ? null : kind });
  };

  const buckets = data?.buckets ?? [];
  const totalOk = buckets.reduce((n, b) => n + b.ok, 0);
  const totalErr = buckets.reduce((n, b) => n + b.err, 0);
  const warehouseDown = !isError && data?.available === false;
  const showChart = Boolean(data?.available) && buckets.length > 0;

  return (
    <div className="trace-dashboard">
      <header className="page-head">
        <div>
          <h1>Dashboard</h1>
          <p className="muted">
            Request volume over your gateway traffic. Filters narrow every bucket in the chart.
          </p>
        </div>
        <div className="trace-dashboard__stats" aria-label="Window totals">
          <div className="trace-dashboard__stat">
            <span className="trace-dashboard__stat-label">Success</span>
            <span className="trace-dashboard__stat-value trace-dashboard__stat-value--ok">
              {totalOk.toLocaleString()}
            </span>
          </div>
          <div className="trace-dashboard__stat">
            <span className="trace-dashboard__stat-label">Error</span>
            <span className="trace-dashboard__stat-value trace-dashboard__stat-value--err">
              {totalErr.toLocaleString()}
            </span>
          </div>
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
          className="btn-ghost trace-dashboard__refresh"
          onClick={() => {
            setRefreshToken((t) => t + 1);
            void queryClient.invalidateQueries({ queryKey: ["trace-volume"] });
          }}
          data-testid="dashboard-refresh"
          aria-label="Refresh chart"
        >
          <Icon.activity size={14} />
          {isFetching ? "Refreshing…" : "Refresh"}
        </button>
      </div>

      <div className="filter-bar filter-bar-window" role="group" aria-label="Custom time window">
        <label className="dt-input">
          <span>Since</span>
          <input
            type="datetime-local"
            value={sinceInput}
            onChange={(e) => {
              const iso = dateInputToIso(e.target.value);
              patchParams({
                since: iso,
                period: null,
                ...(iso ? {} : { before: searchParams.get("before") }),
              });
            }}
            aria-label="Window start"
            data-testid="dashboard-window-since"
          />
        </label>
        <label className="dt-input">
          <span>Before</span>
          <input
            type="datetime-local"
            value={beforeInput}
            onChange={(e) => {
              const iso = dateInputToIso(e.target.value);
              patchParams({
                before: iso,
                period: null,
              });
            }}
            aria-label="Window end"
            data-testid="dashboard-window-before"
          />
        </label>
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
          {JSON.stringify(seriesQuery)}
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
        </aside>

        <section className="panel trace-dashboard__chart-panel" aria-label="Request volume chart">
          <header className="panel-head">
            <h2>Request volume</h2>
            <div className="trace-dashboard__chart-actions">
              <div className="trace-dashboard__chart-toggle" role="group" aria-label="Chart type">
                <button
                  type="button"
                  className={"btn-ghost" + (chartKind === "line" ? " is-active" : "")}
                  aria-pressed={chartKind === "line"}
                  data-testid="dashboard-chart-line"
                  onClick={() => setChartKind("line")}
                >
                  <Icon.chart size={14} />
                  Line
                </button>
                <button
                  type="button"
                  className={"btn-ghost" + (chartKind === "bar" ? " is-active" : "")}
                  aria-pressed={chartKind === "bar"}
                  data-testid="dashboard-chart-bar"
                  onClick={() => setChartKind("bar")}
                >
                  <Icon.bars size={14} />
                  Bar
                </button>
              </div>
              <span className="panel-link muted">
                {isLoading || isFetching
                  ? "loading…"
                  : `${buckets.length} buckets · ${data?.interval_seconds ?? 0}s interval`}
              </span>
            </div>
          </header>
          <VolumeCardBody
            available={data?.available ?? true}
            isError={isError}
            error={error}
            showChart={showChart}
            buckets={buckets}
            statusOk={statusOk}
            statusErr={statusErr}
            kind={chartKind}
          />
          {warehouseDown ? null : (
            <div className="trace-dashboard__legend" aria-hidden="true">
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
            </div>
          )}
        </section>
      </div>
    </div>
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

function VolumeCardBody({
  available,
  isError,
  error,
  showChart,
  buckets,
  statusOk,
  statusErr,
  kind,
}: {
  available: boolean;
  isError: boolean;
  error: unknown;
  showChart: boolean;
  buckets: import("../api").VolumeBucket[];
  statusOk: boolean;
  statusErr: boolean;
  kind: VolumeChartKind;
}) {
  if (isError) {
    return (
      <div className="panel-empty" data-testid="volume-chart-error">
        {error instanceof Error ? error.message : "Failed to load request volume."}
      </div>
    );
  }
  if (!available) {
    return (
      <div className="empty-card trace-dashboard__setup-card" data-testid="volume-chart-unavailable">
        <h3>ClickHouse required</h3>
        <p>
          Historical volume lives in ClickHouse. Set{" "}
          <code>NEXUS_CLICKHOUSE_URL</code> and restart to fill this chart.
        </p>
      </div>
    );
  }
  if (!showChart) {
    return (
      <div className="panel-empty" data-testid="volume-chart-empty">
        No data available
      </div>
    );
  }
  return (
    <TimeSeriesChart
      buckets={buckets}
      statusOk={statusOk}
      statusErr={statusErr}
      kind={kind}
    />
  );
}
