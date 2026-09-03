import "@testing-library/jest-dom/vitest";

// Stub Web Storage. This environment defines the Storage constructor but
// leaves window.localStorage as an own property holding undefined.
//
// ThemeProvider.readMode() reads window.localStorage inside its useState
// initialiser, so without this every component rendered under ThemeProvider
// throws before reaching its assertions. One missing shim here fails most of
// the suite at once, so read this file before chasing an individual failure.
//
// The implementations go on Storage.prototype rather than on the instances.
// DataTable's resize test asserts persistence with
// vi.spyOn(Storage.prototype, "setItem"), and an own method on the instance
// would shadow that spy: the write would happen, the spy would record
// nothing, and the test would fail claiming the column width is not
// persisted. Backing each instance with its own Map keeps localStorage and
// sessionStorage from sharing keys, which Playground relies on.
//
// Guarded like matchMedia below, so an environment that supplies real
// storage gets a no-op instead of a competing stub.
if (typeof window !== "undefined" && !window.localStorage) {
  const stores = new WeakMap<object, Map<string, string>>();
  const proto = (globalThis as unknown as { Storage?: { prototype: object } })
    .Storage?.prototype;

  function mapFor(self: object): Map<string, string> {
    let m = stores.get(self);
    if (!m) {
      m = new Map();
      stores.set(self, m);
    }
    return m;
  }

  const impl = {
    getItem(this: object, k: string): string | null {
      const m = mapFor(this);
      return m.has(String(k)) ? m.get(String(k))! : null;
    },
    setItem(this: object, k: string, v: string): void {
      mapFor(this).set(String(k), String(v));
    },
    removeItem(this: object, k: string): void {
      mapFor(this).delete(String(k));
    },
    clear(this: object): void {
      mapFor(this).clear();
    },
    key(this: object, i: number): string | null {
      return Array.from(mapFor(this).keys())[i] ?? null;
    },
  };

  const target = proto ?? {};
  Object.assign(target, impl);
  Object.defineProperty(target, "length", {
    configurable: true,
    get(this: object) {
      return mapFor(this).size;
    },
  });

  for (const name of ["localStorage", "sessionStorage"] as const) {
    Object.defineProperty(window, name, {
      configurable: true,
      writable: true,
      value: proto ? Object.create(proto) : Object.create(target),
    });
  }
}

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
