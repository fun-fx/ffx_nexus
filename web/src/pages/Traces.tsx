import { useCallback, useEffect, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Chip } from "../components/Chip";
import { DataTable, type Column } from "../components/DataTable";
import { Drawer } from "../components/Drawer";
import { Icon } from "../components/icons";
import { ResizableGrid, type ColumnSpec, type RowSpec } from "../components/ResizableGrid";
import { StatusPill } from "../components/StatusPill";
import { formatExact, formatTimestampShort, formatTokens } from "../lib/format";
import {
  fetchMe,
  fetchTraces,
  type TraceQuery,
  type TraceCursor,
  type TraceSummary,
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

// TurnRow is the turn-grouped display unit. Either one TraceSummary that
// stood alone (turn_id empty OR singleton within the visible page) OR a
// sequence of calls sharing the same turn_id rolled up on the wire.
// trace_count reflects the visible-page slice; the inline expansion can
// still call fetchTraces({ turn }) to surface the calls the server
// windowed out of the initial page.
type TurnRow =
  | { kind: "single"; key: string; calls: [TraceSummary] }
  | { kind: "turn"; key: string; turnID: string; calls: TraceSummary[] };

// flattenToTurnRows mirrors Overview's grouped reading of a TraceSummary
// stream. Items keep insertion order so "Load older" appends in time
// direction without re-sorting. A row is "turn" when at least two
// adjacent calls share the same turn_id — singleton calls (turn_id
// missing OR unique in the visible slice) fall back to single-row mode so
// the cursor and Drawer behaviour stay 1:1 with the legacy table.
function flattenToTurnRows(items: TraceSummary[]): TurnRow[] {
  const out: TurnRow[] = [];
  let i = 0;
  while (i < items.length) {
    const t = items[i];
    const id = t.turn_id;
    // Singleton: empty turn_id, or the next item differs.
    if (!id || items[i + 1]?.turn_id !== id) {
      out.push({ kind: "single", key: t.trace_id, calls: [t] });
      i += 1;
      continue;
    }
    // Group: collect every consecutive item with this turn_id.
    let j = i + 1;
    while (j < items.length && items[j].turn_id === id) j += 1;
    const calls = items.slice(i, j);
    out.push({ kind: "turn", key: id, turnID: id, calls });
    i = j;
  }
  return out;
}

// groupStats takes the visible-page calls that share a turn_id and
// returns the aggregates the parent row shows. Worst-status picks the
// largest status_code so a single 5xx inside an otherwise-green turn
// still surfaces. timestamp is the LAST call's timestamp so the row
// sorts and reads the way it would on the overview panel.
function groupStats(calls: TraceSummary[]) {
  let worst = 0;
  let cost = 0;
  let latency = 0;
  let input = 0;
  let output = 0;
  let cacheHit = false;
  let guardrail: string | null = null;
  let cred: string | null = null;
  let lastAt = calls[0].timestamp;
  for (const c of calls) {
    if (c.status_code > worst) worst = c.status_code;
    cost += c.cost_usd;
    latency += c.latency_ms;
    input += c.input_tokens ?? 0;
    output += c.output_tokens ?? 0;
    if (c.cache_hit) cacheHit = true;
    if (c.guardrail_action && !guardrail) guardrail = c.guardrail_action;
    if (c.credential_source && c.credential_source !== "env" && !cred) {
      cred = c.credential_source;
    }
    if (c.timestamp > lastAt) lastAt = c.timestamp;
  }
  return {
    timestamp: lastAt,
    provider_name: calls[0].provider_name,
    request_model: calls[0].request_model,
    status_code: worst,
    latency_ms: latency,
    input_tokens: input,
    output_tokens: output,
    cost_usd: cost,
    cache_hit: cacheHit,
    guardrail_action: guardrail,
    credential_source: cred,
    user_id: calls[0].user_id,
    user_email: calls[0].user_email,
  };
}

// rolledUpTrace shapes a TraceSummary for a multi-call turn so DataTable
// can sort and click through it like any other row. The shape mirrors
// the singleton rows except for the synthetic `__rollup` and `__callCount`
// markers that drive the inline expansion branch in onRowClick and the
// added `Calls` column. The synthetic marker names are intentionally
// dual-underscore-prefixed so a future TraceSummary schema field could
// not collide accidentally.
export function rolledUpTrace(calls: TraceSummary[]): TraceSummary & {
  __rollup: true;
  __turnID: string;
  __callCount: number;
} {
  const s = groupStats(calls);
  const latest = calls[0];
  const merged = {
    ...latest,
    timestamp: s.timestamp,
    status_code: s.status_code,
    latency_ms: s.latency_ms,
    input_tokens: s.input_tokens,
    output_tokens: s.output_tokens,
    cost_usd: s.cost_usd,
    cache_hit: s.cache_hit ? 1 : 0,
    guardrail_action: s.guardrail_action ?? "",
    credential_source: s.credential_source ?? "",
    __rollup: true as const,
    __turnID: calls[0].turn_id,
    __callCount: calls.length,
  };
  return merged as unknown as TraceSummary & {
    __rollup: true;
    __turnID: string;
    __callCount: number;
  };
}

export function Traces() {
  const qc = useQueryClient();

  const [statusFilter, setStatusFilter] = useState<"all" | "ok" | "err">("all");
  const [providerFilter, setProviderFilter] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  const [selected, setSelected] = useState<TraceSummary | null>(null);
  // Turn row whose calls are currently expanded in inline mode. We
  // model this separately from the singleton Drawer so a turn can be
  // expanded while another trace sits in the Drawer.
  const [expandedTurn, setExpandedTurn] = useState<string | null>(null);

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
  // window's results. We also collapse expansion: a filter change
  // changes the visible-page slice, so the previously-expanded turn is
  // almost certainly no longer in the new view.
  useEffect(() => {
    if (!data) return;
    setItems(data.page.items);
    setNextCursor(data.page.next_cursor);
    setExpandedTurn(null);
  }, [data, isSuccess]);

  // The admin "User" column only renders when the caller is privileged;
  // pulling `me` off the bundle keeps the data flight minimal while
  // letting the column handler pick the role on every render.
  const user: User | null = data?.me ?? null;

  // Unique providers visible in the *current* page only — handy as quick
  // chips but not authoritative. The server-side provider filter is
  // what actually narrows the response; this list is just for UI affordance.
  const providers = useMemo(
    () => Array.from(new Set(items.map((t) => t.provider_name))).sort(),
    [items],
  );

  // turnRows is a parallel array of TraceSummary-shaped records that
  // DataTable can sort and click without a custom renderer. Single
  // calls pass through untouched. Turn buckets roll up to the latest call's
  // rowKey (so a stable Drawer surface survives split/de-dup on
  // refetch) with merged token sums, latest-at-first time, worst status,
  // and total cost — sortValue hooks read those merged numbers so a
  // column click never silently swaps a roll-up for a single value.
  // `raw` keeps the underlying buckets around for the inline expansion.
  const turnRows = useMemo(() => flattenToTurnRows(items), [items]);
  const displayRows: TraceSummary[] = useMemo(
    () => turnRows.map((row) => (row.kind === "single" ? row.calls[0] : rolledUpTrace(row.calls))),
    [turnRows],
  );

  // Inline call list under an expanded turn row. Empty outside of an
  // expansion so we don't fire the network on every filter change.
  const callsFor = useCallback((turnID: string): TraceQuery => {
    // Mirror the active filters into the inline expansion so the
    // operator's status/provider/q/date intent carries down. Limit is
    // generous so a multi-call agent run fits in one round-trip.
    return {
      ...filterQuery,
      limit: 50,
      turn: turnID,
      since: dateInputToIso(sinceInput) || undefined,
      before: dateInputToIso(beforeInput) || undefined,
    };
  }, [filterQuery, sinceInput, beforeInput]);

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

  const hasMore = Boolean(nextCursor.before || nextCursor.since);

  // Stats pinned on the page header use the SAME aggregates the rows
  // show, so error rate / max latency stay consistent with turn-grouped
  // display: error rate is over calls (not over rows), max is over the
  // worst latency in the visible page.
  const totalCalls = items.length;
  const errorCalls = items.filter((t) => t.status_code >= 400).length;
  const maxLatency = items.length === 0 ? 0 : Math.max(...items.map((t) => t.latency_ms));

  // Column shape for the sortable DataTable. Calls comes first so
  // multi-call rows are easy to spot; sortValue operates on the rolled-up
  // numbers because single calls would otherwise bubble into the middle
  // of a same-turn group.
  const columns: Column<TraceSummary>[] = [
    {
      id: "time",
      header: "Time",
      width: 140,
      cell: (t) => {
        // Cell shows a short wall-clock form so the column does not
        // wrap "Aug 11, 2026, 6:51:24 PM" across five lines; the
        // full timestamp stays on the title= tooltip for forensics.
        const full = new Date(t.timestamp).toLocaleString();
        return (
          <span className="mono tabular" title={full}>
            {formatTimestampShort(t.timestamp)}
          </span>
        );
      },
      sortValue: (t) => new Date(t.timestamp).getTime(),
      align: "left",
    },
    {
      id: "provider",
      header: "Provider",
      width: 110,
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
      id: "calls",
      header: "Calls",
      width: 70,
      align: "right",
      cell: (t) => {
        const typed = t as TraceSummary & {
          __rollup?: true;
          __callCount?: number;
        };
        const isRollup = typed.__rollup === true;
        return (
          <span
            className={isRollup ? "turn-calls mono" : "muted mono"}
            title={isRollup ? "Multi-call turn — click to expand" : undefined}
          >
            {isRollup ? (typed.__callCount ?? 0) : "1"}
          </span>
        );
      },
      sortValue: (t) => {
        const typed = t as TraceSummary & {
          __rollup?: true;
          __callCount?: number;
        };
        return typed.__rollup === true ? typed.__callCount ?? 0 : 1;
      },
    },
    {
      id: "status",
      header: "Status",
      width: 90,
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
      width: 90,
      align: "right",
      cell: (t) => <span className="mono">{t.latency_ms} ms</span>,
      sortValue: (t) => t.latency_ms,
    },
    {
      id: "tokens",
      header: "Tokens (in/out)",
      width: 130,
      align: "right",
      cell: (t) => (
        <span
          className="mono"
          title={`in ${formatExact(t.input_tokens ?? 0)} • out ${formatExact(t.output_tokens ?? 0)}`}
        >
          {formatTokens(t.input_tokens ?? 0)}/{formatTokens(t.output_tokens ?? 0)}
        </span>
      ),
      sortValue: (t) => (t.input_tokens ?? 0) + (t.output_tokens ?? 0),
    },
    {
      id: "cost",
      header: "Cost",
      width: 110,
      align: "right",
      cell: (t) => <span className="mono">${t.cost_usd.toFixed(5)}</span>,
      sortValue: (t) => t.cost_usd,
    },
    {
      id: "flags",
      header: "Flags",
      width: 180,
      cell: (t) => (
        <span className="flag-row">
          {t.cache_hit ? <Chip tone="accent">cache</Chip> : null}
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
    ...(user?.role === "admin"
      ? [
          {
            id: "user",
            header: "User",
            width: 180,
            cell: (t: TraceSummary) => t.user_email || "-",
            sortValue: (t: TraceSummary) => t.user_email ?? "",
            disableResize: true,
          } as Column<TraceSummary>,
        ]
      : []),
  ];

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
            the active filter set. Rows with multiple calls are agent turns —
            click to drill into the underlying calls.
          </p>
        </div>
        <div className="page-stats">
          <div className="page-stat">
            <div className="page-stat-label">rows</div>
            <div className="page-stat-value">{turnRows.length}</div>
          </div>
          <div className="page-stat">
            <div className="page-stat-label">calls</div>
            <div className="page-stat-value">{totalCalls}</div>
          </div>
          <div className="page-stat">
            <div className="page-stat-label">error rate</div>
            <div className="page-stat-value">
              {totalCalls === 0
                ? "—"
                : `${((errorCalls / totalCalls) * 100).toFixed(1)}%`}
            </div>
          </div>
          <div className="page-stat">
            <div className="page-stat-label">max latency</div>
            <div className="page-stat-value">
              {maxLatency === 0 ? "—" : `${Math.round(maxLatency)} ms`}
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
        <DataTable<TraceSummary>
          rows={displayRows}
          columns={columns}
          rowKey={(t) =>
            (t as TraceSummary & { __rollup?: boolean; __turnID?: string }).__rollup
              ? "turn:" + ((t as TraceSummary & { __turnID?: string }).__turnID ?? t.trace_id)
              : t.trace_id
          }
          rowTestId={(t) => {
            const typed = t as TraceSummary & {
              __rollup?: true;
              __turnID?: string;
            };
            if (typed.__rollup && typed.__turnID) {
              return "traces-turn-row-" + typed.__turnID;
            }
            return "traces-single-row-" + t.trace_id;
          }}
          onRowClick={(t) => {
            const typed = t as TraceSummary & {
              __rollup?: true;
              __turnID?: string;
            };
            const turnID =
              typed.__rollup && typeof typed.__turnID === "string"
                ? typed.__turnID
                : null;
            if (turnID !== null) {
              setExpandedTurn((prev) => (prev === turnID ? null : turnID));
            } else {
              setSelected(t);
            }
          }}
          emptyMessage={
            isLoading ? "Loading traces…" : "No traces match the filters."
          }
          initialSort={{ id: "time", dir: "desc" }}
          storageKey="nexus:dt:traces"
        />
      </div>

      {/* Inline expansion lives under the DataTable row when a turn is
          open. We deliberately render it outside the table because the
          drill-down is N child rows, not part of the sortable header
          grid; DataTable owns the header/footer/paging chrome. */}
      {expandedTurn && turnRows.find((r) => r.kind === "turn" && r.turnID === expandedTurn)?.kind === "turn" && (
        <InlineExpansion
          turnID={expandedTurn as string}
          query={callsFor(expandedTurn as string)}
        />
      )}

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

// TurnCallsInline is the Traces-page equivalent of Overview's TurnCalls:
// mounted only while a turn row is expanded, so collapsing and
// re-expanding refetches — desired on a live console where a turn is
// still being recorded. Re-applies the active filter set so the inline
// list honours the user's status/provider/q/date intent.
//
// Sub-row template drops Provider and Calls (every call in a turn shares
// the parent's provider; a call has no sub-count). The grid still carries
// Time, Provider, Model, Status, Latency, Tokens, Cost (7 columns) so
// Status/Latency/Tokens/Cost align with the parent DataTable positions
// 5–8; the first cell takes colSpan: 2 to absorb the time+provider slot
// visually, keeping Model column-aligned on a wider screen.
const TRACES_SUB_COLUMNS: ColumnSpec[] = [
  { id: "time", header: "Time", initialWidth: 160 },
  { id: "provider", header: "Provider", initialWidth: 110 },
  { id: "model", header: "Model", initialWidth: "minmax(180px, 1.4fr)" },
  { id: "status", header: "Status", initialWidth: 90 },
  { id: "latency", header: "Latency", initialWidth: 90, align: "right" },
  { id: "tokens", header: "Tokens", initialWidth: 130, align: "right" },
  { id: "cost", header: "Cost", initialWidth: 110, align: "right" },
];

function InlineExpansion({ turnID, query }: { turnID: string; query: TraceQuery }) {
  const { data, isLoading } = useQuery({
    queryKey: ["turn-calls", turnID, query],
    queryFn: () => fetchTraces(query),
  });
  // Server orders newest-first for paging. Inside a turn we want the
  // agent's own sequence, so read it back the other way.
  const calls: TraceSummary[] = [...(data?.items ?? [])].reverse();

  const subRows: RowSpec[] = calls.map((c, i) => ({
    rowKey: c.trace_id,
    className: "trace-row sub-row",
    cells: [
      // colSpan: 2 absorbs the parent's time+provider slot — the sub-
      // shape doesn't render Provider (every call in a turn shares the
      // parent's provider), so we collapse those two grid columns on
      // a single cell. Model / Status / Latency / Tokens / Cost then
      // align under the parent DataTable's columns 3 / 5–8.
      {
        colSpan: 2,
        node: (
          <span className="muted">
            #{i + 1} · {new Date(c.timestamp).toLocaleTimeString()}
          </span>
        ),
      },
      {
        node: (
          <span className="mono ellipsis rg-cell-truncate" title={c.request_model}>
            {c.request_model}
          </span>
        ),
      },
      {
        node: (
          <StatusPill
            label={c.status_code.toString()}
            tone={c.status_code >= 400 ? "err" : "ok"}
          />
        ),
      },
      {
        node: <span className="right mono">{c.latency_ms} ms</span>,
      },
      {
        node: (
          <span
            className="right mono"
            title={`in ${formatExact(c.input_tokens ?? 0)} • out ${formatExact(c.output_tokens ?? 0)}`}
          >
            {formatTokens(c.input_tokens ?? 0)}/{formatTokens(c.output_tokens ?? 0)}
          </span>
        ),
      },
      {
        node: (
          <span className="right mono">${Number(c.cost_usd ?? 0).toFixed(5)}</span>
        ),
      },
    ],
  }));

  if (isLoading) {
    return (
      <div className="session-drill muted" data-testid={`turn-calls-${turnID}`}>
        Loading calls…
      </div>
    );
  }
  if (calls.length === 0) {
    return (
      <div className="session-drill muted" data-testid={`turn-calls-${turnID}`}>
        No calls found.
      </div>
    );
  }
  return (
    <div className="session-drill" data-testid={`turn-calls-${turnID}`}>
      <ResizableGrid
        columns={TRACES_SUB_COLUMNS}
        storageKey="nexus:rg:traces-inline"
        groups={[{ showHeader: false, rows: subRows }]}
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
        <KV label="Tokens" value={<span className="mono">{formatExact(t.input_tokens ?? 0)}/{formatExact(t.output_tokens ?? 0)}</span>} />
        <KV label="Streamed" value={t.streamed ? "yes" : "no"} />
        <KV label="Cache hit" value={t.cache_hit ? "yes" : "no"} />
        <KV label="Credential source" value={t.credential_source || "env"} />
        <KV label="User" value={t.user_email || "-"} />
        <KV label="Turn ID" value={t.turn_id ? <span className="mono">{t.turn_id}</span> : "—"} />
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
