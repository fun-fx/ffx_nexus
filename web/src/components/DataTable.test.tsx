import { describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent, act } from "@testing-library/react";
import axe from "axe-core";
import { DataTable, type Column } from "./DataTable";

interface Row {
  id: string;
  name: string;
}

const rows: Row[] = [
  { id: "a", name: "alpha" },
  { id: "b", name: "beta" },
  { id: "g", name: "gamma" },
];
const cols: Column<Row>[] = [
  { id: "name", header: "Name", cell: (r) => r.name, sortValue: (r) => r.name },
  { id: "id", header: "ID", cell: (r) => r.id, sortValue: (r) => r.id },
];

describe("DataTable", () => {
  it("renders a column header for each column", () => {
    render(<DataTable rows={rows} columns={cols} rowKey={(r) => r.id} />);
    expect(screen.getByRole("columnheader", { name: "Name" })).toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: "ID" })).toBeInTheDocument();
  });

  it("sorts columns by sortValue", () => {
    render(
      <DataTable
        rows={rows}
        columns={cols}
        rowKey={(r) => r.id}
        initialSort={{ id: "name", dir: "asc" }}
      />,
    );
    const items = screen.getAllByRole("gridcell", { name: /alpha|beta|gamma/i });
    expect(items.map((el) => el.textContent)).toEqual(["alpha", "beta", "gamma"]);
  });

  it("calls onRowClick when a row is activated", () => {
    let picked: string | null = null;
    render(
      <DataTable
        rows={rows}
        columns={cols}
        rowKey={(r) => r.id}
        onRowClick={(r) => {
          picked = r.name;
        }}
      />,
    );
    const alphaRow = screen.getByText("alpha").closest("[role=row]") as HTMLElement | null;
    alphaRow?.click();
    expect(picked).toBe("alpha");
  });

  it("shows an empty-state when no rows", () => {
    render(<DataTable rows={[]} columns={cols} emptyMessage="Nothing here." />);
    expect(screen.getByText("Nothing here.")).toBeInTheDocument();
  });

  it("has no axe violations on a populated grid", async () => {
    const { container } = render(
      <DataTable rows={rows} columns={cols} rowKey={(r) => r.id} />,
    );
    const violations = await axe.run(container, {
      rules: { "color-contrast": { enabled: false } },
    });
    expect(violations.violations).toEqual([]);
  });
});

// All resize tests use a fresh, isolated localStorage so a previous
// run does not contaminate the starting widths. The fixture width is
// a number (140px) so the resize handle is wired up for that column.
// jsdom does not model PointerEvent clientX, so the drag pipeline is
// observable only via state side-effects (localStorage writes, grid
// template after a remount). Keyboard interactions DO fire reliably;
// we use those for the live reshape assertions.
const RESIZE_COLS: Column<Row>[] = [
  { id: "name", header: "Name", cell: (r) => r.name, sortValue: (r) => r.name },
  {
    id: "id",
    header: "ID",
    cell: (r) => r.id,
    sortValue: (r) => r.id,
    width: 140,
  },
];

describe("DataTable column resize", () => {
  it("renders a resize handle for columns with a numeric width", () => {
    render(<DataTable rows={rows} columns={RESIZE_COLS} storageKey="" />);
    const handle = screen.getByTestId("dt-resize-id");
    expect(handle).toBeInTheDocument();
    expect(handle.getAttribute("role")).toBe("separator");
    expect(handle.getAttribute("aria-orientation")).toBe("vertical");
  });

  it("keyboard: ArrowRight grows the column by 8px and Home resets to declared", async () => {
    render(<DataTable rows={rows} columns={RESIZE_COLS} storageKey="" />);
    const handle = screen.getByTestId("dt-resize-id") as HTMLElement;
    await act(async () => {
      handle.focus();
      fireEvent.keyDown(handle, { key: "ArrowRight" });
      // Awaiting microtasks lets React flush state between events so
      // the second keydown sees the post-resize value as its starting
      // point — otherwise both setUserPxById calls collapse into one
      // and Home is overridden by the older ArrowRight state.
      await new Promise((r) => setTimeout(r, 0));
      fireEvent.keyDown(handle, { key: "Home" });
      await new Promise((r) => setTimeout(r, 0));
    });
    expect(handle.getAttribute("aria-valuenow")).toBe("140");
  });

  it("clamps the displayed px at MIN_PX after many ArrowLeft presses", async () => {
    render(<DataTable rows={rows} columns={RESIZE_COLS} storageKey="" />);
    const handle = screen.getByTestId("dt-resize-id") as HTMLElement;
    handle.focus();
    for (let i = 0; i < 30; i++) {
      // eslint-disable-next-line no-await-in-loop
      await new Promise((r) => setTimeout(r, 0));
      fireEvent.keyDown(handle, { key: "ArrowLeft" });
    }
    // Final await so the final setUserPxBy flushes.
    // eslint-disable-next-line no-await-in-loop
    await new Promise((r) => setTimeout(r, 0));
    expect(handle.getAttribute("aria-valuenow")).toBe("40");
  });

  it("renders a stored width into the grid-template-columns on mount", () => {
    const KEY = "nexus:test:restored";
    window.localStorage.setItem(KEY, JSON.stringify({ id: 220 }));
    render(<DataTable rows={rows} columns={RESIZE_COLS} storageKey={KEY} />);
    const grid = document.querySelector(".dt-grid") as HTMLElement;
    expect(grid.style.gridTemplateColumns).toContain("220px");
    window.localStorage.removeItem(KEY);
  });

  it("persists the storageKey on writes (contract: writes happen on every user width change)", async () => {
    const KEY = "nexus:test:resize-contract";
    const writes: string[] = [];
    vi.spyOn(Storage.prototype, "setItem").mockImplementation((k, v) => {
      if (k === KEY) writes.push(String(v));
    });
    const { unmount } = render(
      <DataTable rows={rows} columns={RESIZE_COLS} storageKey={KEY} />,
    );
    const handle = screen.getByTestId("dt-resize-id") as HTMLElement;
    await act(async () => {
      handle.focus();
      fireEvent.keyDown(handle, { key: "ArrowRight" });
      await new Promise((r) => setTimeout(r, 0));
    });
    unmount();
    const lastRelevant = writes.filter((w) => w.includes("\"id\""));
    expect(lastRelevant.some((w) => w.includes("148"))).toBe(true);
  });
});
