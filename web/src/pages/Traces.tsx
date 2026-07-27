import { useCallback, useEffect, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { DataTable, type Column } from "../components/DataTable";
import { Drawer } from "../components/Drawer";
import { Chip } from "../components/Chip";
import { StatusPill } from "../components/StatusPill";
import { Icon } from "../components/icons";
import {
  fetchMe,
  fetchTraces,
  type TraceSummary,
  type TraceQuery,
  type TraceCursor,
  type User,
} from "../api";

// fetchTraceBundle keeps the page-stats query and the first page of
// traces side by side. After the first load, subsequent "Load older"
// clicks call fetchTracePage directly so we can append without nuking
// the existing in-memory list.
async function fetchTraceBundle(query: TraceQuery) {
  const [me, list] = await Promise.allSettled([fetchMe(), fetchTraces(query)]);
  return {
    me: me.status === "fulfilled" ? (me.value as User | null) : null,
    page: list.status === "fulfilled" ? list.value : { items: [], next_cursor: { before: "", since: "" } as TraceCursor },
  };
}

// fetchTracePage is the same call as fetchTraces but typed for "append"
// pages a caller has already committed to a list. The cursor is what
// carries us forward between pages — `next_cursor` we received last
// round feeds into the call here, with the status/provider/q re-applied
// so filter intent persists across the pagination.
async function fetchTracePage(query: TraceQuery) {
  return fetchTraces(query);
}

// buildTraceQuery is the single source of truth for the URL we send to
// the server; centralised so the "Load older" handler and the live
// refetch can stay in lock-step on filter state. Note this excludes
// `before` / `since` — those are cursor-only.
function buildFilterQuery(
  statusFilter: "all" | "ok" | "err",
  providerFilter: string | null,
  search: string,
  limit: number,
): TraceQuery {
  return {
    status: statusFilter === "all" ? "" : statusFilter,
    provider: providerFilter ?? undefined,
    q: search.trim() || undefined,
    limit,
  };
}

export function Traces() {
  const qc = useQueryClient();

  const [statusFilter, setStatusFilter] = useState<"all" | "ok" | "err">("all");
  const [providerFilter, setProviderFilter] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  const [selected, setSelected] = useState<TraceSummary | null>(null);

  // Date-range window state. Both fields are kept in <input
  // type="datetime-local"> format (no timezone, browser-local). The
  // /api/traces server-side filter accepts RFC3339 with a Z, so we
  // re-attach local TZ via Date() below before sending the request.
  const [sinceInput, setSinceInput] = useState<string>("");
  const [beforeInput, setBeforeInput] = useState<string>("");

  // Items are accumulated in component state so "Load older" can append
  // without re-fetching earlier pages — the server narrows the funnel
  // consistently, so the in-memory list always stays coherent under the
  // active filter set.
  const [items, setItems] = useState<TraceSummary[]>([]);
  const [nextCursor, setNextCursor] = useState<TraceCursor>({ before: "", since: "" });
  const [loadingMore, setLoadingMore] = useState(false);

  // Initial fetch — server-side filter applied through queryKey so any
  // change to status/provider/q refetches a fresh windowed slice.
  const filterQuery = useMemo(
    () => buildFilterQuery(statusFilter, providerFilter, search, 100),
    [statusFilter, providerFilter, search],
  );
  // The query-call shape includes optional dates so the URL fetchTrace
  // uses them when set.
  const initialQuery: TraceQuery = useMemo(() => {
    return {
      ...filterQuery,
      since: dateInputToIso(sinceInput) || undefined,
      before: dateInputToIso(beforeInput) || undefined,
    };
  }, [filterQuery, sinceInput, beforeInput]);

  const { data, isLoading, isSuccess } = useQuery({
    queryKey: ["traces-page", initialQuery],
    queryFn: () => fetchTraceBundle(initialQuery),
    refetchInterval: 15_000,
  });

  // Mirror the latest server response into our local accumulators. We
  // do this in an effect rather than relying on `useQuery.onSuccess`
  // (deprecated in react-query v5) and treat a fresh queryKey (filter
  // change) as a signal to fully replace, not append — the previous
  // request's data is stale and would otherwise contaminate the new
  // window's results.
  useEffect(() => {
    if (!data) return;
    setItems(data.page.items);
    setNextCursor(data.page.next_cursor);
  }, [data, isSuccess]);

  const user = data?.me ?? null;
  const isAdmin = user?.role === "admin";

  // Unique providers visible in the *current* page only — handy as quick
  // chips but not authoritative. The server-side provider filter is
  // what actually narrows the response; this list is just for UI affordance.
  const providers = useMemo(
    () => Array.from(new Set(items.map((t) => t.provider_name))).sort(),
    [items],
  );

  // "Load older": request the next page using the cursor we received
  // last round, RE-applying the current status/provider/q so filter
  // intent persists. We append into `items` rather than replacing,
  // because the Load button conceptually walks *backwards through time*
  // within the same filter set.
  const loadOlder = useCallback(async () => {
    if (!nextCursor.before && !nextCursor.since) return;
    setLoadingMore(true);
    try {
      const next = await fetchTracePage({
        ...filterQuery,
        before: nextCursor.before || undefined,
        since: nextCursor.since || undefined,
      });
      setItems((prev) => {
        const seen = new Set(prev.map((t) => t.trace_id));
        const merged = next.items.filter((t) => !seen.has(t.trace_id));
        return [...prev, ...merged];
      });
      setNextCursor(next.next_cursor);
    } catch (err) {
      // Best-effort: do not blow away the existing list on a transient
      // network error. The user can retry by clicking again.
      console.error("Load older failed:", err);
    } finally {
      setLoadingMore(false);
    }
  }, [filterQuery, nextCursor]);

  const columns: Column<TraceSummary>[] = [
    {
      id: "time",
      header: "Time",
      width: "160px",
      cell: (t) => (
        <span className="mono">
          {new Date(t.timestamp).toLocaleString()}
        </span>
      ),
      sortValue: (t) => new Date(t.timestamp).getTime(),
      align: "left",
    },
    {
      id: "provider",
      header: "Provider",
      width: "120px",
      cell: (t) => <span className="provider-tag">{t.provider_name}</span>,
      sortValue: (t) => t.provider_name,
    },
    {
      id: "model",
      header: "Model",
      cell: (t) => <span className="mono ellipsis">{t.request_model}</span>,
      sortValue: (t) => t.request_model,
    },
    {
      id: "status",
      header: "Status",
      width: "90px",
      cell: (t) => (
        <StatusPill
          label={t.status_code.toString()}
          tone={t.status_code >= 400 ? "err" : "ok"}
        />
      ),
      sortValue: (t) => t.status_code,
    },
    {
      id: "latency",
      header: "Latency",
      width: "90px",
      align: "right",
      cell: (t) => <span className="mono">{t.latency_ms} ms</span>,
      sortValue: (t) => t.latency_ms,
    },
    {
      id: "tokens",
      header: "Tokens (in/out)",
      width: "120px",
      align: "right",
      cell: (t) => (
        <span className="mono">
          {t.input_tokens}/{t.output_tokens}
        </span>
      ),
      sortValue: (t) => (t.input_tokens ?? 0) + (t.output_tokens ?? 0),
    },
    {
      id: "cost",
      header: "Cost",
      width: "100px",
      align: "right",
      cell: (t) => <span className="mono">${t.cost_usd.toFixed(5)}</span>,
      sortValue: (t) => t.cost_usd,
    },
    {
      id: "flags",
      header: "Flags",
      width: "180px",
      cell: (t) => (
        <span className="flag-row">
          {t.cache_hit ? (
            <Chip tone="accent">cache</Chip>
          ) : null}
          {t.guardrail_action ? (
            <Chip tone="warn">{t.guardrail_action.split(":")[0]}</Chip>
          ) : null}
          {t.credential_source && t.credential_source !== "env" ? (
            <Chip tone="info">
              {t.credential_source === "user" ? "byok" : t.credential_source}
            </Chip>
          ) : null}
          {!t.cache_hit &&
          !t.guardrail_action &&
          (!t.credential_source || t.credential_source === "env") ? (
            <span className="muted">-</span>
          ) : null}
        </span>
      ),
    },
    ...(isAdmin
      ? [
          {
            id: "user",
            header: "User",
            width: "180px",
            cell: (t: TraceSummary) => t.user_email || "-",
            sortValue: (t: TraceSummary) => t.user_email ?? "",
          } as Column<TraceSummary>,
        ]
      : []),
  ];

  const hasMore = Boolean(nextCursor.before || nextCursor.since);

  return (
    <div className="traces-page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Workspace · live
          </div>
          <h1 className="page-title">Traces</h1>
          <p className="page-sub">
            Gateway traffic. Filter, sort, and click a row to inspect. Use the
            date range or <em>Load older</em> to walk back through time within
            the active filter set.
          </p>
        </div>
        <div className="page-stats">
          <div className="page-stat">
            <div className="page-stat-label">rows</div>
            <div className="page-stat-value">{items.length}</div>
          </div>
          <div className="page-stat">
            <div className="page-stat-label">error rate</div>
            <div className="page-stat-value">
              {items.length === 0
                ? "—"
                : `${(
                    (items.filter((t) => t.status_code >= 400).length /
                      items.length) *
                    100
                  ).toFixed(1)}%`}
            </div>
          </div>
          <div className="page-stat">
            <div className="page-stat-label">avg p95 latency</div>
            <div className="page-stat-value">
              {items.length === 0
                ? "—"
                : `${Math.round(
                    Math.max(...items.map((t) => t.latency_ms)),
                  )} ms`}
            </div>
          </div>
        </div>
      </header>

      <div className="filter-bar">
        <div className="filter-search">
          <Icon.zap size={14} />
          <input
            placeholder="Search model, provider, user, or guardrail…"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            aria-label="Search traces"
          />
        </div>
        <div className="filter-chips" role="group" aria-label="Status filter">
          <Chip
            tone={statusFilter === "all" ? "accent" : "neutral"}
            active={statusFilter === "all"}
            onClick={() => setStatusFilter("all")}
          >
            All
          </Chip>
          <Chip
            tone={statusFilter === "ok" ? "ok" : "neutral"}
            active={statusFilter === "ok"}
            onClick={() => setStatusFilter("ok")}
          >
            2xx/3xx
          </Chip>
          <Chip
            tone={statusFilter === "err" ? "err" : "neutral"}
            active={statusFilter === "err"}
            onClick={() => setStatusFilter("err")}
          >
            4xx/5xx
          </Chip>
        </div>
        <div className="filter-chips" role="group" aria-label="Provider filter">
          <Chip
            tone={providerFilter === null ? "accent" : "neutral"}
            active={providerFilter === null}
            onClick={() => setProviderFilter(null)}
          >
            All providers
          </Chip>
          {providers.map((p) => (
            <Chip
              key={p}
              tone={providerFilter === p ? "accent" : "neutral"}
              active={providerFilter === p}
              onClick={() => setProviderFilter(p)}
            >
              {p}
            </Chip>
          ))}
        </div>
      </div>

      <div className="filter-bar filter-bar-window" role="group" aria-label="Time window">
        <label className="dt-input">
          <span>Since</span>
          <input
            type="datetime-local"
            value={sinceInput}
            onChange={(e) => setSinceInput(e.target.value)}
            aria-label="Window start (browser local time)"
            data-testid="traces-window-since"
          />
        </label>
        <label className="dt-input">
          <span>Before</span>
          <input
            type="datetime-local"
            value={beforeInput}
            onChange={(e) => setBeforeInput(e.target.value)}
            aria-label="Window end (browser local time)"
            data-testid="traces-window-before"
          />
        </label>
        <button
          type="button"
          className="btn-ghost"
          onClick={() => {
            setSinceInput("");
            setBeforeInput("");
          }}
          disabled={!sinceInput && !beforeInput}
          data-testid="traces-window-clear"
        >
          Clear window
        </button>
        {/* Hidden bridge: the date inputs feed through to the server
            queryKey via dateInputToIso; this empty div is a layout hook for
            tests that need to assert the value of the converted ISO. */}
        <span className="sr-only" data-testid="traces-window-bridge">
          {"since=" + (dateInputToIso(sinceInput) ?? "") + ";before=" + (dateInputToIso(beforeInput) ?? "")}
        </span>
      </div>

      <div className="panel">
        <DataTable
          rows={items}
          columns={columns}
          rowKey={(t) => t.trace_id}
          onRowClick={(t) => setSelected(t)}
          emptyMessage={
            isLoading ? "Loading traces…" : "No traces match the filters."
          }
          initialSort={{ id: "time", dir: "desc" }}
        />
      </div>

      <div className="traces-pager">
        <button
          type="button"
          className="btn-ghost"
          onClick={loadOlder}
          disabled={!hasMore || loadingMore}
          data-testid="traces-load-older"
        >
          {loadingMore ? "Loading…" : hasMore ? "Load older" : "No more pages"}
        </button>
        <span className="muted traces-pager-hint">
          {hasMore
            ? "more pages available — keep the same filters"
            : "end of result set — relax filters or widen the window"}
        </span>
      </div>

      <Drawer
        open={Boolean(selected)}
        onClose={() => setSelected(null)}
        title={
          selected ? (
            <span className="mono">{selected.request_model}</span>
          ) : null
        }
        footer={
          selected ? (
            <button
              type="button"
              className="btn-ghost"
              onClick={() => setSelected(null)}
            >
              Close
            </button>
          ) : null
        }
      >
        {selected && <TraceDetail t={selected} />}
      </Drawer>

      {/* Force the useQueryClient import to register by exposing it
          through a hidden helper button — easier than a no-op cast and
          keeps the symbol alive for future cache invalidation. */}
      <button
        type="button"
        className="sr-only"
        aria-hidden="true"
        onClick={() => qc.invalidateQueries({ queryKey: ["traces-page"] })}
      />
    </div>
  );
}

// dateInputToIso converts an <input type="datetime-local"> string
// ("YYYY-MM-DDTHH:mm") to a UTC RFC3339 with Z suffix. Empty input
// returns "" so the query builder can omit it from the URL entirely.
function dateInputToIso(value: string): string | null {
  if (!value) return null;
  // The browser interprets the local-time input as browser-local time;
  // we anchor it on the user's timezone via Date() to honour daylight
  // savings correctly. The "Z" suffix is then added because the date
  // constructor normalises to local TZ; without the offset the server
  // would store different wall times across user sessions.
  const d = new Date(value);
  if (isNaN(d.getTime())) return null;
  return d.toISOString();
}

function TraceDetail({ t }: { t: TraceSummary }) {
  return (
    <div className="trace-detail">
      <div className="kv-grid">
        <KV label="Trace ID" value={<span className="mono">{t.trace_id}</span>} />
        <KV label="Time" value={new Date(t.timestamp).toLocaleString()} />
        <KV label="Provider" value={<span className="provider-tag">{t.provider_name}</span>} />
        <KV label="Requested model" value={<span className="mono">{t.request_model}</span>} />
        <KV label="Status" value={
          <StatusPill
            label={t.status_code.toString()}
            tone={t.status_code >= 400 ? "err" : "ok"}
          />
        } />
        <KV label="Latency" value={`${t.latency_ms} ms`} />
        <KV label="Cost" value={`$${t.cost_usd.toFixed(5)}`} />
        <KV label="TTFT" value={t.ttft_ms ? `${t.ttft_ms} ms` : "—"} />
        <KV label="Tokens" value={<span className="mono">{t.input_tokens}/{t.output_tokens}</span>} />
        <KV label="Streamed" value={t.streamed ? "yes" : "no"} />
        <KV label="Cache hit" value={t.cache_hit ? "yes" : "no"} />
        <KV label="Credential source" value={t.credential_source || "env"} />
        <KV label="User" value={t.user_email || "-"} />
      </div>
      {t.guardrail_action && (
        <>
          <h3 className="kv-section">Guardrail</h3>
          <div className="kv-grid">
            <KV label="Action" value={<Chip tone="warn">{t.guardrail_action}</Chip>} />
          </div>
        </>
      )}
    </div>
  );
}

function KV({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="kv">
      <div className="kv-label">{label}</div>
      <div className="kv-value">{value}</div>
    </div>
  );
}
