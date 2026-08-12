import { describe, expect, it, vi, afterEach, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "../theme/ThemeProvider";
import { Spend } from "./Spend";

// Tests focus on the URL the page assembles and the drill-panel toggle
// rather than the chart geometry. fetch is stubbed; responses are
// canned. We are not re-implementing the ReactQuery cache flush logic
// just to assert that `setPickedDay` triggered the breakdown fetch.

const adminMe = {
  id: "u-admin",
  email: "admin@nexus.local",
  role: "admin" as const,
  org_id: "o1",
  enforce_limits: true,
  created_at: "2026-01-01T00:00:00Z",
};

const memberMe = {
  ...adminMe,
  id: "u-member",
  email: "member@nexus.local",
  role: "member" as const,
};

const otherUser = {
  id: "u-other",
  email: "other@nexus.local",
  role: "member" as const,
  org_id: "o1",
};

let callLog: Array<{ url: string; method: string; body: unknown }> = [];

function stubFetch(
  handler: (url: string, method: string, body: unknown) => Response,
) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = init?.method?.toUpperCase() ?? "GET";
      const body = init?.body ? JSON.parse(String(init.body)) : null;
      callLog.push({ url, method, body });
      return handler(url, method, body);
    }),
  );
}

function renderSpend(me: typeof adminMe | typeof memberMe) {
  callLog = [];
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <ThemeProvider>
      <MemoryRouter initialEntries={["/spend"]}>
        <QueryClientProvider client={qc}>
          <Spend />
        </QueryClientProvider>
      </MemoryRouter>
    </ThemeProvider>,
  );
  // Apply the me payload only after render so the page's me-query
  // resolves; otherwise the Spend page reads admin=false and skips
  // fetching /api/users.
  return { ...utils, qc, me };
}

// withSummaryFetch wraps a test-specific stub handler so the
// self-spend / admin-spend summary endpoints return a deterministic
// shape even in tests that don't assert on hero data. The summary
// query runs as soon as the page mounts, so naively falling through
// to the default `jsonResponse([])` would crash the SpendHero render
// with a TypeError on its sanitize step.
//
// URL detection is anchored: `summary?...` or `summary?days=N` — the
// query string with `days=` distinguishes it from any future
// `/summary` prefix that might be added to other endpoints.
function withSummaryFetch(
  handler: (url: string, method: string, body: unknown) => Response,
): (url: string, method: string, body: unknown) => Response {
  return (url, method, body) => {
    if (
      url === "/api/me/spend/summary" ||
      url.startsWith("/api/me/spend/summary?") ||
      /^\/api\/users\/[^/]+\/spend\/summary(\?|$)/.test(url)
    ) {
      const m = url.match(/days=(\d+)/);
      const days = m ? Number(m[1]) : 30;
      return summaryResponse(days, 0, 0, false);
    }
    return handler(url, method, body);
  };
}

// summaryResponse shapes a /api/{me|users}/spend/summary response.
// The cast through `unknown` keeps the test stub from leaking the
// real DailySpendSummary type into unrelated test bodies.
function summaryResponse(days: number, current = 0, previous = 0, hasPrevious = false): Response {
  return jsonResponse({
    days,
    current_cost_usd: current,
    previous_cost_usd: previous,
    delta_cost_usd: current - previous,
    savings_pct: previous > 0 ? ((previous - current) / previous) * 100 : 0,
    has_previous: hasPrevious,
    current_tokens: 0,
    previous_tokens: 0,
    current_requests: 0,
    previous_requests: 0,
    current_cache_hits: 0,
    previous_cache_hits: 0,
  });
}

beforeEach(() => {
  callLog = [];
});

afterEach(() => {
  vi.unstubAllGlobals();
});

function jsonResponse(data: unknown): Response {
  return new Response(JSON.stringify(data), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

// ---- 1. Self view: scoped to /api/me/spend/daily?days=30 ---------------

describe("Spend — self view", () => {
  it("hits /api/me/spend/daily with the URL the page assembles", async () => {
    stubFetch(withSummaryFetch((url) => {
      if (url === "/api/me") return jsonResponse(adminMe);
      if (url.startsWith("/api/me/spend/daily?days=")) {
        return jsonResponse([
          { day: "2026-08-11", cost_usd: 1.23, tokens: 1000, requests: 4, cache_hits: 1 },
          { day: "2026-08-10", cost_usd: 0.5, tokens: 500, requests: 2, cache_hits: 0 },
        ]);
      }
      return jsonResponse([]);
    }));

    renderSpend(adminMe);

    await waitFor(() => {
      expect(callLog.some((c) => c.url.startsWith("/api/me/spend/daily?days="))).toBe(true);
    });

    // Default 30d range. Confirm the URL was hit with days=30 without
    // other params.
    const dailyCall = callLog.find(
      (c) => c.url.startsWith("/api/me/spend/daily") && !c.url.includes("/breakdown"),
    );
    expect(dailyCall).toBeDefined();
    expect(dailyCall!.url).toContain("days=30");
  });

  it("switching the range chip re-fetches with the new value", async () => {
    stubFetch(withSummaryFetch((url) => {
      if (url === "/api/me") return jsonResponse(adminMe);
      if (url.startsWith("/api/me/spend/daily?days=")) {
        return jsonResponse([]);
      }
      return jsonResponse([]);
    }));

    renderSpend(adminMe);
    await waitFor(() => {
      expect(callLog.some((c) => c.url.includes("days=30"))).toBe(true);
    });

    fireEvent.click(screen.getByTestId("spend-range-7"));

    await waitFor(() => {
      const calls = callLog.filter((c) => c.url.startsWith("/api/me/spend/daily?days="));
      expect(calls.some((c) => c.url.includes("days=7"))).toBe(true);
    });
  });

  it("clicking a day opens the breakdown panel and fires the breakdown fetch", async () => {
    stubFetch(withSummaryFetch((url) => {
      if (url === "/api/me") return jsonResponse(adminMe);
      if (url === "/api/me/spend/daily?days=30") {
        return jsonResponse([
          { day: "2026-08-11", cost_usd: 1.23, tokens: 1000, requests: 4, cache_hits: 1 },
        ]);
      }
      if (url === "/api/me/spend/daily/2026-08-11/breakdown") {
        return jsonResponse([
          {
            model: "openai/gpt-4o-mini",
            provider: "openai",
            response_model: "gpt-4o-mini-2024-07-18",
            cost_usd: 1.0,
            tokens: 800,
            requests: 3,
            cache_hits: 0,
          },
          {
            model: "openai/gpt-4o-mini",
            provider: "openai",
            response_model: "",
            cost_usd: 0,
            tokens: 200,
            requests: 1,
            cache_hits: 1,
          },
        ]);
      }
      return jsonResponse([]);
    }));

    renderSpend(adminMe);
    // Wait for the daily list to render so the action button is mounted.
    await waitFor(() => {
      expect(screen.getByTestId("spend-pick-2026-08-11")).toBeInTheDocument();
    });

    fireEvent.click(screen.getByTestId("spend-pick-2026-08-11"));

    await waitFor(() => {
      const breakdownCall = callLog.find((c) =>
        c.url.endsWith("/api/me/spend/daily/2026-08-11/breakdown"),
      );
      expect(breakdownCall).toBeDefined();
    });
    expect(screen.getByText(/2026-08-11 breakdown/)).toBeInTheDocument();
  });

  it("keeps the picked-day chip alive after closing the breakdown, and reopens the panel on chip click", async () => {
    // The user reported the previous flow dropped the picked day as
    // soon as they hit "Close drill", so the breakdown could only be
    // reached again by re-clicking the bar in the chart. The new flow
    // keeps a `drilled:` chip rail under the chart so a reader can
    // click back into the same day without scrolling.
    stubFetch(withSummaryFetch((url) => {
      if (url === "/api/me") return jsonResponse(adminMe);
      if (url === "/api/me/spend/daily?days=30") {
        return jsonResponse([
          { day: "2026-08-11", cost_usd: 1.23, tokens: 1000, requests: 4, cache_hits: 1 },
        ]);
      }
      if (url === "/api/me/spend/daily/2026-08-11/breakdown") {
        return jsonResponse([
          {
            model: "openai/gpt-4o-mini",
            provider: "openai",
            response_model: "gpt-4o-mini-2024-07-18",
            cost_usd: 1.0,
            tokens: 800,
            requests: 3,
            cache_hits: 0,
          },
        ]);
      }
      return jsonResponse([]);
    }));

    renderSpend(adminMe);
    await waitFor(() => {
      expect(screen.getByTestId("spend-pick-2026-08-11")).toBeInTheDocument();
    });

    // 1. Pick a day → breakdown panel opens, drill chip rail appears.
    fireEvent.click(screen.getByTestId("spend-pick-2026-08-11"));
    await waitFor(() => {
      expect(screen.getByText(/2026-08-11 breakdown/)).toBeInTheDocument();
      expect(screen.getByTestId("spend-drilled-rail")).toBeInTheDocument();
    });

    const breakdownCallsBeforeClose = callLog.filter((c) =>
      c.url.endsWith("/api/me/spend/daily/2026-08-11/breakdown"),
    ).length;

    // 2. Close drill → panel hides AND the rail stays visible with the
    //    same day label printed on the chip.
    fireEvent.click(screen.getByTestId("spend-close-breakdown"));
    await waitFor(() => {
      expect(screen.queryByText(/2026-08-11 breakdown/)).not.toBeInTheDocument();
      expect(screen.getByTestId("spend-drilled-rail")).toBeInTheDocument();
      expect(screen.getByTestId("spend-drilled-chip")).toHaveTextContent("2026-08-11");
    });

    // 3. Click the drill chip → panel re-opens for the same day WITHOUT
    //    triggering a second breakdown fetch (the cache is already
    //    warm — the original `breakdownCallsBeforeClose` count should
    //    hold).
    fireEvent.click(screen.getByTestId("spend-drilled-chip"));
    await waitFor(() => {
      expect(screen.getByText(/2026-08-11 breakdown/)).toBeInTheDocument();
    });
    const breakdownCallsAfterReopen = callLog.filter((c) =>
      c.url.endsWith("/api/me/spend/daily/2026-08-11/breakdown"),
    ).length;
    expect(breakdownCallsAfterReopen).toBe(breakdownCallsBeforeClose);

    // 4. Chip × button fully clears the picked day and the rail.
    fireEvent.click(screen.getByTestId("spend-drilled-chip-remove"));
    await waitFor(() => {
      expect(screen.queryByTestId("spend-drilled-rail")).not.toBeInTheDocument();
    });
    expect(screen.queryByText(/2026-08-11 breakdown/)).not.toBeInTheDocument();
  });

  it("range chip reset clears the picked day entirely (no orphan rail)", async () => {
    stubFetch(withSummaryFetch((url) => {
      if (url === "/api/me") return jsonResponse(adminMe);
      if (url.startsWith("/api/me/spend/daily?days=")) {
        return jsonResponse([
          { day: "2026-08-11", cost_usd: 1.23, tokens: 1000, requests: 4, cache_hits: 1 },
        ]);
      }
      if (url.endsWith("/breakdown")) {
        return jsonResponse([
          {
            model: "openai/gpt-4o-mini",
            provider: "openai",
            response_model: "gpt-4o-mini-2024-07-18",
            cost_usd: 1.0,
            tokens: 800,
            requests: 3,
            cache_hits: 0,
          },
        ]);
      }
      return jsonResponse([]);
    }));

    renderSpend(adminMe);
    await waitFor(() => {
      expect(screen.getByTestId("spend-pick-2026-08-11")).toBeInTheDocument();
    });
    fireEvent.click(screen.getByTestId("spend-pick-2026-08-11"));
    await waitFor(() => {
      expect(screen.getByTestId("spend-drilled-rail")).toBeInTheDocument();
    });

    // Switching to a different range resets the chart; the picked day
    // is now meaningless in the new window so the rail clears too.
    fireEvent.click(screen.getByTestId("spend-range-7"));
    await waitFor(() => {
      expect(screen.queryByTestId("spend-drilled-rail")).not.toBeInTheDocument();
    });
  });
});

// ---- 2. Admin scope switcher: /api/users/{id}/spend/daily -------------

describe("Spend — admin scope", () => {
  it("lists members and switches fetches to /api/users/{id}/spend/daily", async () => {
    stubFetch(withSummaryFetch((url) => {
      if (url === "/api/me") return jsonResponse(adminMe);
      if (url === "/api/users") return jsonResponse([otherUser]);
      if (url.startsWith("/api/me/spend/daily?days=")) return jsonResponse([]);
      if (url === "/api/users/u-other/spend/daily?days=30") {
        return jsonResponse([
          { day: "2026-08-11", cost_usd: 9.99, tokens: 8000, requests: 20, cache_hits: 0 },
        ]);
      }
      return jsonResponse([]);
    }));

    renderSpend(adminMe);
    await waitFor(() => {
      expect(screen.getByTestId("spend-scope-u-other")).toBeInTheDocument();
    });

    fireEvent.click(screen.getByTestId("spend-scope-u-other"));

    await waitFor(() => {
      const call = callLog.find((c) =>
        c.url.startsWith("/api/users/u-other/spend/daily"),
      );
      expect(call).toBeDefined();
    });
  });

  it("does NOT render the user switcher for non-admin", async () => {
    stubFetch(withSummaryFetch((url) => {
      if (url === "/api/me") return jsonResponse(memberMe);
      if (url.startsWith("/api/me/spend/daily")) return jsonResponse([]);
      return jsonResponse([]);
    }));

    renderSpend(memberMe);
    await waitFor(() => {
      expect(callLog.some((c) => c.url.startsWith("/api/me/spend/daily"))).toBe(true);
    });
    // Non-admin: no /api/users query fired.
    const usersCall = callLog.find((c) => c.url === "/api/users");
    expect(usersCall).toBeUndefined();
    expect(screen.queryByTestId("spend-scope-me")).toBeNull();
  });
});

// ---- 3. Hero card: cost headline + savings pct -----------------------

describe("Spend — hero card", () => {
  it("renders the trailing-window cost headline as the hero number", async () => {
    stubFetch((url) => {
      if (url === "/api/me") return jsonResponse(adminMe);
      if (url === "/api/me/spend/summary?days=30") {
        return summaryResponse(30, 1234.56, 1900, true);
      }
      if (url.startsWith("/api/me/spend/daily")) return jsonResponse([]);
      return summaryResponse(30, 0, 0, false);
    });

    renderSpend(adminMe);
    // The hero card paints `summary.current_cost_usd` as the headline.
    // We give the React Query cache a tick to resolve the in-flight
    // /api/me/spend/summary query — the first render paints a zero
    // fallback while summary is loading (no data yet).
    await waitFor(() => {
      const hero = screen.getByTestId("spend-hero-cost");
      expect(hero).toHaveTextContent(/\$1\.23K|\$1\.[2-9]K/i);
    }, { timeout: 3000 });
    expect(screen.getByTestId("spend-hero-cost")).not.toHaveTextContent(/tokens/i);
  });

  it("flips the savings pct colour: negative = warn, positive = ok", async () => {
    stubFetch((url) => {
      if (url === "/api/me") return jsonResponse(adminMe);
      if (url === "/api/me/spend/summary?days=30") {
        // current=1900 previous=1234.56 → cost INCREASED 54.0%
        return summaryResponse(30, 1900, 1234.56, true);
      }
      if (url.startsWith("/api/me/spend/daily")) return jsonResponse([]);
      return summaryResponse(30, 0, 0, false);
    });

    renderSpend(adminMe);
    await waitFor(() => {
      const tile = screen.getByTestId("spend-hero-savings");
      expect(tile.querySelector(".is-negative")).not.toBeNull();
      expect(tile).toHaveTextContent(/-53\.9%/);
    });
  });

  it("shows `—` for the savings pct when there is no previous window", async () => {
    stubFetch((url) => {
      if (url === "/api/me") return jsonResponse(adminMe);
      if (url === "/api/me/spend/summary?days=30") {
        return summaryResponse(30, 100, 0, false);
      }
      if (url.startsWith("/api/me/spend/daily")) return jsonResponse([]);
      return summaryResponse(30, 0, 0, false);
    });

    renderSpend(adminMe);
    await waitFor(() => {
      const tile = screen.getByTestId("spend-hero-savings");
      expect(tile).toHaveTextContent("—");
      expect(tile).toHaveTextContent(/no previous window/i);
    });
  });
});
