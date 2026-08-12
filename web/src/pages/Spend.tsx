import { useQuery } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import { Chip } from "../components/Chip";
import { DataTable, type Column } from "../components/DataTable";
import { Icon } from "../components/icons";
import {
  fetchMe,
  fetchMySpendBreakdown,
  fetchMySpendDaily,
  fetchMySpendSummary,
  fetchUserSpendBreakdown,
  fetchUserSpendDaily,
  fetchUserSpendSummary,
  fetchUsers,
  type DailySpendBreakdownRow,
  type DailySpendRow,
  type DailySpendSummary,
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

// spendBreakdownFetch picks the per-day breakdown fetcher for the
// current scope. `id == ""` keeps the call site ready for the not-
// logged-in branch (the page's <RequireAuth> already masks the UI).
function spendBreakdownFetch(scope: Scope, day: string): Promise<DailySpendBreakdownRow[]> {
  if (scope.kind === "me") return fetchMySpendBreakdown(day);
  return fetchUserSpendBreakdown(scope.id, day);
}

// spendSummaryFetch secures the hero rollup (current + previous
// window totals in one shot). The server-side plan combines the two
// intervals in a single ClickHouse query so the page doesn't issue a
// second trip just to render the savings pct.
function spendSummaryFetch(scope: Scope, days: number): Promise<DailySpendSummary> {
  if (scope.kind === "me") return fetchMySpendSummary(days);
  return fetchUserSpendSummary(scope.id, days);
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
  // `panelOpen` lets the user close the breakdown panel without
  // dropping the picked day entirely. The drill chip rail (below the
  // chart) keeps the picked day visible after a close, so the user can
  // click the chip to reopen the breakdown instead of having to
  // re-click the bar in the daily chart.
  const [panelOpen, setPanelOpen] = useState(false);

  const usersQuery = useQuery({
    queryKey: ["users"],
    queryFn: () => fetchUsers(),
    enabled: admin,
  });

  const dailyQuery = useQuery({
    queryKey: ["spend", "daily", scope, days],
    queryFn: () => spendFetch(scope, days),
  });

  // Summary backs the hero card. Its query key carries the same
  // [scope, days] pair as the daily list — switching the range or
  // scope chip triggers both fetches in lockstep, so the hero can't
  // display a savings figure that lags the chart behind it.
  const summaryQuery = useQuery({
    queryKey: ["spend", "summary", scope, days],
    queryFn: () => spendSummaryFetch(scope, days),
  });

  const breakdownQuery = useQuery({
    queryKey: ["spend", "breakdown", scope, pickedDay],
    queryFn: () => spendBreakdownFetch(scope, pickedDay!),
    // Fetch as soon as a day is picked; the panel may be closed
    // momentarily (`panelOpen === false`) but the cache is warm so
    // re-opening it is instant. Sending the fetch on close would
    // either be wasteful or wrong, so we keep the exchange simple:
    // pick ⇒ fetch + open; close ⇒ keep cache + hide panel.
    enabled: pickedDay !== null,
  });

  const pickDay = (d: string) => {
    setPickedDay((prev) => (prev === d ? null : d));
    setPanelOpen(true);
  };

  // Aggregates for the page hero stat strip. We compute them locally so
  // picking a range or scope rerenders the stats without an extra API.
  const aggregates = useMemo(() => aggregateDaily(dailyQuery.data ?? []), [dailyQuery.data]);

  return (
    <div className="spend-page">
      <SpendHero summary={summaryQuery.data ?? null} days={days} aggregates={aggregates} />

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
                setPanelOpen(false);
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
                setPanelOpen(false);
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
                  setPanelOpen(false);
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
          onPick={pickDay}
        />
        {pickedDay ? (
          <div
            className="spend-drilled-rail"
            data-testid="spend-drilled-rail"
            role="group"
            aria-label="Open drill-downs"
          >
            <span className="spend-drilled-rail__label">drilled</span>
            <span
              role="button"
              tabIndex={0}
              className="spend-drilled-chip"
              data-testid="spend-drilled-chip"
              aria-expanded={panelOpen}
              aria-label={`Reopen drill-down for ${pickedDay}`}
              onClick={() => setPanelOpen(true)}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  setPanelOpen(true);
                }
              }}
            >
              {pickedDay}
              <button
                type="button"
                className="spend-drilled-chip__remove"
                data-testid="spend-drilled-chip-remove"
                aria-label="Close drill-down"
                onClick={(e) => {
                  e.stopPropagation();
                  setPickedDay(null);
                  setPanelOpen(false);
                }}
              >
                ×
              </button>
            </span>
          </div>
        ) : null}
      </section>

      <section className="panel" aria-label="Daily spend list">
        <DataTable<DailySpendRow>
          rows={dailyQuery.data ?? []}
          columns={buildDailyColumns(pickDay)}
          rowKey={(r) => r.day}
          storageKey="spend-daily"
          pageSize={20}
        />
      </section>

      {pickedDay && panelOpen ? (
        <section className="panel" aria-label="Per-day breakdown">
          <header className="panel-head">
            <h2>{pickedDay} breakdown</h2>
            <button
              type="button"
              className="btn-ghost btn-small"
              onClick={() => setPanelOpen(false)}
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

// formatUsdLarge handles the hero-card numbers: short-form up to
// millions so the accent-gradient headline reads like a dashboard
// rather than a wall of cents. We trade off precision (max 2
// decimals) for legibility — the underlying summary endpoint already
// sent raw dollar amounts so the smaller tiles (Today / 7-day avg /
// Previous-window) keep their full precision via formatUsd.
function formatUsdLarge(n: number): string {
  if (!Number.isFinite(n)) return "$0.00";
  if (Math.abs(n) < 1000) return formatUsd(n);
  const units: Array<[number, string]> = [
    [1_000_000_000, "B"],
    [1_000_000, "M"],
    [1_000, "K"],
  ];
  for (const [unit, suffix] of units) {
    if (Math.abs(n) >= unit) {
      const v = n / unit;
      const digits = Math.abs(v) >= 100 ? 0 : Math.abs(v) >= 10 ? 1 : 2;
      return `$${v.toFixed(digits)}${suffix}`;
    }
  }
  return formatUsd(n);
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
  // The chart pads the top so the picked value label ("$1.23") sits
  // comfortably above its bar — picked → 22px clearance, axis labels
  // (day stamps) live in their own band below the axis so they never
  // collide with the bars or with the panel padding.
  const padTop = 22;
  const axisHeight = 18;
  const height = 168;
  const barWidth = (width - 8) / Math.max(1, rows.length);

  // Pick which day-stamps to print on the axis. Emitting every label at
  // 30-day density crowds the chart; we keep ~6 evenly-spaced labels
  // plus the first and last so the boundaries are always pinned.
  const axisStride = Math.max(1, Math.ceil(rows.length / 6));
  // Sanitize the picked day for the value label: NaN guards from the
  // sanitiser, plus a missing-pick case (no label emitted).
  const pickedRow = pickedDay
    ? rows.find((r) => r.day === pickedDay)
    : undefined;

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
              ? Math.max(2, (safe.cost_usd / maxCost) * (height - padTop - axisHeight - 4))
              : 2;
          const x = 4 + i * barWidth;
          const y = height - axisHeight - h;
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
        {/* Day-axis labels: stride-decorated so a 30-day strip stays
            legible. The labels live below the bars in their own band. */}
        {rows.map((r, i) => {
          // First or last day, plus every Nth in the middle. The
          // boundary days always print so a reader can see the window
          // edges without enlarging.
          const isEdge = i === 0 || i === rows.length - 1;
          const isStrideHit = i % axisStride === 0;
          if (!isEdge && !isStrideHit) return null;
          const cx = 4 + i * barWidth + barWidth / 2;
          return (
            <text
              key={`axis-${r.day}`}
              className="spend-bar__axis-label"
              x={cx}
              y={height - 4}
              textAnchor="middle"
            >
              {r.day.slice(5)}
            </text>
          );
        })}
        {/* Picked-bar value label: the only top-of-bar text we ever
            print. Keeps the chart visually quiet except where a reader
            has actively drilled in. */}
        {pickedRow ? (
          <text
            key={`val-${pickedRow.day}`}
            data-testid={`spend-bar-value-${pickedRow.day}`}
            className="spend-bar__value"
            x={4 + rows.findIndex((r) => r.day === pickedRow.day) * barWidth + barWidth / 2}
            y={Math.max(14, height - axisHeight - (chosenBarHeight(pickedRow, maxCost, height - padTop - axisHeight - 4)) - 6)}
            textAnchor="middle"
          >
            ${formatUsd(safeRow(pickedRow).cost_usd)}
          </text>
        ) : null}
      </svg>
    </div>
  );
}

// chosenBarHeight mirrors the bar-height math used in the render loop
// (so the value label sits exactly one row above the picked bar's top
// edge regardless of the picked row's cost). Centralising it here
// keeps the y-coordinate above the bar honest if the formula later
// changes.
function chosenBarHeight(
  row: DailySpendRow,
  maxCost: number,
  usable: number,
): number {
  const safe = safeRow(row);
  if (maxCost <= 0) return 2;
  return Math.max(2, (safe.cost_usd / maxCost) * usable);
}

// --- SpendHero ---------------------------------------------------------
//
// The page's hero block: a left-aligned title pair next to a 1×2 hero
// card grid. The dominant tile carries the trailing-N-days cost in
// the design-token accent gradient; the second tile carries the
// savings pct derived from the equal-length window immediately
// preceding it. Below that, four smaller tiles recap today / 7-day
// avg / cache responses / delta-cost — all cost-shaped, no tokens —
// so an operator looking at the page gets what the Spend page exists
// for in one glance without having to scroll.
//
// Layout contract: `.spend-hero` is a CSS grid — title column +
// hero cards column at the top, then the 4-chip strip below spanning
// both columns. Switching to flex would lose the right-edge alignment
// invariant between the dominant tile and the title block.
function SpendHero({
  summary,
  days,
  aggregates,
}: {
  summary: DailySpendSummary | null;
  days: number;
  aggregates: ReturnType<typeof aggregateDaily>;
}) {
  const hasPrevious = summary?.has_previous ?? false;
  const pct = summary?.savings_pct ?? 0;
  const delta = summary?.delta_cost_usd ?? 0;
  // HeroCost: the trailing N-days dollar amount in the accent
  // gradient. Falls back to the existing local aggregate when the
  // /summary endpoint is still resolving so the page never paints an
  // empty hero.
  const heroCost = summary ? summary.current_cost_usd : aggregates.totalCost;
  return (
    <div className="spend-page-head">
      <div className="spend-page-head__title">
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

      <div className="spend-hero-cards" aria-label="Cost hero summary">
        <article className="spend-hero-cost" data-testid="spend-hero-cost">
          <div className="spend-hero-cost__label">
            Last {days} days · total cost
          </div>
          <div className="spend-hero-cost__value mono">
            ${formatUsdLarge(heroCost)}
          </div>
          <div className="spend-hero-cost__sub">
            {hasPrevious ? (
              <span
                className={
                  "spend-hero-cost__delta" +
                  (delta >= 0 ? " is-up" : " is-down")
                }
              >
                <span className="spend-hero-cost__delta-arrow" aria-hidden="true">
                  {delta >= 0 ? "↑" : "↓"}
                </span>
                ${formatUsd(Math.abs(delta))}{" "}
                <span className="muted">vs previous {days} days</span>
              </span>
            ) : (
              <span className="muted">
                First window — comparison unlocks after the next {days} days
                of traffic
              </span>
            )}
          </div>
        </article>

        <article className="spend-hero-savings" data-testid="spend-hero-savings">
          <div className="spend-hero-card__label">Cost change · vs previous {days} days</div>
          <div
            className={
              "spend-hero-savings__value mono" +
              (hasPrevious ? (pct >= 0 ? " is-positive" : " is-negative") : " is-na")
            }
          >
            {hasPrevious ? `${pct >= 0 ? "+" : ""}${pct.toFixed(1)}%` : "—"}
          </div>
          <div className="spend-hero-savings__sub muted mono">
            {hasPrevious
              ? pct >= 0
                ? "cost decreased"
                : "cost increased"
              : "no previous window"}
          </div>
        </article>
      </div>

      <div className="spend-hero-strip" role="group" aria-label="Spend metric strip">
        <SpendMetric
          label="Today"
          value={formatUsd(aggregates.todayCost)}
          sub={`${aggregates.dayCount} days tracked`}
        />
        <SpendMetric
          label={`Previous ${days} days`}
          value={hasPrevious ? formatUsd(summary!.previous_cost_usd) : "—"}
          sub={`${(summary?.previous_requests ?? 0).toLocaleString()} req`}
        />
        <SpendMetric
          label="7-day avg"
          value={formatUsd(aggregates.last7AvgCost)}
          sub="trailing 7 days"
        />
        <SpendMetric
          label="Cache responses"
          value={aggregates.totalCacheHits.toLocaleString()}
          sub={`${(summary?.current_cache_hits ?? 0).toLocaleString()} this window`}
        />
      </div>
    </div>
  );
}

// SpendMetric is one of the four small tiles below the hero cards
// (Today / Previous / 7-day avg / Cache responses). Renders label
// uppercase + big numeric + tiny subline — same grid cell, all cost-
// shaped, no token-shaped lines so the strip reads consistently.
function SpendMetric({
  label,
  value,
  sub,
}: {
  label: string;
  value: string;
  sub: string;
}) {
  return (
    <div className="spend-hero-metric">
      <div className="spend-hero-metric__label">{label}</div>
      <div className="spend-hero-metric__value mono">{value}</div>
      <div className="spend-hero-metric__sub muted mono">{sub}</div>
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
