import { describe, it, expect } from "vitest";
import { safeNext } from "./safeNext";

describe("safeNext", () => {
  it("keeps ordinary console routes", () => {
    for (const p of [
      "/",
      "/traces",
      "/routing/gpt-4o-mini",
      "/eval?tab=profiles",
      "/docs/quickstart",
      "/playground?model=claude-3-5-sonnet",
    ]) {
      expect(safeNext(p)).toBe(p);
    }
  });

  it("rejects targets that leave the origin", () => {
    // Each of these would land the operator on someone else's page after a
    // password entry, which is the phishing shape the advisories describe.
    for (const p of [
      "//evil.example",
      "https://evil.example",
      "http://evil.example",
      "javascript:alert(1)",
      "data:text/html,<script>alert(1)</script>",
      "evil.example",
      "../../etc/passwd",
    ]) {
      expect(safeNext(p)).toBeNull();
    }
  });

  // The regression this function exists for. The previous guards checked
  // startsWith("//") only, so every case here passed them: it starts with a
  // single slash and the second character is not a slash. Browsers normalise
  // the backslash to a slash, making the result an external origin.
  it("rejects the backslash bypass (GHSA-wrjc-x8rr-h8h6, GHSA-jjmj-jmhj-qwj2)", () => {
    for (const p of [
      "/\\evil.example",
      "/\\\\evil.example",
      "/\\/evil.example",
      "/traces\\@evil.example",
      "\\/evil.example",
    ]) {
      expect(safeNext(p)).toBeNull();
    }
  });

  it("rejects control characters that parsers strip after validation", () => {
    expect(safeNext("/\u0000/evil.example")).toBeNull();
    expect(safeNext("/\n//evil.example")).toBeNull();
    expect(safeNext("/\t\\evil.example")).toBeNull();
  });

  it("treats absent and empty input as no redirect", () => {
    expect(safeNext(null)).toBeNull();
    expect(safeNext(undefined)).toBeNull();
    expect(safeNext("")).toBeNull();
  });
});
