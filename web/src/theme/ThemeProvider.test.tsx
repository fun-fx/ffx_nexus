import { describe, expect, it } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import axe from "axe-core";
import { ThemeProvider } from "./ThemeProvider";
import { ThemeToggle } from "./ThemeToggle";

function Probe() {
  return (
    <ThemeProvider>
      <ThemeToggle />
    </ThemeProvider>
  );
}

describe("ThemeProvider + ThemeToggle", () => {
  it("sets data-theme on the html element on mount", async () => {
    render(<Probe />);
    await waitFor(() => {
      expect(["dark", "light"]).toContain(
        document.documentElement.dataset.theme ?? "",
      );
    });
  });

  it("toggle flips the data-theme attribute", async () => {
    render(<Probe />);
    await waitFor(() => {
      expect(["dark", "light"]).toContain(
        document.documentElement.dataset.theme ?? "",
      );
    });
    const before = document.documentElement.dataset.theme ?? "";
    const user = userEvent.setup();
    await user.click(screen.getByRole("button"));
    const after = document.documentElement.dataset.theme ?? "";
    expect(after).not.toBe(before);
  });

  it("toggle button has a descriptive aria-label", async () => {
    render(<Probe />);
    await waitFor(() =>
      expect(document.documentElement.dataset.theme ?? "").not.toBe(""),
    );
    const btn = screen.getByRole("button");
    expect(btn.getAttribute("aria-label")).toMatch(/Switch to (light|dark) theme/);
  });

  it("no axe violations", async () => {
    const { container } = render(<Probe />);
    const violations = await axe.run(container, {
      rules: { "color-contrast": { enabled: false } },
    });
    expect(violations.violations).toEqual([]);
  });
});
