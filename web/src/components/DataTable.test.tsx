import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
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
