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
    stubFetch((url) => {
      if (url === "/api/me") return jsonResponse(adminMe);
      if (url.startsWith("/api/me/spend/daily?days=")) {
        return jsonResponse([
          { day: "2026-08-11", cost_usd: 1.23, tokens: 1000, requests: 4, cache_hits: 1 },
          { day: "2026-08-10", cost_usd: 0.5, tokens: 500, requests: 2, cache_hits: 0 },
        ]);
      }
      return jsonResponse([]);
    });

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
    stubFetch((url) => {
      if (url === "/api/me") return jsonResponse(adminMe);
      if (url.startsWith("/api/me/spend/daily?days=")) {
        return jsonResponse([]);
      }
      return jsonResponse([]);
    });

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
    stubFetch((url) => {
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
    });

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
});

// ---- 2. Admin scope switcher: /api/users/{id}/spend/daily -------------

describe("Spend — admin scope", () => {
  it("lists members and switches fetches to /api/users/{id}/spend/daily", async () => {
    stubFetch((url) => {
      if (url === "/api/me") return jsonResponse(adminMe);
      if (url === "/api/users") return jsonResponse([otherUser]);
      if (url.startsWith("/api/me/spend/daily?days=")) return jsonResponse([]);
      if (url === "/api/users/u-other/spend/daily?days=30") {
        return jsonResponse([
          { day: "2026-08-11", cost_usd: 9.99, tokens: 8000, requests: 20, cache_hits: 0 },
        ]);
      }
      return jsonResponse([]);
    });

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
    stubFetch((url) => {
      if (url === "/api/me") return jsonResponse(memberMe);
      if (url.startsWith("/api/me/spend/daily")) return jsonResponse([]);
      return jsonResponse([]);
    });

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
