/* @vitest-environment jsdom */
import "@testing-library/jest-dom";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "../theme/ThemeProvider";
import { vi } from "vitest";
import { Users } from "./Users";

function makeClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 0, gcTime: 0 } },
  });
}

function wrap(node: React.ReactNode) {
  return render(
    <QueryClientProvider client={makeClient()}>
      <ThemeProvider>{node}</ThemeProvider>
    </QueryClientProvider>,
  );
}

function jsonRes(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("<Users /> invite flow", () => {
  beforeEach(() => {
    if (!navigator.clipboard) {
      Object.defineProperty(navigator, "clipboard", {
        value: { writeText: vi.fn().mockResolvedValue(undefined) },
        configurable: true,
      });
    } else {
      vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue(undefined);
    }
  });

  it("renders the Members + Invites panels for an admin and shows the Invite user button", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/api/me")) {
        return jsonRes(200, { id: "u-admin", org_id: "default", email: "a@x", role: "admin" });
      }
      if (url.endsWith("/api/users")) {
        return jsonRes(200, []);
      }
      if (url.endsWith("/api/invites")) {
        return jsonRes(200, []);
      }
      return jsonRes(404, { error: "not stubbed" });
    }) as unknown as typeof fetch;

    try {
      wrap(<Users />);
      // Wait for the page to render the admin heading + Invite user button.
      await screen.findByRole("button", { name: /Invite user/i });
      // Both sub-panels are mounted.
      expect(screen.getByRole("heading", { name: "Invites" })).toBeInTheDocument();
      expect(screen.getAllByText(/Members/i).length).toBeGreaterThanOrEqual(1);
    } finally {
      globalThis.fetch = originalFetch as typeof fetch;
    }
  });

  it("shows the invite URL after a successful POST /api/invites", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/api/me")) {
        return jsonRes(200, { id: "u-admin", org_id: "default", email: "a@x", role: "admin" });
      }
      if (url.endsWith("/api/users")) {
        return jsonRes(200, []);
      }
      if (url.endsWith("/api/invites")) {
        // Both GET (initial load, refetch) and POST (issue). For simplicity,
        // the stub answers both with the same success envelope; we don't
        // need route-by-route wiring for this test.
        return jsonRes(201, {
          id: "inv-1",
          org_id: "default",
          email: "fresh@x",
          role: "member",
          created_by: "u-admin",
          created_at: new Date().toISOString(),
          expires_at: new Date(Date.now() + 7 * 24 * 3600_000).toISOString(),
          url: "https://nexus.ffx.ai/invite/raw-token-1",
          token: "raw-token-1",
        });
      }
      return jsonRes(404, { error: "not stubbed" });
    }) as unknown as typeof fetch;

    try {
      wrap(<Users />);
      // Capture the request body to verify the wire payload later.
      const fetchSpy = vi.spyOn(globalThis, "fetch");
      const button = await screen.findByRole("button", { name: /Invite user/i });
      await act(async () => {
        fireEvent.click(button);
      });
      // Drawer renders an `<input type="email">` for the email row.
      const emailInputs = await screen.findAllByDisplayValue("");
      const emailInput = emailInputs.find((el) => (el as HTMLInputElement).type === "email") as HTMLInputElement;
      await act(async () => {
        fireEvent.change(emailInput, { target: { value: "fresh@x" } });
      });
      const form = emailInput.closest("form") as HTMLFormElement;
      await act(async () => {
        form.requestSubmit();
      });
      // The fetch call recorded with POST + fresh@x payload.
      await waitFor(() => {
        const calls = fetchSpy.mock.calls;
        const match = calls.find(
          ([u, init]) =>
            String(u).endsWith("/api/invites") &&
            ((init as RequestInit | undefined)?.method ?? "GET").toUpperCase() === "POST" &&
            String(((init as RequestInit | undefined)?.body ?? "")).includes("fresh@x"),
        );
        expect(match).toBeTruthy();
      });
      // Result banner surfaces the URL the server returned.
      await waitFor(() => {
        expect(screen.getByTestId("invite-url")).toHaveTextContent("raw-token-1");
      });
      expect(screen.getByTestId("invite-copy")).toBeInTheDocument();
    } finally {
      globalThis.fetch = originalFetch as typeof fetch;
    }
  });
});
