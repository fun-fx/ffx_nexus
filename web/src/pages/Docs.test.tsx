import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Docs } from "./Docs";

const docIndex = {
  title: "Nexus documentation",
  tagline: "Run a Nexus cluster with eyes open",
  categories: [
    {
      slug: "concepts",
      title: "Concepts",
      entries: [
        {
          path: "quickstart",
          title: "Quickstart",
          summary: "Five minutes to first call.",
          category: "concepts",
          order: 1,
          status: "stable",
          bytes: 1024,
        },
        {
          path: "model-benchmarks",
          title: "Model benchmarks",
          summary: "Hosted eval of model quality.",
          category: "concepts",
          order: 5,
          status: "v1beta",
          bytes: 4096,
        },
      ],
    },
    {
      slug: "operations",
      title: "Operations",
      entries: [
        {
          path: "observability/grafana-helm-toggle",
          title: "Grafana Helm toggle",
          summary: "Wire Grafana into the cluster.",
          category: "operations",
          order: 3,
          status: "stable",
          bytes: 2048,
        },
      ],
    },
  ],
  quick_links: [
    {
      path: "quickstart",
      title: "Quickstart",
      summary: "Five minutes to first call.",
      category: "concepts",
      order: 1,
      bytes: 1024,
    },
    {
      path: "model-benchmarks",
      title: "Model benchmarks",
      summary: "Hosted eval of model quality.",
      category: "concepts",
      order: 5,
      status: "v1beta",
      bytes: 4096,
    },
  ],
};

const quickstartPage = {
  path: "quickstart",
  title: "Nexus Quickstart",
  summary: "Five minutes from `curl` to first response.",
  category: "concepts",
  order: 1,
  status: "stable",
  source_path: "quickstart.md",
  bytes: 1024,
  body: [
    "# Nexus Quickstart",
    "",
    "Five minutes from `curl ... | bash` to your first chat completion.",
    "",
    "## Run the install",
    "",
    "- Install Docker",
    "- Run the script",
    "",
    "## Verify",
    "",
    "After install, hit `http://localhost:8091` and you should see the console.",
    "",
    "## Where to go next",
  ].join("\n"),
};

type FetchResponse = { status: number; body: unknown };
type FetchHandler = {
  match: (url: string) => boolean;
  respond: () => Promise<FetchResponse>;
};

function jsonRes(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function mockFetch(handlers: FetchHandler[]) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      for (const h of handlers) {
        if (h.match(url)) {
          const r = await h.respond();
          return jsonRes(r.body, r.status);
        }
      }
      return jsonRes({ error: "no fixture" }, 404);
    }),
  );
}

function renderDocs(initialPath: string) {
  const c = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  return render(
    <QueryClientProvider client={c}>
      <MemoryRouter initialEntries={[initialPath]}>
        <Routes>
          <Route path="/docs" element={<Docs />} />
          <Route path="/docs/*" element={<Docs />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  document.body.innerHTML = "";
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("Docs", () => {
  it("renders the docs index hero + quick links", async () => {
    mockFetch([
      {
        match: (url) => url === "/api/docs",
        respond: () => Promise.resolve({ status: 200, body: docIndex }),
      },
    ]);
    renderDocs("/docs");
    // The hero block reflects the index's `tagline` (or falls back
    // to the title). The fixture supplies both, so we expect the
    // tagline here as a proxy for "the index was fetched and
    // rendered." Either way, the tagline renders twice — once in
    // the h1 and once in the subparagraph — so we use findAllByText
    // and demand at least one.
    await screen.findAllByText(docIndex.tagline, {}, { timeout: 2000 });
    // Quick link titles appear in the hero grid AND the sidebar;
    // using getAllByText means we do not care which one wins —
    // only that at least one is present.
    const quickstart = await screen.findAllByText("Quickstart");
    expect(quickstart.length).toBeGreaterThanOrEqual(1);
    expect((await screen.findAllByText("Model benchmarks")).length).toBeGreaterThanOrEqual(1);
  });

  it("highlights the active page in the sidebar", async () => {
    mockFetch([
      {
        match: (url) => url === "/api/docs",
        respond: () => Promise.resolve({ status: 200, body: docIndex }),
      },
      {
        match: (url) => url.startsWith("/api/docs/quickstart"),
        respond: () => Promise.resolve({ status: 200, body: quickstartPage }),
      },
    ]);
    renderDocs("/docs/quickstart");
    await waitFor(() => {
      const links = screen.getAllByRole("link", { name: /quickstart/i });
      expect(links.some((l) => l.className.includes("is-active"))).toBe(true);
    });
  });

  it("renders markdown body for a page", async () => {
    mockFetch([
      {
        match: (url) => url === "/api/docs",
        respond: () => Promise.resolve({ status: 200, body: docIndex }),
      },
      {
        match: (url) => url.startsWith("/api/docs/quickstart"),
        respond: () => Promise.resolve({ status: 200, body: quickstartPage }),
      },
    ]);
    renderDocs("/docs/quickstart");
    // The article body and the on-this-page rail both render the
    // H2 headings, so we expect at least 2 occurrences of "Run the
    // install" once the page has loaded.
    const matches = await screen.findAllByText("Run the install");
    expect(matches.length).toBeGreaterThanOrEqual(2);
  });

  it("renders a 404 panel when the page is missing", async () => {
    mockFetch([
      {
        match: (url) => url === "/api/docs",
        respond: () => Promise.resolve({ status: 200, body: docIndex }),
      },
      {
        match: (url) => url.startsWith("/api/docs/missing"),
        respond: () => Promise.resolve({ status: 404, body: { error: "no entry" } }),
      },
    ]);
    renderDocs("/docs/missing-page");
    expect(await screen.findByText("No such page")).toBeTruthy();
    expect(await screen.findByText("the index")).toBeTruthy();
  });

  it("renders a sign-in gate when /api/docs returns 401", async () => {
    mockFetch([
      {
        match: (url) => url === "/api/docs",
        respond: () =>
          Promise.resolve({
            status: 401,
            body: { error: "login required" },
          }),
      },
    ]);
    renderDocs("/docs");
    // Two elements render "Sign in" — the hero copy and the link label
    // — because both are intentional parts of the gate (the copy
    // explains *what* to do, the link is the *how*). Demand at least
    // one so the test does not depend on exact-match.
    expect((await screen.findAllByText(/sign in/i)).length).toBeGreaterThanOrEqual(1);
    const allLinks = await screen.findAllByRole("link");
    const loginLink = allLinks.find(
      (a) =>
        a.getAttribute("href")?.startsWith("/login") &&
        a.getAttribute("href")?.includes("%2Fdocs"),
    );
    expect(loginLink).toBeTruthy();
  });

  it("renders a 'temporarily unavailable' panel when /api/docs returns 5xx", async () => {
    mockFetch([
      {
        match: (url) => url === "/api/docs",
        respond: () =>
          Promise.resolve({
            status: 503,
            body: { error: "docs not configured" },
          }),
      },
    ]);
    renderDocs("/docs");
    expect(await screen.findByText(/temporarily unavailable/i)).toBeTruthy();
  });
});
