import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import axe from "axe-core";
import { Chip } from "./Chip";

async function runAxe(container: HTMLElement) {
  const results = await axe.run(container, {
    rules: {
      // We don't require a color contrast guarantee for plain chip tests in
      // jsdom (default white-on-white tokens are not realistic anyway).
      "color-contrast": { enabled: false },
    },
  });
  return results.violations;
}

describe("Chip", () => {
  it("renders the label", () => {
    render(<Chip tone="neutral">All</Chip>);
    expect(screen.getByText("All")).toBeInTheDocument();
  });

  it("becomes a button when onClick is provided", () => {
    let clicked = 0;
    render(
      <Chip tone="accent" active onClick={() => clicked++}>
        hover me
      </Chip>,
    );
    const btn = screen.getByRole("button", { name: /hover me/i });
    btn.click();
    expect(clicked).toBe(1);
    expect(btn).toHaveAttribute("aria-pressed", "true");
  });

  it("has no axe violations as a static label", async () => {
    const { container } = render(<Chip tone="neutral">tag</Chip>);
    const violations = await runAxe(container);
    expect(violations).toEqual([]);
  });
});
