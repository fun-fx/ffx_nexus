import { describe, expect, it } from "vitest";
import { formatExact, formatTokens } from "./format";

describe("formatTokens", () => {
  it("leaves counts under a thousand alone", () => {
    expect(formatTokens(0)).toBe("0");
    expect(formatTokens(1)).toBe("1");
    expect(formatTokens(842)).toBe("842");
    expect(formatTokens(999)).toBe("999");
  });

  it("switches to K at a thousand", () => {
    expect(formatTokens(1_000)).toBe("1.0K");
    expect(formatTokens(49_123)).toBe("49.1K");
    expect(formatTokens(156_147)).toBe("156.1K");
    expect(formatTokens(999_999)).toBe("1000.0K");
  });

  it("switches to M at a million", () => {
    expect(formatTokens(1_000_000)).toBe("1.00M");
    expect(formatTokens(1_234_567)).toBe("1.23M");
    expect(formatTokens(23_400_000)).toBe("23.40M");
  });

  // Token counts arrive from JSON and a missing field reads as undefined.
  // Rendering "NaN" in the Tokens column would look like a gateway bug.
  it("renders non-finite and negative input as zero", () => {
    expect(formatTokens(NaN)).toBe("0");
    expect(formatTokens(Infinity)).toBe("0");
    expect(formatTokens(-5)).toBe("0");
    expect(formatTokens(undefined as unknown as number)).toBe("0");
  });
});

describe("formatExact", () => {
  it("groups thousands for the tooltip", () => {
    expect(formatExact(0)).toBe("0");
    expect(formatExact(842)).toBe("842");
    expect(formatExact(49_123)).toBe("49,123");
    expect(formatExact(1_234_567)).toBe("1,234,567");
  });

  it("renders non-finite input as zero", () => {
    expect(formatExact(NaN)).toBe("0");
    expect(formatExact(-5)).toBe("0");
  });
});
