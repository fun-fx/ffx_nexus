import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode,
} from "react";

// Pixel clamps for the user-resizable column widths. The lower bound
// keeps the column from disappearing entirely; the upper bound stops a
// stray drag from breaking the layout on wide screens.
const MIN_PX = 40;
const MAX_PX = 1200;

// Bound a candidate pixel width to the allowed range; NaN / non-finite
// inputs fall back to `fallback` so a corrupted storage entry cannot
// collapse a column to zero.
const clampPx = (n: number, fallback: number): number => {
  if (!Number.isFinite(n)) return fallback;
  return Math.max(MIN_PX, Math.min(MAX_PX, Math.round(n)));
};

// parseWidth extracts the pixel value from a column's `width` setting.
// Strings like "84px" map to a number; flexible strings (minmax / %)
// return undefined so the column is treated as non-resizable.
function parseWidth(width: WidthSetting | undefined): number | undefined {
  if (width === undefined) return undefined;
  if (typeof width === "number" && Number.isFinite(width)) return width;
  if (typeof width === "string") {
    const m = width.match(/^(-?\d+(?:\.\d+)?)px$/);
    if (m) return parseFloat(m[1]);
    return undefined;
  }
  return undefined;
}

export type WidthSetting = string | number;

export interface ColumnSpec {
  id: string;
  /** Header label rendered on top of the data rows. */
  header: ReactNode;
  /** Initial width when nothing is persisted; either a px number or a
   *  flexible CSS length (e.g. "minmax(0, 1fr)"). */
  initialWidth: WidthSetting;
  /** Optional minimum pixel width when the user resizes. Defaults to
   *  40 when omitted. The hard ceiling is always MAX_PX. */
  minWidth?: number;
  /** Optional maximum pixel width override; falls back to MAX_PX. */
  maxWidth?: number;
  /** Optional cell alignment marker carried through to the rendered
   *  cell via class — let callers opt into right/center align without
   *  prop-drilling per row. */
  align?: "left" | "right" | "center";
}

export interface CellContent {
  /** When set, this cell merges across multiple grid columns using
   *  `gridColumn: 1 / span N`. Lets sub-shapes absorb the columns a
   *  parent shape renders but the sub-shape chooses not to display. */
  colSpan?: number;
  node: ReactNode;
  /**
   * Per-cell alignment override. When omitted, the column's `align`
   * marker is the tiebreaker; when both are absent the cell stays
   * left-aligned to match the legacy `.trace-row` typography.
   */
  align?: "left" | "right" | "center";
}

export interface RowSpec {
  /** Stable key for React reconciliation. Must be unique per row in a
   *  single ResizableGrid instance. */
  rowKey: string;
  /** testid applied to the row container so tests can target it. */
  rowTestId?: string;
  /**
   * Cell array. Each entry is one column by default; passing `colSpan`
   * on a CellContent lets the caller merge consecutive columns (used
   * by sub-row templates to combine the parent's Provider+Time slot,
   * for example).
   *
   * Length should equal `columns.length` once you sum in every
   * colSpan'd cell; otherwise the trailing slots become empty rather
   * than overflowing visually.
   */
  cells: CellContent[];
  /** Optional class hook for the row container; lets callers add
   *  is-expandable / is-open etc. without prop drilling. */
  className?: string;
  /** Optional onClick handler — a button-like row toggling. */
  onClick?: () => void;
  /** Optional role for accessibility; defaults to "row". */
  role?: string;
  /** Optional tabIndex override; numbers >= 0 make the row keyboard
   *  focusable. */
  tabIndex?: number;
  /** aria-expanded hook for button-shaped rows. */
  ariaExpanded?: boolean;
}

interface GroupSpec {
  /**
   * Show the header row above the rows. Caller flips this to false
   * for sub-shapes so the parent's header is the only one rendered.
   * Defaults to true.
   */
  showHeader?: boolean;
  /** Header cells align with `CellContent`, so a custom header can be
   *  declared per-column if the simple `columns[i].header` is not
   *  enough (e.g. when the caller wants colSpan'd headers). */
  customHeader?: CellContent[];
  rows: RowSpec[];
}

interface Props {
  columns: ColumnSpec[];
  /**
   * Where to persist user-resized widths. Per-column entries are saved
   * under `<storageKey>:<columnId>` in localStorage when the caller
   * doesn't pass `defaultUserPx` (our internal map).
   */
  storageKey: string;
  /**
   * Optional external width map — typically fed in by a parent that
   * wants to coordinate widths across two grids (sub-row's widths
   * match the parent grid's widths). The shape mirrors what the
   * component would load from localStorage under `storageKey`.
   */
  defaultUserPx?: Record<string, number>;
  /**
   * Render multiple shapes (turn-row + sub-row, etc.) in one virtual
   * table. Each group's header is independent, so callers usually pass
   * a single group for top-level rendering and a separate ResizableGrid
   * for the drill-down with `showHeader: false`.
   */
  groups: GroupSpec[];
  /** CSS class applied to the outer grid container — lets callers
   *  plug into existing layout containers (`.trace-table`). */
  className?: string;
}

interface DragSession {
  colId: string;
  startX: number;
  startPx: number;
}

interface RenderedColumn {
  col: ColumnSpec;
  track: string;
  px: number | undefined;
}

// loadStoredWidths reads the saved column widths under storageKey. We
// silently fall back to an empty map when localStorage is unavailable
// (private mode, SSR).
function loadStoredWidths(storageKey: string): Record<string, number> {
  if (!storageKey || typeof window === "undefined") return {};
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
    return {};
  }
}

// saveStoredWidths writes back the current per-column pixel widths.
// Best effort — quota or private-mode failures are non-fatal.
function saveStoredWidths(storageKey: string, widths: Record<string, number>) {
  if (!storageKey || typeof window === "undefined") return;
  try {
    window.localStorage.setItem(storageKey, JSON.stringify(widths));
  } catch {
    /* swallow — next drag will retry */
  }
}

export function ResizableGrid({
  columns,
  storageKey,
  defaultUserPx,
  groups,
  className,
}: Props) {
  const dragSessionRef = useRef<DragSession | null>(null);

  // userPxById maps column.id → numeric pixel width chosen by the user
  // via the resize handle. We seed it from localStorage so the
  // operator's drag persists across reloads, and let `defaultUserPx`
  // override on initial render (sub-row mirrors its parent at first
  // mount so they line up visually).
  const [userPxById, setUserPxById] = useState<Record<string, number>>(() => {
    const stored = loadStoredWidths(storageKey);
    if (defaultUserPx) {
      return { ...stored, ...defaultUserPx };
    }
    return stored;
  });

  // Persist any width changes back to localStorage. Doing this in an
  // effect (vs. inline on every drag) lets React batch multiple
  // setState calls within one frame into one storage write.
  useEffect(() => {
    saveStoredWidths(storageKey, userPxById);
  }, [storageKey, userPxById]);

  // rendered[i].track is the CSS grid track contribution for column i.
  // Last column's track is minmax(0, 1fr) so the grid still absorbs
  // remaining horizontal slack without producing a horizontal scroll;
  // every other column is a px number so the user resize handle is
  // meaningful on the visual edge.
  const rendered = useMemo<RenderedColumn[]>(() => {
    const lastIndex = columns.length - 1;
    return columns.map((col, i) => {
      const userPx = userPxById[col.id];
      const pxDefault =
        typeof col.initialWidth === "number" && Number.isFinite(col.initialWidth)
          ? col.initialWidth
          : parseWidth(col.initialWidth);
      const declared = pxDefault ?? 120;
      const effective = userPx !== undefined ? clampPx(userPx, declared) : undefined;
      const isLast = i === lastIndex;
      let track: string;
      let px: number | undefined;
      if (effective !== undefined) {
        track = `${effective}px`;
        px = effective;
      } else if (typeof col.initialWidth === "string") {
        track = col.initialWidth;
      } else {
        track = "minmax(0, 1fr)";
      }
      // Override the last column with a flexible track so the grid
      // remains full-width even after the user has dragged every other
      // column narrower than the container.
      if (isLast) {
        track = "minmax(0, 1fr)";
      }
      return { col, track, px };
    });
  }, [columns, userPxById]);

  const updatePx = useCallback(
    (colId: string, nextPx: number) => {
      setUserPxById((prev) => ({ ...prev, [colId]: nextPx }));
    },
    [],
  );

  // pointerDown initiates a drag session. We stash the start coords and
  // pixel width so we can compute deltas relative to the start point
  // rather than the live value (matters when the user wiggles the mouse
  // on a small column where every pixel counts).
  const handlePointerDown = useCallback(
    (colId: string, e: React.PointerEvent<HTMLDivElement>) => {
      const renderedForCol = rendered.find((r) => r.col.id === colId);
      const declared =
        typeof renderedForCol?.col.initialWidth === "number"
          ? renderedForCol.col.initialWidth
          : parseWidth(renderedForCol?.col.initialWidth) ?? 120;
      const startPx = userPxById[colId] ?? declared;
      dragSessionRef.current = {
        colId,
        startX: e.clientX,
        startPx,
      };
      try {
        e.currentTarget.setPointerCapture(e.pointerId);
      } catch {
        /* synthetic events can throw — non-fatal */
      }
      document.body.classList.add("rg-resizing");
      e.preventDefault();
    },
    [rendered, userPxById],
  );

  // pointerMove updates the candidate column width live. The result is
  // committed to React state on every move so the inline
  // gridTemplateColumns stays in sync — this drives the header / row
  // geometry in lock-step.
  const handlePointerMove = useCallback(
    (e: React.PointerEvent<HTMLDivElement>) => {
      const session = dragSessionRef.current;
      if (!session) return;
      const delta = e.clientX - session.startX;
      const next = clampPx(session.startPx + delta, session.startPx);
      setUserPxById((prev) => ({ ...prev, [session.colId]: next }));
    },
    [],
  );

  // pointerUp finalises the drag. The latest userPxById value is the
  // canonical size; we just clear the session so subsequent moves
  // don't carry stale state.
  const handlePointerUp = useCallback(
    (e: React.PointerEvent<HTMLDivElement>) => {
      const session = dragSessionRef.current;
      if (!session) return;
      try {
        e.currentTarget.releasePointerCapture(e.pointerId);
      } catch {
        /* synthetic event — ignore */
      }
      document.body.classList.remove("rg-resizing");
      dragSessionRef.current = null;
    },
    [],
  );

  // Keyboard accessibility: arrows nudge the column by 8px; Home
  // resets to the declared width; the handle is tabIndex=0 so screen
  // readers announce it as a slider/separator.
  const handleKeyDown = useCallback(
    (colId: string, e: ReactKeyboardEvent<HTMLDivElement>) => {
      const col = rendered.find((r) => r.col.id === colId);
      const declared: number =
        typeof col?.col.initialWidth === "number"
          ? col.col.initialWidth
          : parseWidth(col?.col.initialWidth) ?? 120;
      const userPx = userPxById[colId];
      const current = userPx ?? declared;
      const min = col?.col.minWidth ?? MIN_PX;
      const max = col?.col.maxWidth ?? MAX_PX;
      let changed: number | null = null;
      switch (e.key) {
        case "ArrowLeft":
        case "ArrowDown":
          changed = Math.max(min, current - 8);
          break;
        case "ArrowRight":
        case "ArrowUp":
          changed = Math.min(max, current + 8);
          break;
        case "Home":
        case "Escape":
          changed = declared;
          break;
        default:
          return;
      }
      e.preventDefault();
      updatePx(colId, clampPx(changed!, declared));
    },
    [rendered, userPxById, updatePx],
  );

  const renderHandle = (colId: string) => {
    const renderedForCol = rendered.find((r) => r.col.id === colId);
    if (!renderedForCol) return null;
    // The first column has no left neighbour (no handle), and any
    // column whose track is a flexible string (no numeric px) is
    // declared non-resizable; the last column is always flexible by
    // convention so we skip it for handle emission too.
    const colIndex = rendered.findIndex((r) => r.col.id === colId);
    if (colIndex <= 0) return null;
    if (colIndex === rendered.length - 1) return null;
    const label =
      typeof renderedForCol.col.header === "string"
        ? `Resize ${renderedForCol.col.header} column`
        : `Resize column ${renderedForCol.col.id}`;
    const valueNow = renderedForCol.px ?? 0;
    if (valueNow <= 0) return null;
    return (
      <div
        className="rg-col-handle"
        role="separator"
        aria-orientation="vertical"
        aria-label={label}
        aria-valuenow={valueNow}
        aria-valuemin={renderedForCol.col.minWidth ?? MIN_PX}
        aria-valuemax={renderedForCol.col.maxWidth ?? MAX_PX}
        tabIndex={0}
        data-testid={`rg-resize-${renderedForCol.col.id}`}
        onPointerDown={(e) => handlePointerDown(renderedForCol.col.id, e)}
        onPointerMove={handlePointerMove}
        onPointerUp={handlePointerUp}
        onKeyDown={(e) => handleKeyDown(renderedForCol.col.id, e)}
      />
    );
  };

  const renderHeaderRow = (group: GroupSpec, rowIndex: number) => {
    // Build the default header — one CellContent per column, holding
    // the column's label — and accept a custom override from the
    // caller when the header layout diverges from the row layout
    // (e.g. merged header cells). Both branches are wired through the
    // same CellContent type so the span math further down stays in
    // sync regardless of who supplied the headers.
    const headerCells: CellContent[] =
      group.customHeader ?? columns.map((c) => ({ node: c.header }));
    // Pre-compute which column.id the END of each cell sits on. We use
    // this anchor to attach the resize handle so handle #N is at the
    // right boundary of the column whose id matches rendered[N].col.id.
    let cursor = 0;
    const placed = headerCells.map((cell) => {
      const span = Math.max(1, cell.colSpan ?? 1);
      const startCol = cursor + 1;
      cursor += span;
      return { ...cell, startCol, span };
    });
    return (
      <div
        className="rg-row rg-row--head"
        role="row"
        key={`__head-${rowIndex}`}
        style={
          {
            gridTemplateColumns: rendered.map((r) => r.track).join(" "),
          } as CSSProperties
        }
      >
        {placed.map((cell, idx) => {
          const endColumnId =
            cell.span === 1
              ? columns[cell.startCol - 1]?.id
              : columns[cell.startCol + cell.span - 2]?.id;
          const handle = endColumnId ? renderHandle(endColumnId) : null;
          return (
            <div
              key={idx}
              role="columnheader"
              className="rg-cell rg-cell--head"
              style={{
                gridColumn:
                  cell.span > 1
                    ? `${cell.startCol} / span ${cell.span}`
                    : undefined,
              }}
            >
              {cell.node}
              {handle}
            </div>
          );
        })}
      </div>
    );
  };

  return (
    <div
      className={["rg", className].filter(Boolean).join(" ")}
      data-rg-storage-key={storageKey}
    >
      {groups.map((group, gi) => (
        <div key={`g-${gi}`} className="rg-group">
          {group.showHeader !== false && renderHeaderRow(group, gi)}
          {group.rows.map((row) => {
            const placedCells: ReactNode[] = [];
            let cursor = 0;
            const cells = row.cells;
            for (let i = 0; i < cells.length; i += 1) {
              const cell = cells[i];
              const span = Math.max(1, cell.colSpan ?? 1);
              const startCol = cursor + 1;
              cursor += span;
              // Attach the resize handle at the END column of each cell.
              // For single-span cells that handle belongs to the cell
              // itself; for colSpan'd cells we anchor the handle at the
              // rightmost column covered by the colspan so the operator
              // can still grab the edge of the merged region.
              const endColumnId =
                span === 1
                  ? columns[startCol - 1]?.id
                  : columns[startCol + span - 2]?.id;
              const handle = endColumnId ? renderHandle(endColumnId) : null;
              const spanStyle =
                span > 1
                  ? {
                      gridColumn: `${startCol} / span ${span}`,
                    }
                  : undefined;
              const columnAlign =
                span === 1
                  ? columns[startCol - 1]?.align
                  : undefined;
              const effectiveAlign = cell.align ?? columnAlign ?? "left";
              placedCells.push(
                <div
                  key={`c-${row.rowKey}-${i}`}
                  role={row.role === "button" ? undefined : "cell"}
                  className={[
                    "rg-cell",
                    `rg-cell--align-${effectiveAlign}`,
                    row.role === "button" ? "rg-cell--button" : "",
                  ]
                    .filter(Boolean)
                    .join(" ")}
                  style={spanStyle}
                >
                  {cell.node}
                  {handle}
                </div>,
              );
            }
            return (
              <div
                key={row.rowKey}
                className={["rg-row", row.className].filter(Boolean).join(" ")}
                role={row.role ?? "row"}
                tabIndex={row.tabIndex}
                aria-expanded={row.ariaExpanded}
                data-testid={row.rowTestId}
                onClick={row.onClick}
                style={
                  {
                    gridTemplateColumns: rendered.map((r) => r.track).join(" "),
                  } as CSSProperties
                }
              >
                {placedCells}
              </div>
            );
          })}
        </div>
      ))}
    </div>
  );
}
