import "@testing-library/jest-dom/vitest";

// Stub matchMedia for ThemeProvider's prefers-dark detection.
if (typeof window !== "undefined" && typeof window.matchMedia !== "function") {
  // @ts-expect-error - test stub
  window.matchMedia = (query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  });
}
