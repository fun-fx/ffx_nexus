/* @vitest-environment jsdom */
import "@testing-library/jest-dom";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { ThemeProvider } from "../theme/ThemeProvider";
import { vi } from "vitest";
import { EvalProfilesCard } from "./EvalProfiles";

function makeClient() {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false, staleTime: 0, gcTime: 0 },
    },
  });
}

function wrap(node: React.ReactNode) {
  return render(
    <QueryClientProvider client={makeClient()}>
      <ThemeProvider>
        <MemoryRouter>{node}</MemoryRouter>
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

function mockFetchSequence(scenarios: Array<() => Response | Promise<Response>>) {
  let i = 0;
  const originalFetch: typeof fetch | undefined = globalThis.fetch;
  globalThis.fetch = vi.fn(async () => {
    const scenario = scenarios[i++] ?? scenarios[scenarios.length - 1];
    return scenario();
  }) as unknown as typeof fetch;
  return () => {
    globalThis.fetch = originalFetch as typeof fetch;
  };
}

describe("<EvalProfilesCard />", () => {
  it("renders empty state when no profiles", async () => {
    const restore = mockFetchSequence([() => new Response(JSON.stringify({ profiles: [] }), { status: 200 })]);
    try {
      wrap(<EvalProfilesCard isAdmin />);
      await waitFor(() => expect(screen.queryByText(/No profiles/i)).toBeInTheDocument());
    } finally {
      restore();
    }
  });

  it("lists profiles and groups them by scope", async () => {
    const restore = mockFetchSequence([
      () =>
        new Response(
          JSON.stringify({
            profiles: [
              {
                id: "ep_org",
                name: "Org judge",
                kind: "slm_judge",
                scope: "org",
                endpoint: { base_url: "http://judge", model: "m", key_source: "org" },
                sample_rate: 0.4,
                enabled: true,
                metrics: ["answer_relevancy"],
              },
              {
                id: "ep_user",
                name: "Mine safety",
                kind: "remote_eval",
                scope: "user",
                owner_user_id: "u-1",
                endpoint: { base_url: "http://x", key_source: "inline", key_ref: "tk-1" },
                sample_rate: 1.0,
                enabled: true,
                metrics: ["toxicity"],
              },
            ],
          }),
          { status: 200 },
        ),
    ]);
    try {
      wrap(<EvalProfilesCard isAdmin />);
      await waitFor(() => expect(screen.getByText("Org judge")).toBeInTheDocument());
      expect(screen.getByText("Mine safety")).toBeInTheDocument();
      expect(screen.getByText(/40%/)).toBeInTheDocument();
      expect(screen.getByText(/100%/)).toBeInTheDocument();
      expect(screen.getAllByText(/Org profiles/i).length).toBeGreaterThan(0);
      expect(screen.getAllByText(/My profiles/i).length).toBeGreaterThan(0);
    } finally {
      restore();
    }
  });

  it("creates a profile via the drawer", async () => {
    let createdPayload: unknown = null;
    const restore = mockFetchSequence([
      () => new Response(JSON.stringify({ profiles: [] }), { status: 200 }),
      () => {
        // Capture the body of the create request.
        return {
          status: 201,
          json: async () => ({
            id: "ep_new",
            name: "My judge",
            kind: "slm_judge",
            scope: "org",
            endpoint: { base_url: "http://judge", model: "m", key_source: "org" },
            sample_rate: 0.5,
            enabled: true,
            metrics: ["toxicity"],
            threshold: 0.5,
          }),
        } as unknown as Response;
      },
      () => new Response(JSON.stringify({ profiles: [] }), { status: 200 }),
    ]);

    // Wrap fetch to grab the create payload.
    const realFetch = globalThis.fetch;
    globalThis.fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (typeof input === "string" && input.includes("/eval/profiles") && init?.method === "POST") {
        createdPayload = JSON.parse(init.body as string);
      }
      return realFetch(input as RequestInfo, init);
    }) as unknown as typeof fetch;

    const user = userEvent.setup();
    try {
      wrap(<EvalProfilesCard isAdmin />);
      await screen.findByText(/No profiles/i);
      await user.click(screen.getByRole("button", { name: /New profile/i }));
      await screen.findByTestId("profile-drawer");
      await user.type(screen.getByPlaceholderText(/Claude/), "My judge");
      await user.click(screen.getByTestId("profile-submit"));
      await waitFor(() => {
        expect(createdPayload).toBeTruthy();
      });
      expect(createdPayload).toMatchObject({ name: "My judge", kind: "slm_judge" });
    } finally {
      restore();
    }
  });

  it("disables key source other than builtin for heuristic kinds", async () => {
    const restore = mockFetchSequence([
      () => new Response(JSON.stringify({ profiles: [] }), { status: 200 }),
    ]);
    try {
      wrap(<EvalProfilesCard isAdmin />);
      await waitFor(() => expect(screen.queryByText(/No profiles/i)).toBeInTheDocument());
      const user = userEvent.setup();
      await user.click(screen.getByRole("button", { name: /New profile/i }));
      const drawer = within(screen.getByTestId("profile-drawer"));
      const kindSelect = drawer.getAllByRole("combobox")[0];
      await user.selectOptions(kindSelect, "heuristic_pii");
      // The key_source select is the second combobox; builtin should be
      // auto-selected and other options disabled.
      const allCombos = drawer.getAllByRole("combobox");
      const keySourceSelect = allCombos[allCombos.length - 1];
      expect((keySourceSelect as HTMLSelectElement).value).toBe("builtin");
    } finally {
      restore();
    }
  });
});
