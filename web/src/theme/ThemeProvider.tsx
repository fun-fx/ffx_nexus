/**
 * ThemeProvider — applies `data-theme="dark|light"` on <html>.
 * Order of preference: explicit user choice (localStorage) → system preference.
 */

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from "react";

type Theme = "dark" | "light";
type Mode = Theme | "system";

interface ThemeCtx {
  theme: Theme;
  mode: Mode;
  setMode: (m: Mode) => void;
  toggle: () => void;
}

const Ctx = createContext<ThemeCtx | null>(null);
const STORAGE_KEY = "nexus.theme";

function readMode(): Mode {
  if (typeof window === "undefined") return "dark";
  const raw = window.localStorage.getItem(STORAGE_KEY);
  if (raw === "dark" || raw === "light") return raw;
  return "system";
}

function resolveTheme(mode: Mode): Theme {
  if (mode === "dark" || mode === "light") return mode;
  if (typeof window === "undefined") return "dark";
  return window.matchMedia?.("(prefers-color-scheme: light)").matches
    ? "light"
    : "dark";
}

export function ThemeProvider({ children }: { children: React.ReactNode }) {
  const [mode, setModeState] = useState<Mode>(() => readMode());
  const [theme, setTheme] = useState<Theme>(() => resolveTheme(readMode()));

  // Apply to <html> for CSS variable cascade.
  useEffect(() => {
    const next = resolveTheme(mode);
    setTheme(next);
    document.documentElement.dataset.theme = next;
  }, [mode]);

  // React to system changes if mode === "system".
  useEffect(() => {
    if (mode !== "system" || typeof window === "undefined") return;
    const mq = window.matchMedia("(prefers-color-scheme: light)");
    const cb = () => setTheme(mq.matches ? "light" : "dark");
    mq.addEventListener?.("change", cb);
    return () => mq.removeEventListener?.("change", cb);
  }, [mode]);

  const setMode = useCallback((m: Mode) => {
    setModeState(m);
    if (typeof window !== "undefined") {
      if (m === "system") window.localStorage.removeItem(STORAGE_KEY);
      else window.localStorage.setItem(STORAGE_KEY, m);
    }
  }, []);

  const toggle = useCallback(() => {
    setModeState((prev) => {
      const current = resolveTheme(prev);
      const next: Theme = current === "dark" ? "light" : "dark";
      window.localStorage.setItem(STORAGE_KEY, next);
      return next;
    });
  }, []);

  const value = useMemo<ThemeCtx>(
    () => ({ theme, mode, setMode, toggle }),
    [theme, mode, setMode, toggle],
  );

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useTheme(): ThemeCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error("useTheme outside ThemeProvider");
  return v;
}
