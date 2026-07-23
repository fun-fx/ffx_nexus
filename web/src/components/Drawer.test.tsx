import { describe, expect, it } from "vitest";
import { render, fireEvent, waitFor, screen } from "@testing-library/react";
import { useState } from "react";
import { Drawer } from "./Drawer";

function Demo({ initialOpen }: { initialOpen: boolean }) {
  const [open, setOpen] = useState(initialOpen);
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>Open drawer</button>
      <Drawer open={open} onClose={() => setOpen(false)} title="Demo">
        <form id="demo-form">
          <label>
            <span>Name</span>
            <input type="text" placeholder="cb" autoFocus />
          </label>
        </form>
      </Drawer>
    </>
  );
}

describe("<Drawer /> focus management", () => {
  it("focuses the first text input in the body, not the header close button", async () => {
    render(<Demo initialOpen={true} />);

    // Header button is rendered, but its focus halo must NOT be active.
    const closeBtn = screen.getByRole("button", { name: "Close" });
    const input = screen.getByPlaceholderText("cb") as HTMLInputElement;

    await waitFor(() => {
      expect(document.activeElement).toBe(input);
    });
    expect(document.activeElement).not.toBe(closeBtn);
  });

  it("keeps focus on the input while typing — does not steal to the close button", async () => {
    render(<Demo initialOpen={true} />);

    const input = screen.getByPlaceholderText("cb") as HTMLInputElement;
    await waitFor(() => expect(document.activeElement).toBe(input));

    // Simulate the user typing two characters. Without our fix, the auto-focus
    // on the header close button would steal focus away from the input after
    // the first keystroke.
    fireEvent.change(input, { target: { value: "cb" } });

    expect(document.activeElement).toBe(input);
    expect(input.value).toBe("cb");
  });

  it("falls back to the first focusable child when no text field exists", async () => {
    function ConfirmOnly() {
      const [open, setOpen] = useState(true);
      return (
        <Drawer open={open} onClose={() => setOpen(false)} title="Confirm">
          <p>No inputs here.</p>
          <button type="button">OK</button>
        </Drawer>
      );
    }
    render(<ConfirmOnly />);
    const ok = screen.getByRole("button", { name: "OK" });
    await waitFor(() => expect(document.activeElement).toBe(ok));
  });
});
