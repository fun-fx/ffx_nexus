import { useQuery } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import { Chip } from "../components/Chip";
import { DataTable, type Column } from "../components/DataTable";
import { Icon } from "../components/icons";
import {
  fetchMe,
  fetchMySpendBreakdown,
  fetchMySpendDaily,
  fetchUserSpendBreakdown,
  fetchUserSpendDaily,
  fetchUsers,
  type DailySpendBreakdownRow,
  type DailySpendRow,
  type User,
} from "../api";

// One Spend page, two scopes:
//
//   - "me"      : the caller's own gateway_traces (admin or not)
//   - "user:<id>": an admin drilling into a member's traffic
//
// Admins also see an "All users" entry which sets scope to "me" but
// fetches the same dataset — useful as a default workspace view that
// does not require picking a member to inspect the org-wide chart.
type Scope =
  | { kind: "me" }
  | { kind: "user"; id: string };

function isAdmin(user: User | null): boolean {
  return Boolean(user && user.role === "admin");
}

// spendFetch lays out the per-scope fetch. When `id` is "" the caller is
// anonymous (not logged in) so we keep the call site ready for the
// not-logged-in branch (the page's <RequireAuth> already masks the UI).
function spendFetch(scope: Scope, days: number): Promise<DailySpendRow[]> {
  if (scope.kind === "me") return fetchMySpendDaily(days);
  return fetchUserSpendDaily(scope.id, days);
}

function spendBreakdownFetch(scope: Scope, day: string): Promise<DailySpendBreakdownRow[]> {
  if (scope.kind === "me") return fetchMySpendBreakdown(day);
  return fetchUserSpendBreakdown(scope.id, day);
}

const RANGE_DAYS = [7, 30, 90] as const;

export function Spend() {
  const meQuery = useQuery({ queryKey: ["me"], queryFn: () => fetchMe() });
  const admin = isAdmin(meQuery.data ?? null);

  // Admin scope switcher. Empty string means "self (default)". A user
  // id string drills into that member. Empty list of users → fall back
  // to self with a disabled note.
  const [scope, setScope] = useState<Scope>({ kind: "me" });
  const [days, setDays] = useState<number>(30);
  const [pickedDay, setPickedDay] = useState<string | null>(null);

  const usersQuery = useQuery({
    queryKey: ["users"],
    queryFn: () => fetchUsers(),
    enabled: admin,
  });

  const dailyQuery = useQuery({
    queryKey: ["spend", "daily", scope, days],
    queryFn: () => spendFetch(scope, days),
  });

  const breakdownQuery = useQuery({
    queryKey: ["spend", "breakdown", scope, pickedDay],
    queryFn: () => spendBreakdownFetch(scope, pickedDay!),
    enabled: pickedDay !== null,
  });

  // Aggregates for the page hero stat strip. We compute them locally so
  // picking a range or scope rerenders the stats without an extra API.
  const aggregates = useMemo(() => aggregateDaily(dailyQuery.data ?? []), [dailyQuery.data]);

  return (
    <div className="spend-page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <Icon.wallet size={14} />
            <span>Workspace · spend</span>
          </div>
          <h1 className="page-title">Spend</h1>
          <p className="page-sub">
            Per-day LLM cost from <code>gateway_traces</code>. Use the range
            chips or the scope switcher (admin) to slice the chart and the
            drill panel. Cache responses show up as <em>cache hits</em> in
            the daily list — they cost $0 upstream but still occupy capacity
            on the gateway.
          </p>
        </div>
        <div className="page-stats" aria-label="Spend summary">
          <SpendStat
            label={`Last ${days} days`}
            value={formatUsd(aggregates.totalCost)}
          />
          <SpendStat
            label="Today"
            value={formatUsd(aggregates.todayCost)}
          />
          <SpendStat
            label="7-day avg"
            value={formatUsd(aggregates.last7AvgCost)}
          />
          <SpendStat
            label="Cache responses"
            value={aggregates.totalCacheHits.toLocaleString()}
          />
        </div>
      </header>

      <div className="filter-bar" role="group" aria-label="Range and scope">
        <div className="filter-chips" role="group" aria-label="Range">
          {RANGE_DAYS.map((d) => (
            <Chip
              key={d}
              tone={days === d ? "accent" : "neutral"}
              active={days === d}
              data-testid={`spend-range-${d}`}
              onClick={() => {
                setDays(d);
                setPickedDay(null);
              }}
            >
              {d}d
            </Chip>
          ))}
        </div>
        {admin ? (
          <div className="filter-chips" role="group" aria-label="Scope">
            <Chip
              tone={scope.kind === "me" ? "accent" : "neutral"}
              active={scope.kind === "me"}
              data-testid="spend-scope-me"
              onClick={() => {
                setScope({ kind: "me" });
                setPickedDay(null);
              }}
            >
              {meQuery.data?.email ?? "Self"}
            </Chip>
            {(usersQuery.data ?? []).map((u) => (
              <Chip
                key={u.id}
                tone={
                  scope.kind === "user" && scope.id === u.id ? "accent" : "neutral"
                }
                active={scope.kind === "user" && scope.id === u.id}
                data-testid={`spend-scope-${u.id}`}
                onClick={() => {
                  setScope({ kind: "user", id: u.id });
                  setPickedDay(null);
                }}
              >
                {u.email}
              </Chip>
            ))}
          </div>
        ) : null}
      </div>

      <section className="panel" aria-label="Daily spend chart">
        <header className="panel-head">
          <h2>Daily cost</h2>
          <span className="panel-link muted">
            {dailyQuery.isFetching ? "loading…" : `${aggregates.dayCount} days`}
          </span>
        </header>
        <DailyBar
          rows={dailyQuery.data ?? []}
          pickedDay={pickedDay}
          onPick={(d) => setPickedDay(d === pickedDay ? null : d)}
        />
      </section>

      <section className="panel" aria-label="Daily spend list">
        <DataTable<DailySpendRow>
          rows={dailyQuery.data ?? []}
          columns={buildDailyColumns((d) =>
            setPickedDay((prev) => (prev === d ? null : d)),
          )}
          rowKey={(r) => r.day}
          storageKey="spend-daily"
          pageSize={20}
        />
      </section>

      {pickedDay ? (
        <section className="panel" aria-label="Per-day breakdown">
          <header className="panel-head">
            <h2>{pickedDay} breakdown</h2>
            <button
              type="button"
              className="btn-ghost btn-small"
              onClick={() => setPickedDay(null)}
              data-testid="spend-close-breakdown"
            >
              Close drill
            </button>
          </header>
          {breakdownQuery.isFetching ? (
            <div className="panel-loading">Loading breakdown…</div>
          ) : (breakdownQuery.data ?? []).length === 0 ? (
            <div className="panel-empty">
              No traffic on {pickedDay}. The day might pre-date the
              gateway_traces TTL or have nothing routed through the
              gateway.
            </div>
          ) : (
            <BreakdownPanel rows={breakdownQuery.data ?? []} />
          )}
        </section>
      ) : null}
    </div>
  );
}

// --- Helpers ------------------------------------------------------------

function formatUsd(n: number): string {
  if (!Number.isFinite(n)) return "$0.00";
  // Step up precision: small totals get 4 decimals so a $1.23 day is
  // legible, while big totals strip to whole cents.
  if (n < 1) return `$${n.toFixed(4)}`;
  if (n < 100) return `$${n.toFixed(2)}`;
  return `$${n.toLocaleString(undefined, { maximumFractionDigits: 2 })}`;
}

function aggregateDaily(rows: DailySpendRow[]) {
  let totalCost = 0;
  let totalCacheHits = 0;
  let todayCost = 0;
  let last7Sum = 0;
  let last7Count = 0;

  // Today: the row whose day matches today's UTC date (server side
  // produces server-UTC days). If the request just-launched today and
  // the day hasn't ticked over server-side yet, today's row may be
  // absent — that reads as 0.
  const today = new Date().toISOString().slice(0, 10);
  for (const r of rows) {
    totalCost += r.cost_usd;
    totalCacheHits += r.cache_hits;
    if (r.day === today) todayCost = r.cost_usd;
  }

  // last7Avg: aggregate the trailing 7 days even when fewer are present,
  // dividing by the count we have so a one-day window does not report
  // $0 avg.
  const tail = rows.slice(-7);
  for (const r of tail) {
    last7Sum += r.cost_usd;
    last7Count += 1;
  }
  const last7AvgCost = last7Count === 0 ? 0 : last7Sum / last7Count;

  return {
    totalCost,
    todayCost,
    last7AvgCost,
    totalCacheHits,
    dayCount: rows.length,
  };
}

// Filter out rows that have any non-finite numeric field so the chart
// does not draw with a NaN bar height. Cheap guard for the sanitiser
// path on the API side.
function safeRow(r: DailySpendRow): DailySpendRow {
  return {
    ...r,
    cost_usd: Number.isFinite(r.cost_usd) ? r.cost_usd : 0,
    tokens: Number.isFinite(r.tokens) ? r.tokens : 0,
    requests: Number.isFinite(r.requests) ? r.requests : 0,
    cache_hits: Number.isFinite(r.cache_hits) ? r.cache_hits : 0,
  };
}

// dailyColumns is the column set for the per-day list. Day and Cost
// are sortable; the action column opens the drill panel inline.
// `pick` is plumbed through so the cell button can call setPickedDay
// via closure rather than a synthetic DOM event (DataTable does not
// pass an onClick hook into column.cell).
function buildDailyColumns(pick: (day: string) => void): Column<DailySpendRow>[] {
  return [
    {
      id: "day",
      header: "Day",
      width: 130,
      align: "left",
      sortValue: (r) => r.day,
      cell: (r) => <span className="mono tabular">{r.day}</span>,
    },
    {
      id: "cost",
      header: "Cost (USD)",
      width: 130,
      align: "right",
      sortValue: (r) => r.cost_usd,
      cell: (r) => (
        <span className="mono tabular">${formatUsd(safeRow(r).cost_usd)}</span>
      ),
    },
    {
      id: "tokens",
      header: "Tokens",
      width: 110,
      align: "right",
      sortValue: (r) => r.tokens,
      cell: (r) => (
        <span className="mono tabular">{safeRow(r).tokens.toLocaleString()}</span>
      ),
    },
    {
      id: "requests",
      header: "Requests",
      width: 90,
      align: "right",
      sortValue: (r) => r.requests,
      cell: (r) => (
        <span className="mono tabular">{safeRow(r).requests.toLocaleString()}</span>
      ),
    },
    {
      id: "cache_hits",
      header: "Cache hits",
      width: 100,
      align: "right",
      sortValue: (r) => r.cache_hits,
      cell: (r) => (
        <span className="mono tabular">{safeRow(r).cache_hits.toLocaleString()}</span>
      ),
    },
    {
      id: "action",
      header: "",
      cell: (r) => (
        <button
          type="button"
          className="btn-ghost btn-small"
          data-testid={`spend-pick-${r.day}`}
          onClick={() => pick(r.day)}
        >
          Drill
        </button>
      ),
    },
  ];
}

// --- Daily bar (inline SVG) --------------------------------------------

function DailyBar({
  rows,
  pickedDay,
  onPick,
}: {
  rows: DailySpendRow[];
  pickedDay: string | null;
  onPick: (d: string) => void;
}) {
  if (rows.length === 0) {
    return (
      <div className="panel-empty">
        No spend yet in the selected window. Pick a wider range or wait
        for new traffic to land.
      </div>
    );
  }
  // Build a continuous axis so days with no traffic still occupy a slot
  // (renders as a 0-height bar). Days are server-side UTC strings in
  // YYYY-MM-DD order; we trust that and skip date math here.
  const maxCost = Math.max(0, ...rows.map((r) => safeRow(r).cost_usd));
  const width = 720;
  const height = 160;
  const padTop = 12;
  const barWidth = (width - 8) / Math.max(1, rows.length);

  return (
    <div className="spend-bar">
      <svg
        viewBox={`0 0 ${width} ${height}`}
        width="100%"
        height={height}
        role="img"
        aria-label="Daily cost bar chart"
      >
        {rows.map((r, i) => {
          const safe = safeRow(r);
          const h =
            maxCost > 0
              ? Math.max(2, (safe.cost_usd / maxCost) * (height - padTop - 4))
              : 2;
          const x = 4 + i * barWidth;
          const y = height - h;
          const isPicked = r.day === pickedDay;
          return (
            <g key={r.day}>
              <rect
                x={x}
                y={y}
                width={Math.max(1, barWidth - 2)}
                height={h}
                rx={2}
                data-testid={`spend-bar-${r.day}`}
                className={"spend-bar__rect" + (isPicked ? " is-picked" : "")}
                onClick={() => onPick(r.day)}
                role="button"
                aria-label={`Drill into ${r.day}`}
              />
              <title>
                {r.day} · {formatUsd(safe.cost_usd)} ·{" "}
                {safe.requests.toLocaleString()} req ·{" "}
                {safe.cache_hits.toLocaleString()} cache hits
              </title>
            </g>
          );
        })}
      </svg>
    </div>
  );
}

// --- SpendStat helper --------------------------------------------------

function SpendStat({ label, value }: { label: string; value: string }) {
  return (
    <div className="page-stat">
      <div className="page-stat-label">{label}</div>
      <div className="page-stat-value mono">{value}</div>
    </div>
  );
}

// --- Breakdown panel --------------------------------------------------

function BreakdownPanel({ rows }: { rows: DailySpendBreakdownRow[] }) {
  // Cumulative-share bar so the top of the list reads at a glance:
  // the longest bar is the biggest contributor. Cache-hit rows get
  // a distinct tone so a reader can skip past them when looking for
  // the actual bill.
  const totalCost = rows.reduce((acc, r) => acc + r.cost_usd, 0);
  const maxCost = Math.max(1e-9, ...rows.map((r) => r.cost_usd));
  return (
    <div className="spend-breakdown">
      {rows.map((r) => {
        const share = totalCost > 0 ? (r.cost_usd / totalCost) * 100 : 0;
        const bar = (r.cost_usd / maxCost) * 100;
        const isCacheOnly =
          r.cache_hits === r.requests && r.cost_usd === 0;
        return (
          <div
            key={`${r.model}-${r.provider}-${r.response_model ?? ""}`}
            className={"spend-row" + (isCacheOnly ? " is-cache-only" : "")}
            role="row"
          >
            <span className="spend-row__name">
              <span className="provider-tag">{r.provider}</span>
              <span className="mono spend-row__model">{r.model}</span>
              {r.response_model && r.response_model !== r.model ? (
                <span className="spend-row__responded" title="response_model">
                  → <span className="mono">{r.response_model}</span>
                </span>
              ) : null}
              {isCacheOnly ? (
                <Chip tone="accent">cache only</Chip>
              ) : null}
            </span>
            <span className="spend-row__bar" role="presentation">
              <span
                className="spend-row__bar-fill"
                style={{ width: `${Math.max(1, bar)}%` }}
              />
            </span>
            <span className="spend-row__cost mono">
              ${formatUsd(r.cost_usd)}
            </span>
            <span className="spend-row__share mono muted">
              {share.toFixed(1)}%
            </span>
            <span className="spend-row__requests muted mono">
              {r.requests.toLocaleString()} req ·{" "}
              {r.cache_hits.toLocaleString()} cache
            </span>
          </div>
        );
      })}
    </div>
  );
}
