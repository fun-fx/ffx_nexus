import { describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import axe from "axe-core";
import { useState } from "react";
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

describe("<Drawer /> focus management", () => {
  function Demo() {
    const [open, setOpen] = useState(true);
    return (
      <Drawer open={open} onClose={() => setOpen(false)} title="Demo">
        <form id="demo-form">
          <label>
            <span>Name</span>
            <input type="text" placeholder="cb" autoFocus />
          </label>
        </form>
      </Drawer>
    );
  }

  function ConfirmOnly() {
    const [open, setOpen] = useState(true);
    return (
      <Drawer open={open} onClose={() => setOpen(false)} title="Confirm">
        <p>No inputs here.</p>
        <button type="button">OK</button>
      </Drawer>
    );
  }

  it("focuses the first text input in the body, not the header close button", async () => {
    render(<Demo />);

    const closeBtn = screen.getByRole("button", { name: "Close" });
    const input = screen.getByPlaceholderText("cb") as HTMLInputElement;

    await waitFor(() => {
      expect(document.activeElement).toBe(input);
    });
    expect(document.activeElement).not.toBe(closeBtn);
  });

  it("keeps focus on the input while typing — does not steal to the close button", async () => {
    render(<Demo />);

    const input = screen.getByPlaceholderText("cb") as HTMLInputElement;
    await waitFor(() => expect(document.activeElement).toBe(input));

    // Simulate the user typing two characters. Without our fix, the auto-focus
    // on the header close button would steal focus away from the input after
    // the first keystroke.
    fireEvent.change(input, { target: { value: "cb" } });

    expect(document.activeElement).toBe(input);
    expect(input.value).toBe("cb");
  });

  it("falls back to a body button when the drawer has no text field", async () => {
    render(<ConfirmOnly />);
    const ok = screen.getByRole("button", { name: "OK" });
    await waitFor(() => expect(document.activeElement).toBe(ok));
    // The header close button must not steal focus when the body has its own
    // interactive control to land on first.
    expect(document.activeElement).not.toBe(
      screen.getByRole("button", { name: "Close" }),
    );
  });
});
