import { useMemo, useState } from "react";

export interface Column<T> {
  id: string;
  header: React.ReactNode;
  cell: (row: T) => React.ReactNode;
  sortValue?: (row: T) => string | number;
  width?: string;
  align?: "left" | "right" | "center";
}

interface Props<T> {
  rows: T[];
  columns: Column<T>[];
  rowKey?: (row: T) => string;
  onRowClick?: (row: T) => void;
  emptyMessage?: React.ReactNode;
  initialSort?: { id: string; dir: "asc" | "desc" };
  pageSize?: number;
}

export function DataTable<T>({
  rows,
  columns,
  rowKey,
  onRowClick,
  emptyMessage = "No rows.",
  initialSort,
  pageSize = 25,
}: Props<T>) {
  const [sort, setSort] = useState<{ id: string; dir: "asc" | "desc" } | null>(
    initialSort ?? null,
  );
  const [page, setPage] = useState(0);

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

  return (
    <div className="dt">
      <div className="dt-scroller">
        <div className="dt-grid" role="grid" style={{ gridTemplateColumns: columns.map((c) => c.width ?? "minmax(0, 1fr)").join(" ") }}>
          <div className="dt-row is-head" role="row">
            {columns.map((c) => (
              <div
                key={c.id}
                role="columnheader"
                aria-sort={
                  sort?.id === c.id
                    ? sort.dir === "asc"
                      ? "ascending"
                      : "descending"
                    : "none"
                }
                className={
                  "dt-th" +
                  (c.align ? ` align-${c.align}` : "") +
                  (sort?.id === c.id ? " is-sorted" : "")
                }
              >
                <button
                  type="button"
                  className="dt-th-btn"
                  onClick={() => {
                    if (!c.sortValue) return;
                    setSort((prev) => {
                      if (prev?.id === c.id) {
                        return { id: c.id, dir: prev.dir === "asc" ? "desc" : "asc" };
                      }
                      return { id: c.id, dir: "asc" };
                    });
                    setPage(0);
                  }}
                  tabIndex={c.sortValue ? 0 : -1}
                  aria-label={typeof c.header === "string" ? `Sort by ${c.header}` : undefined}
                >
                  {c.header}
                  {c.sortValue && (
                    <span className="dt-sort-ind" aria-hidden="true">
                      {sort?.id === c.id
                        ? sort.dir === "asc"
                          ? "▲"
                          : "▼"
                        : "↕"}
                    </span>
                  )}
                </button>
              </div>
            ))}
          </div>
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
                {columns.map((c) => (
                  <div
                    key={c.id}
                    className={"dt-cell" + (c.align ? ` align-${c.align}` : "")}
                    role="gridcell"
                  >
                    {c.cell(row)}
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
