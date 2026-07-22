import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import axe from "axe-core";
import { Drawer } from "./Drawer";

describe("Drawer", () => {
  it("does not render when closed", () => {
    render(
      <Drawer open={false} onClose={() => {}} title="Hidden">
        <p>should not render</p>
      </Drawer>,
    );
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("renders the title and children when open", () => {
    render(
      <Drawer open onClose={() => {}} title="Inspect">
        <p>hello drawer</p>
      </Drawer>,
    );
    const dialog = screen.getByRole("dialog");
    expect(dialog).toHaveAttribute("aria-modal", "true");
    expect(screen.getByText("hello drawer")).toBeInTheDocument();
  });

  it("closes on Esc", async () => {
    const onClose = vi.fn();
    render(
      <Drawer open onClose={onClose} title="Inspect">
        <button>x</button>
      </Drawer>,
    );
    await userEvent.keyboard("{Escape}");
    expect(onClose).toHaveBeenCalled();
  });

  it("no axe violations when open", async () => {
    const { container } = render(
      <Drawer open onClose={() => {}} title="Inspect">
        <p>content</p>
      </Drawer>,
    );
    const violations = await axe.run(container, {
      rules: { "color-contrast": { enabled: false } },
    });
    expect(violations.violations).toEqual([]);
  });
});
