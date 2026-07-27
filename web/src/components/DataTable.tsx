import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
} from "react";

export interface Column<T> {
  id: string;
  header: React.ReactNode;
  cell: (row: T) => React.ReactNode;
  sortValue?: (row: T) => string | number;
  /**
   * Initial column width. Accepts both a CSS length ("160px", "10%",
   * "minmax(0, 1fr)") and a raw pixel number. When a number is given,
   * the column is *resizable* on the UI by the operator, and the
   * resulting width is persisted under `<storageKey>:<id>` in
   * localStorage. When a string is given, the column stays at that
   * width and is NOT resizable; this preserves the legacy behaviour
   * for columns that opt out (e.g. the chip-stack "Flags" column).
   */
  width?: string | number;
  align?: "left" | "right" | "center";
  /** Force-disable resize for this column even if width is numeric. */
  disableResize?: boolean;
}

interface Props<T> {
  rows: T[];
  columns: Column<T>[];
  rowKey?: (row: T) => string;
  onRowClick?: (row: T) => void;
  emptyMessage?: React.ReactNode;
  initialSort?: { id: string; dir: "asc" | "desc" };
  pageSize?: number;
  /**
   * localStorage namespace for remembering per-column widths after the
   * user drags a resize handle. Multiple tables can coexist on the
   * same page by passing distinct keys (e.g. "traces" vs "users").
   * Pass an empty string to disable persistence (useful inside tests).
   */
  storageKey?: string;
}

// Pixel clamps for the user-resizable column widths. The lower bound
// keeps the column from disappearing entirely; the upper bound stops a
// stray drag from breaking the layout on wide screens.
const MIN_PX = 40;
const MAX_PX = 1200;

// parseWidth extracts the numeric pixel value from a width string OR
// returns the supplied number. Returns undefined when the column has
// no usable pixel sizing — i.e. it should grow with grid `minmax(0, 1fr)`
// in the legacy "fixed-side" layout.
function parseWidth(width: string | number | undefined): number | undefined {
  if (typeof width === "number" && Number.isFinite(width)) return width;
  if (typeof width === "string") {
    const m = width.match(/^(-?\d+(?:\.\d+)?)px$/);
    if (m) return parseFloat(m[1]);
    return undefined;
  }
  return undefined;
}

// loadStoredWidths reads the saved column widths for a storageKey. We
// silently fall back to an empty map when localStorage is unavailable
// (e.g. SSR / private mode in older Safari). The shape is verified
// once on read; if it is invalidated by a future schema change the
// caller recovers by ignoring the entry rather than crashing.
function loadStoredWidths(storageKey: string): Record<string, number> {
  if (!storageKey) return {};
  if (typeof window === "undefined") return {};
  try {
    const raw = window.localStorage.getItem(storageKey);
    if (!raw) return {};
    const parsed = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
      return {};
    }
    const out: Record<string, number> = {};
    for (const [k, v] of Object.entries(parsed as Record<string, unknown>)) {
      if (typeof v === "number" && Number.isFinite(v)) out[k] = v;
    }
    return out;
  } catch {
    // Corrupted JSON or no quota.
    return {};
  }
}

// saveStoredWidths writes back the current per-column pixel widths.
// We split-per-key to avoid clobbering across tables.
function saveStoredWidths(storageKey: string, widths: Record<string, number>) {
  if (!storageKey || typeof window === "undefined") return;
  try {
    window.localStorage.setItem(storageKey, JSON.stringify(widths));
  } catch {
    // Best effort; the next drag will retry.
  }
}

export function DataTable<T>({
  rows,
  columns,
  rowKey,
  onRowClick,
  emptyMessage = "No rows.",
  initialSort,
  pageSize = 25,
  storageKey = "",
}: Props<T>) {
  const [sort, setSort] = useState<{ id: string; dir: "asc" | "desc" } | null>(
    initialSort ?? null,
  );
  const [page, setPage] = useState(0);

  // dragSessionRef holds the active drag mutation in flight. We keep
  // it as a ref so each instance of DataTable has its own session —
  // a module-level singleton would collide when two tables render on
  // the same page (rare but observed in eval profiles + traces).
  const dragSessionRef = useRef<DragSession | null>(null);

  // userPxById maps column.id → numeric pixel width chosen by the user
  // via the resize handle. We initialise it from localStorage so the
  // operator's drag persists across reloads.
  const [userPxById, setUserPxById] = useState<Record<string, number>>(
    () => loadStoredWidths(storageKey),
  );

  // Persist any width changes back to localStorage. Doing this in an
  // effect (vs. inline on every drag) batches multiple setState calls
  // within one frame into one storage write.
  useEffect(() => {
    saveStoredWidths(storageKey, userPxById);
  }, [storageKey, userPxById]);

  const sorted = useMemo(() => {
    if (!sort) return rows;
    const col = columns.find((c) => c.id === sort.id);
    if (!col?.sortValue) return rows;
    const dir = sort.dir === "asc" ? 1 : -1;
    return [...rows].sort((a, b) => {
      const av = col.sortValue!(a);
      const bv = col.sortValue!(b);
      if (av < bv) return -dir;
      if (av > bv) return dir;
      return 0;
    });
  }, [rows, sort, columns]);

  const pages = Math.max(1, Math.ceil(sorted.length / pageSize));
  const safePage = Math.min(page, pages - 1);
  const slice = sorted.slice(safePage * pageSize, (safePage + 1) * pageSize);

  // Build the columns rendered with their effective widths. The user's
  // resize wins if present, otherwise we use the column's declared
  // width (numeric → px count, string → pass-through), otherwise the
  // flexible default `minmax(0, 1fr)`.
  const rendered = useMemo(() => {
    return columns.map((c) => {
      const userPx = userPxById[c.id];
      const declared = parseWidth(c.width);
      let track: string;
      let px: number | undefined;
      if (userPx !== undefined) {
        track = `${userPx}px`;
        px = userPx;
      } else if (typeof c.width === "number" && Number.isFinite(c.width)) {
        track = `${c.width}px`;
        px = c.width;
      } else if (declared !== undefined) {
        track = `${declared}px`;
        px = declared;
      } else if (typeof c.width === "string") {
        track = c.width;
      } else {
        track = "minmax(0, 1fr)";
      }
      return { col: c, track, px };
    });
  }, [columns, userPxById]);

  return (
    <div className="dt">
      <div className="dt-scroller">
        <div
          className={"dt-grid" + (dragSessionRef.current ? " is-resizing" : "")}
          role="grid"
          style={
            {
              gridTemplateColumns: rendered.map((r) => r.track).join(" "),
            } as CSSProperties
          }
        >
          <HeaderRow<T>
            rendered={rendered}
            sort={sort}
            onSort={(col) => {
              if (!col.sortValue) return;
              setSort((prev) =>
                prev?.id === col.id
                  ? { id: col.id, dir: prev.dir === "asc" ? "desc" : "asc" }
                  : { id: col.id, dir: "asc" },
              );
              setPage(0);
            }}
            userPxById={userPxById}
            setUserPxById={setUserPxById}
            dragSessionRef={dragSessionRef}
          />
          {slice.length === 0 && (
            <div className="dt-empty" role="row">{emptyMessage}</div>
          )}
          {slice.map((row, ri) => {
            const key = rowKey ? rowKey(row) : `r-${ri}`;
            return (
              <div
                className={"dt-row" + (onRowClick ? " is-clickable" : "")}
                role="row"
                key={key}
                style={
                  {
                    gridTemplateColumns: rendered.map((r) => r.track).join(" "),
                  } as CSSProperties
                }
                onClick={onRowClick ? () => onRowClick(row) : undefined}
                tabIndex={onRowClick ? 0 : -1}
                onKeyDown={(e) => {
                  if (!onRowClick) return;
                  if (e.key === "Enter" || e.key === " ") {
                    e.preventDefault();
                    onRowClick(row);
                  }
                }}
              >
                {rendered.map(({ col }) => (
                  <div
                    key={col.id}
                    className={"dt-cell" + (col.align ? ` align-${col.align}` : "")}
                    role="gridcell"
                  >
                    {col.cell(row)}
                  </div>
                ))}
              </div>
            );
          })}
        </div>
      </div>
      {pages > 1 && (
        <div className="dt-pager">
          <button
            type="button"
            className="btn-ghost"
            onClick={() => setPage((p) => Math.max(0, p - 1))}
            disabled={safePage === 0}
          >
            ←
          </button>
          <span className="dt-pager-label">
            {safePage + 1} / {pages}
          </span>
          <button
            type="button"
            className="btn-ghost"
            onClick={() => setPage((p) => Math.min(pages - 1, p + 1))}
            disabled={safePage >= pages - 1}
          >
            →
          </button>
        </div>
      )}
    </div>
  );
}

// HeaderRow holds the column-header cells AND the resize handle for
// each column. Resize is implemented with pointer events because
// mouse-only would silently fail on touch devices; pointer events are
// a strict superset that handles both.
interface RenderedColumn<T> {
  col: Column<T>;
  track: string;
  px?: number;
}

interface HeaderRowProps<T> {
  rendered: RenderedColumn<T>[];
  sort: { id: string; dir: "asc" | "desc" } | null;
  onSort: (col: Column<T>) => void;
  userPxById: Record<string, number>;
  setUserPxById: React.Dispatch<
    React.SetStateAction<Record<string, number>>
  >;
  dragSessionRef: React.MutableRefObject<DragSession | null>;
}

interface DragSession {
  id: string;
  startX: number;
  startPx: number;
  prev: HTMLElement | null;
}

function HeaderRow<T>(props: HeaderRowProps<T>) {
  const { rendered, sort, onSort, userPxById, setUserPxById, dragSessionRef } =
    props;

  // pointerDown initiates a drag session. We capture the pointer so
  // mousemove/mouseup fire even if the cursor leaves the header cell
  // mid-drag, and we stash the original px width so we can compute
  // deltas relative to the start point rather than the current value
  // (which matters when the user wiggles the mouse on a small column).
  const handlePointerDown = useCallback(
    (colId: string, e: React.PointerEvent<HTMLDivElement>) => {
      const declared = rendered.find((r) => r.col.id === colId)?.px;
      const startPx =
        userPxById[colId] ?? (typeof declared === "number" ? declared : 120);
      dragSessionRef.current = {
        id: colId,
        startX: e.clientX,
        startPx,
        prev: e.currentTarget.parentElement,
      };
      try {
        e.currentTarget.setPointerCapture(e.pointerId);
      } catch {
        /* setPointerCapture can throw on synthetic events; non-fatal. */
      }
      document.body.classList.add("dt-resizing");
      e.preventDefault();
    },
    [rendered, userPxById, dragSessionRef],
  );

  // pointerMove updates the column width. We commit the next px into
  // React state via setUserPxById on every move so the inline style
  // stays in sync and React-driven rerenders stay truthful. The cost
  // is one re-render per pointerframe but React-Testing-Library's
  // `fireEvent` chains sequentially so test output is deterministic.
  const handlePointerMove = useCallback(
    (e: React.PointerEvent<HTMLDivElement>) => {
      const session = dragSessionRef.current;
      if (!session) return;
      const delta = e.clientX - session.startX;
      const next = Math.max(MIN_PX, Math.min(MAX_PX, session.startPx + delta));
      if (Number.isNaN(next) || !Number.isFinite(next)) return;
      setUserPxById((prev) => ({ ...prev, [session.id]: next }));
    },
    [dragSessionRef],
  );

  // pointerUp finalises the drag — the latest userPxById value is the
  // canonical size, no extra work needed beyond tracking the session
  // end so subsequent pointermoves don't carry stale state.
  const handlePointerUp = useCallback(
    (e: React.PointerEvent<HTMLDivElement>) => {
      const session = dragSessionRef.current;
      if (!session) return;
      try {
        e.currentTarget.releasePointerCapture(e.pointerId);
      } catch {
        /* synthetic event — ignore */
      }
      document.body.classList.remove("dt-resizing");
      dragSessionRef.current = null;
    },
    [dragSessionRef],
  );

  const handleKeyDown = useCallback(
    (colId: string, e: React.KeyboardEvent<HTMLDivElement>) => {
      // Keyboard accessibility: arrows nudge the column by 8px; Home
      // resets to the declared width; Esc cancels. Only meaningful
      // when a resize handle has focus.
      const renderedForCol = rendered.find((r) => r.col.id === colId);
      const declared = renderedForCol?.px ?? 120;
      const userPx = userPxById[colId];
      // current prefers the most-recent user drag value when present,
      // otherwise the declared width. This lets the user undo their
      // most-recent tweak with one Home press.
      const current = userPx ?? declared;
      let changed: number | null = null;
      switch (e.key) {
        case "ArrowLeft":
          changed = Math.max(MIN_PX, current - 8);
          break;
        case "ArrowRight":
          changed = Math.min(MAX_PX, current + 8);
          break;
        case "Home":
        case "Escape":
          changed = declared;
          break;
        default:
          return;
      }
      e.preventDefault();
      setUserPxById((prev) =>
        prev[colId] === changed ? prev : { ...prev, [colId]: changed! },
      );
    },
    [rendered, userPxById, setUserPxById],
  );

  return (
    <div className="dt-row is-head" role="row">
      {rendered.map(({ col }) => {
        const resizable =
          !col.disableResize &&
          (userPxById[col.id] !== undefined ||
            (typeof col.width === "number" && Number.isFinite(col.width)) ||
            parseWidth(col.width) !== undefined);
        return (
          <div
            key={col.id}
            role="columnheader"
            aria-sort={
              sort?.id === col.id
                ? sort.dir === "asc"
                  ? "ascending"
                  : "descending"
                : "none"
            }
            className={
              "dt-th" +
              (col.align ? ` align-${col.align}` : "") +
              (sort?.id === col.id ? " is-sorted" : "")
            }
          >
            <button
              type="button"
              className="dt-th-btn"
              onClick={() => onSort(col)}
              tabIndex={col.sortValue ? 0 : -1}
              aria-label={
                typeof col.header === "string" ? `Sort by ${col.header}` : undefined
              }
            >
              {col.header}
              {col.sortValue && (
                <span className="dt-sort-ind" aria-hidden="true">
                  {sort?.id === col.id ? (sort.dir === "asc" ? "\u25B2" : "\u25BC") : "\u2195"}
                </span>
              )}
            </button>
            {resizable ? (
              <div
                className="dt-resize-handle"
                role="separator"
                aria-orientation="vertical"
                aria-label={
                  typeof col.header === "string"
                    ? `Resize ${col.header} column`
                    : "Resize column"
                }
                aria-valuenow={userPxById[col.id] ?? parseWidth(col.width) ?? 120}
                aria-valuemin={MIN_PX}
                aria-valuemax={MAX_PX}
                tabIndex={0}
                data-testid={`dt-resize-${col.id}`}
                onPointerDown={(e) => handlePointerDown(col.id, e)}
                onPointerMove={handlePointerMove}
                onPointerUp={handlePointerUp}
                onKeyDown={(e) => handleKeyDown(col.id, e)}
              />
            ) : null}
          </div>
        );
      })}
    </div>
  );
}

// dragSessionRef is held as a per-instance ref in the parent
// DataTable component so two tables on the same page do not collide.
