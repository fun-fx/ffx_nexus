import { describe, expect, it, vi, afterEach } from "vitest";
import { render, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { ThemeProvider } from "../theme/ThemeProvider";
import { Playground } from "../pages/Playground";

const adminMe = {
  id: "u-admin",
  email: "admin@nexus.local",
  role: "admin" as const,
  org_id: "o1",
};

// Useful stand-in for the gate /whoami/etc. probes the playground mounts.
function minimalFetch() {
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    if (url.endsWith("/api/me")) {
      return new Response(JSON.stringify(adminMe), { status: 200 });
    }
    if (url.endsWith("/api/me/playground/catalog")) {
      return new Response(
        JSON.stringify({
          chat: ["openai/gpt-4o", "anthropic/claude-opus-4"],
          embed: [],
          user: [
            {
              provider: "teamprov",
              models: ["teamprov/gpt-5"],
              scope: "org",
            },
            {
              provider: "aliceprov",
              models: ["aliceprov/llama3"],
              scope: "user",
              owner_id: "u-admin",
            },
          ],
        }),
        { status: 200 },
      );
    }
    if (url.endsWith("/api/me/keys")) {
      return new Response(JSON.stringify([]), { status: 200 });
    }
    if (url.endsWith("/api/me/credentials")) {
      return new Response(JSON.stringify([]), { status: 200 });
    }
    return new Response("{}", { status: 200 });
  });
}

function renderPlayground() {
  vi.stubGlobal("fetch", minimalFetch());
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={["/playground"]}>
          <Playground />
        </MemoryRouter>
      </QueryClientProvider>
    </ThemeProvider>,
  );
}

afterEach(() => vi.unstubAllGlobals());

async function pickModelSelect(): Promise<HTMLSelectElement> {
  // The Playground renders two <select>s (key picker + model picker).
  // Pick the model picker by finding the "Public providers" optgroup
  // label — that group is always populated (it carries the "auto"
  // sentinel) so the test doesn't race the React Query catalog fetch.
  await waitFor(() => {
    const groups = Array.from(
      document.querySelectorAll("optgroup"),
    ) as HTMLOptGroupElement[];
    if (!groups.some((g) => g.label.trim() === "Public providers")) {
      throw new Error("model picker not yet rendered");
    }
  });
  // Give the React Query catalog fetch a tick to settle so all related
  // assertions see the populated state. Without this the second render
  // hasn't kicked in yet and the team/personal groups look empty.
  await new Promise((r) => setTimeout(r, 200));
  const publicGroup = document.querySelector(
    'optgroup[label="Public providers"]',
  ) as HTMLOptGroupElement;
  return publicGroup.parentElement as HTMLSelectElement;
}

describe("<Playground /> model picker", () => {
  it("groups models by visibility scope", async () => {
    renderPlayground();
    const select = await pickModelSelect();
    expect(select).toBeInTheDocument();

    const groups = select.querySelectorAll("optgroup");
    const labels = Array.from(groups).map((g) => g.getAttribute("label"));
    expect(labels).toEqual([
      "Public providers",
      "Team routers",
      "Personal routers",
    ]);

    const publicOpts = Array.from(
      groups[0].querySelectorAll("option"),
    ).map((o) => o.textContent);
    expect(publicOpts).toContain("auto");
    expect(publicOpts).toContain("openai/gpt-4o");
    expect(publicOpts).toContain("anthropic/claude-opus-4");

    const teamOpts = Array.from(
      groups[1].querySelectorAll("option"),
    ).map((o) => o.textContent);
    expect(teamOpts).toEqual(["teamprov/gpt-5"]);

    const personalOpts = Array.from(
      groups[2].querySelectorAll("option"),
    ).map((o) => o.textContent);
    expect(personalOpts).toEqual(["aliceprov/llama3"]);
  });

  it("falls back to labelling unknown scopes as public (legacy server)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === "string" ? input : input.toString();
        if (url.endsWith("/api/me")) {
          return new Response(JSON.stringify(adminMe), { status: 200 });
        }
        if (url.endsWith("/api/me/playground/catalog")) {
          return new Response(
            JSON.stringify({
              chat: [],
              embed: [],
              user: [
                {
                  provider: "legacyprov",
                  models: ["legacyprov/foo", "legacyprov/bar"],
                },
              ],
            }),
            { status: 200 },
          );
        }
        return new Response("[]", { status: 200 });
      }),
    );
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <ThemeProvider>
        <QueryClientProvider client={qc}>
          <MemoryRouter initialEntries={["/playground"]}>
            <Playground />
          </MemoryRouter>
        </QueryClientProvider>
      </ThemeProvider>,
    );
    const select = await pickModelSelect();
    const publicGroup = select.querySelector(
      'optgroup[label="Public providers"]',
    )!;
    const publicOpts = Array.from(
      publicGroup.querySelectorAll("option"),
    ).map((o) => o.textContent);
    expect(publicOpts).toEqual(
      expect.arrayContaining(["legacyprov/foo", "legacyprov/bar"]),
    );
  });

  it("selecting a team router updates the model input", async () => {
    renderPlayground();
    const select = await pickModelSelect();
    // Wait until the catalog fetch populates the team router option.
    await waitFor(() => {
      const teamGroup = select.querySelector(
        'optgroup[label="Team routers"]',
      ) as HTMLOptGroupElement | null;
      expect(
        Array.from(teamGroup?.querySelectorAll("option") ?? [])
          .some((o) => o.value === "teamprov/gpt-5"),
      ).toBe(true);
    });
    fireEvent.change(select, { target: { value: "teamprov/gpt-5" } });
    await waitFor(() => {
      expect(select.value).toBe("teamprov/gpt-5");
    });
  });
});
