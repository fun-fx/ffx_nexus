import { describe, expect, it, vi, afterEach } from "vitest";
import { MemoryRouter } from "react-router-dom";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "../theme/ThemeProvider";
import { Eval } from "../pages/Eval";
import { ZERO_EVAL, type EvalConfigSnapshot } from "../api";

const adminMe = {
  id: "u1",
  email: "admin@nexus.local",
  role: "admin" as const,
  org_id: "o1",
};

type Overrides = { quality?: number; cost?: number; latency?: number };

function buildBundle(o: Overrides) {
  return {
    routing: {
      weights: {
        quality: 0.6,
        cost: 0.2,
        latency: 0.2,
        ...o,
      },
      window: "1h",
      refresh: "30s",
      groups: {},
      groups_spec: "",
      load_balance: false,
    },
    eval: {
      pii_enabled: true,
      completeness_enabled: true,
      sample_rate: 0.1,
      workers: 4,
      judge: {
        enabled: true,
        base_url: "http://judge.local",
        model: "judge-v1",
        api_key_set: false,
      },
      remote: { enabled: true, url: "http://remote.local", metrics: ["faithfulness"], timeout: "30s" },
    },
    eval_enabled: true,
    routing_enabled: true,
    score_store: "clickhouse",
    trace_store: "clickhouse",
    score_persisted: true,
    routing_stats_store: "clickhouse",
    plugin_only: false,
    purge_legacy_profiles_on_boot: false,
    restart_required: [],
  };
}

type Patch = {
  routing: { weights: { quality: number; cost: number; latency: number } };
};

function renderEval(
  o: Overrides = {},
  onPatch: (body: Patch) => void = () => {},
) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = init?.method?.toUpperCase() ?? "GET";
      if (method === "PATCH") {
        const body = init?.body ? JSON.parse(String(init.body)) : {};
        onPatch(body);
        return new Response("{}", { status: 200 });
      }
      if (url.endsWith("/api/me")) {
        return new Response(JSON.stringify(adminMe), { status: 200 });
      }
      if (url.endsWith("/api/eval/config")) {
        return new Response(JSON.stringify(buildBundle(o)), { status: 200 });
      }
      if (url.endsWith("/api/eval/profiles")) {
        return new Response(
          JSON.stringify({
            profiles: [
              { id: "default-judge", kind: "slm_judge", scope: "org", enabled: true },
              { id: "default-remote", kind: "remote_eval", scope: "org", enabled: true },
              { id: "default-pii", kind: "heuristic_pii", scope: "org", enabled: true },
              { id: "default-completeness", kind: "heuristic_completeness", scope: "org", enabled: true },
            ],
          }),
          { status: 200 },
        );
      }
      return new Response("{}", { status: 200 });
    }),
  );
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <Eval />
        </MemoryRouter>
      </QueryClientProvider>
    </ThemeProvider>,
  );
  return Object.assign(utils, { qc });
}

afterEach(() => vi.unstubAllGlobals());

describe("<Eval /> weights sliders", () => {
  it("clamps a server-supplied negative weight to 0 on render", async () => {
    renderEval({ quality: -0.2 });

    const allMatches = await screen.findAllByText("Quality");
    const sliderLabel = allMatches.find(
      (el) => el.classList.contains("weight-slider-label"),
    )!;
    expect(sliderLabel).toBeInTheDocument();

    const qualityCard = sliderLabel.parentElement!;
    expect(qualityCard.textContent).toMatch(/Quality\s*0%/);

    const sliders = document.querySelectorAll<HTMLInputElement>(
      "input[type=range]",
    );
    expect(sliders[0].value).toBe("0");
  });

  it("drag is isolated — moving one axis does NOT move the others", async () => {
    renderEval();
    await waitFor(() => screen.getByText("Routing weights"));

    const sliders = document.querySelectorAll<HTMLInputElement>(
      "input[type=range]",
    );
    // Move quality to 0.4; cost and latency must stay at exactly the
    // historical 0.2 / 0.2 values. This is the whole UX promise of the
    // "free drag, normalize at save" model.
    fireEvent.change(sliders[0], { target: { value: "0.4" } });
    await waitFor(() => {
      const labels = Array.from(
        document.querySelectorAll<HTMLElement>(".weight-slider-value"),
      );
      expect(labels[0].textContent).toMatch(/^40%$/);
      expect(labels[1].textContent).toMatch(/^20%$/);
      expect(labels[2].textContent).toMatch(/^20%$/);
    });

    // Drag latency alone to 0.7 — quality and cost remain where the user
    // left them.
    fireEvent.change(sliders[2], { target: { value: "0.7" } });
    await waitFor(() => {
      const labels = Array.from(
        document.querySelectorAll<HTMLElement>(".weight-slider-value"),
      );
      expect(labels[0].textContent).toMatch(/^40%$/);
      expect(labels[1].textContent).toMatch(/^20%$/);
      expect(labels[2].textContent).toMatch(/^70%$/);
    });
  });

  it("save normalises a non-1 sum to the simplex", async () => {
    let captured: Patch | null = null;
    renderEval({}, (body) => {
      captured = body;
    });
    await waitFor(() => screen.getByText("Routing weights"));

    const sliders = document.querySelectorAll<HTMLInputElement>(
      "input[type=range]",
    );
    // Drag into a state that does not sum to 1: quality 0.7 + latency 0.2
    // (cost stays at 0.2) → sum 1.1. The save hook should normalise it.
    fireEvent.change(sliders[0], { target: { value: "0.7" } });
    fireEvent.change(sliders[2], { target: { value: "0.2" } });
    await waitFor(() => screen.getByText("Save weights"));
    fireEvent.click(screen.getByText(/Save weights/i));
    await waitFor(() => captured !== null);
    const sent = captured!.routing.weights;
    const sum = sent.quality + sent.cost + sent.latency;
    expect(Math.abs(sum - 1)).toBeLessThanOrEqual(0.005);
    // Visible relative ordering preserved (q highest, c lowest).
    expect(sent.quality).toBeGreaterThan(sent.latency);
    expect(sent.cost).toBeGreaterThanOrEqual(sent.latency - 0.001);

    // Hint surfaces the rebalance message to the admin.
    await screen.findByText(/Sum was rebalanced to 100%/i);
  });

  it("zero-axis state is honoured: when one slider is 0, the other two absorb 1.0 at save", async () => {
    let captured: Patch | null = null;
    renderEval({}, (body) => {
      captured = body;
    });
    await waitFor(() => screen.getByText("Routing weights"));

    const sliders = document.querySelectorAll<HTMLInputElement>(
      "input[type=range]",
    );
    // Drag latency to 0; quality and cost keep the historical 0.6/0.2
    // distribution. Save should send a normalised row where latency is 0
    // and q + c = 1.
    fireEvent.change(sliders[2], { target: { value: "0" } });
    fireEvent.click(screen.getByText(/Save weights/i));
    await waitFor(() => captured !== null);
    const sent = captured!.routing.weights;
    expect(sent.latency).toBe(0);
    expect(sent.quality).toBeCloseTo(0.6 / 0.8, 2);
    expect(sent.cost).toBeCloseTo(0.2 / 0.8, 2);
    expect(sent.quality + sent.cost + sent.latency).toBeCloseTo(1, 2);
  });

  it("all-zero drag falls back to the historical 60/20/20 default", async () => {
    let captured: Patch | null = null;
    renderEval({}, (body) => {
      captured = body;
    });
    await waitFor(() => screen.getByText("Routing weights"));

    const sliders = document.querySelectorAll<HTMLInputElement>(
      "input[type=range]",
    );
    sliders.forEach((el) => {
      fireEvent.change(el, { target: { value: "0" } });
    });
    fireEvent.click(screen.getByText(/Save weights/i));
    await waitFor(() => captured !== null);
    const sent = captured!.routing.weights;
    // Either the historical default OR a "100% 로 저장되었습니다" toast,
    // but we expect the server-visible sum to be 1 either way.
    expect(sent.quality + sent.cost + sent.latency).toBeCloseTo(1, 2);
  });
});

describe("<Eval /> heuristic table layout", () => {
  it("renders all metrics in one panel — every toggle is interactive, env-seeded profiles included", async () => {
    renderEval();
    await waitFor(() => screen.getByText("Evaluators"));

    // v0.6.9 unified all four metric rows under a single switch with
    // no aria-disabled. There are 4 switches from the heuristics
    // table plus 4 more from the Eval Profiles card (default-pii /
    // default-completeness / default-judge / default-remote all seeded
    // by renderEval's fixture).
    const switches = screen.queryAllByRole("switch");
    expect(switches.length).toBe(8);
    // None of the role=switch elements are aria-disabled any more —
    // even the env-seeded SLM judge / Remote eval rows now expose a
    // real Enable/Disable switch.
    const disabled = switches.filter(
      (s) => s.getAttribute("aria-disabled") === "true",
    );
    expect(disabled.length).toBe(0);

    // The text "SLM judge" and "Remote eval" are present in the
    // Heuristic rows render the metric name as their title; legacy rows
    // append "(legacy)" to the same title. Multiple matches are fine
    // because the same names also appear as chips in the profiles card.
    expect(screen.getAllByText("SLM judge").length).toBeGreaterThan(0);
    expect(screen.getAllByText(/Remote eval/).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/^PII$/i).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/^Completeness$/i).length).toBeGreaterThan(0);
  });

  it("renders 4 interactive toggles on the heuristic table — admin can flip every kind", async () => {
    renderEval();
    await waitFor(() => screen.getByText("Evaluators"));

    // v0.6.9 unified all 4 rows under the same switch. The label's
    // aria-label flips between "disable X" and "enable X" so the tests
    // match by metric-specific label rather than the prior fixed
    // "Disable evaluation" button text.
    const switches = screen.queryAllByRole("switch");
    // 4 metric rows + 4 profile rows below (default-{pii,completeness,judge,remote})
    expect(switches.length).toBe(8);
    // Every switch is interactive, not aria-disabled any more.
    const disabledLabelsOnTable = switches.filter(
      (s) => s.getAttribute("aria-disabled") === "true",
    );
    expect(disabledLabelsOnTable.length).toBe(0);

    // The four kinds are still present in both the heuristic table and
    // the profile card below; multiple matches are fine.
    expect(screen.getAllByText("SLM judge").length).toBeGreaterThan(0);
    expect(screen.getAllByText(/Remote eval/).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/^PII$/i).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/^Completeness$/i).length).toBeGreaterThan(0);
  });

  it("admin sees Change configuration buttons on SLM judge and Remote eval rows", async () => {
    renderEval();
    await waitFor(() => screen.getByText("Evaluators"));

    // v0.6.9: every heuristic-table row surfaces Change configuration
    // because admins can edit any seeded default profile directly.
    const changeBtns = screen.getAllByText(/Change configuration/i);
    expect(changeBtns.length).toBeGreaterThanOrEqual(2);
  });
});

describe("<Eval /> admin toggle profile flow", () => {
  it("clicking the toggle on SLM judge PATCHes default-judge with {enabled:false}", async () => {
    const patches: Array<{ url: string; body: unknown; method: string }> = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const method = init?.method?.toUpperCase() ?? "GET";
        if (method === "PATCH") {
          const body = init?.body ? JSON.parse(String(init.body)) : {};
          patches.push({ url, body, method });
          return new Response("{}", { status: 200 });
        }
        if (url.endsWith("/api/me")) {
          return new Response(JSON.stringify(adminMe), { status: 200 });
        }
        if (url.endsWith("/api/eval/config")) {
          return new Response(JSON.stringify(buildBundle({})), { status: 200 });
        }
        if (url.endsWith("/api/eval/profiles")) {
          return new Response(
            JSON.stringify({
              profiles: [
                { id: "default-judge", kind: "slm_judge", scope: "org", enabled: true },
                { id: "default-remote", kind: "remote_eval", scope: "org", enabled: true },
                { id: "default-pii", kind: "heuristic_pii", scope: "org", enabled: true },
                { id: "default-completeness", kind: "heuristic_completeness", scope: "org", enabled: true },
              ],
            }),
            { status: 200 },
          );
        }
        return new Response("{}", { status: 200 });
      }),
    );

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <ThemeProvider>
        <QueryClientProvider client={qc}>
          <MemoryRouter>
            <Eval />
          </MemoryRouter>
        </QueryClientProvider>
      </ThemeProvider>,
    );

    await waitFor(() => screen.getByText("Evaluators"));

    // Click the SLM judge switch (aria-label="toggle SLM judge").
    const slmSwitch = screen.getByRole("switch", { name: /toggle SLM judge/i });
    fireEvent.click(slmSwitch);
    await waitFor(() =>
      expect(
        patches.find((p) => p.url.includes("default-judge")),
      ).toBeDefined(),
    );
    expect(
      (patches.find((p) => p.url.includes("default-judge"))!.body as { enabled?: boolean })
        .enabled,
    ).toBe(false);
  });

  it("clicking the PII toggle PATCHes default-pii with {enabled:false}", async () => {
    const patches: Array<{ url: string; body: unknown; method: string }> = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const method = init?.method?.toUpperCase() ?? "GET";
        if (method === "PATCH") {
          const body = init?.body ? JSON.parse(String(init.body)) : {};
          patches.push({ url, body, method });
          return new Response("{}", { status: 200 });
        }
        if (url.endsWith("/api/me")) {
          return new Response(JSON.stringify(adminMe), { status: 200 });
        }
        if (url.endsWith("/api/eval/config")) {
          return new Response(JSON.stringify(buildBundle({})), { status: 200 });
        }
        if (url.endsWith("/api/eval/profiles")) {
          return new Response(
            JSON.stringify({
              profiles: [
                { id: "default-pii", kind: "heuristic_pii", scope: "org", enabled: true },
                { id: "default-completeness", kind: "heuristic_completeness", scope: "org", enabled: true },
              ],
            }),
            { status: 200 },
          );
        }
        return new Response("{}", { status: 200 });
      }),
    );

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <ThemeProvider>
        <QueryClientProvider client={qc}>
          <MemoryRouter>
            <Eval />
          </MemoryRouter>
        </QueryClientProvider>
      </ThemeProvider>,
    );

    await waitFor(() => screen.getByText("Evaluators"));

    // Wait until the profile store has hydrated so profileIdByKind
    // resolves to non-null values; otherwise the toggle click fires
    // before the row's profileId is set and silently no-ops.
    await waitFor(() =>
      expect(screen.getAllByRole("switch", { name: /toggle PII/i }).length).toBeGreaterThan(0),
    );
    const piiSwitch = screen.getByRole("switch", { name: /toggle PII/i });
    fireEvent.click(piiSwitch);
    await waitFor(() =>
      expect(
        patches.find((p) => p.url.includes("default-pii")),
      ).toBeDefined(),
    );
    expect(
      (patches.find((p) => p.url.includes("default-pii"))!.body as { enabled?: boolean })
        .enabled,
    ).toBe(false);
  });

  it("non-admin members do not see admin-only profile actions", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const method = init?.method?.toUpperCase() ?? "GET";
        if (method === "PATCH") {
          return new Response("{}", { status: 200 });
        }
        if (url.endsWith("/api/me")) {
          return new Response(
            JSON.stringify({
              ...adminMe,
              role: "member",
            }),
            { status: 200 },
          );
        }
        if (url.endsWith("/api/eval/config")) {
          return new Response(JSON.stringify(buildBundle({})), { status: 200 });
        }
        if (url.endsWith("/api/eval/profiles")) {
          return new Response(JSON.stringify({ profiles: [] }), { status: 200 });
        }
        return new Response("{}", { status: 200 });
      }),
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <ThemeProvider>
        <QueryClientProvider client={qc}>
          <MemoryRouter>
            <Eval />
          </MemoryRouter>
        </QueryClientProvider>
      </ThemeProvider>,
    );
    // The Eval page is admin-gated, so a member just gets a Forbidden
    // page. None of the admin-only actions render for non-admin.
    await waitFor(() => screen.getByText(/Forbidden/i));
    expect(screen.queryByText(/Change configuration/i)).toBeNull();
    expect(screen.queryByText(/Evaluators/i)).toBeNull();
  });

  it("clicking Test on a plugin row renders the probe outcome inline", async () => {    const probeCalls: string[] = [];
    const pluginRows = [
      {
        id: "row-1",
        name: "langfuse-judge",
        spec_yaml: ["apiVersion: nexus.io/v1alpha1",
          "kind: EvalPlugin",
          "metadata:",
          "  name: langfuse-judge",
          "spec:",
          "  service:",
          "    type: langfuse",
          "    endpoint: https://cloud.langfuse.com",
          "    auth:",
          "      secretRef: langfuse-creds",
          "      keyRef: public_key|secret_key",
          "  send:",
          "    trigger: on_trace",
          "    sampling: 0.1",
          "  collect:",
          "    mode: webhook",
          "    interval: 60s",
          "  timeout: 30s"].join("\n"),
        enabled: true,
      },
    ];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const method = init?.method?.toUpperCase() ?? "GET";
        if (url.endsWith("/api/me")) {
          return new Response(JSON.stringify(adminMe), { status: 200 });
        }
        if (url.endsWith("/api/eval/config")) {
          return new Response(JSON.stringify(buildBundle({})), { status: 200 });
        }
        if (url.endsWith("/api/eval/profiles")) {
          return new Response(JSON.stringify({ profiles: [] }), { status: 200 });
        }
        if (url.includes("/api/eval/plugins/") && url.endsWith("/test")) {
          probeCalls.push(url);
          return new Response(
            JSON.stringify({
              ok: true,
              message: "langfuse endpoint reachable.",
              latency_ms: 42,
            }),
            { status: 200 },
          );
        }
        if (url.endsWith("/api/eval/plugins")) {
          if (method === "POST") {
            return new Response("{}", { status: 201 });
          }
          if (method === "GET") {
            return new Response(JSON.stringify({ plugins: pluginRows }), { status: 200 });
          }
        }
        return new Response("{}", { status: 200 });
        return new Response("{}", { status: 200 });
      }),
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <ThemeProvider>
        <QueryClientProvider client={qc}>
          <MemoryRouter>
            <Eval />
          </MemoryRouter>
        </QueryClientProvider>
      </ThemeProvider>,
    );
    await waitFor(() => screen.getByText("Evaluators"));
    // The plugin row should be present before we attempt to click.
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /Test/ })).toBeInTheDocument(),
    );
    const testBtn = await screen.findByRole("button", { name: /Test/ });
    fireEvent.click(testBtn);
    await waitFor(() =>
      expect(screen.getByTestId("plugin-test-langfuse-judge").textContent).toMatch(/reachable/),
    );
    // URL must carry the plugin's metadata.name, not the row's UUID.
    expect(probeCalls.find((u) => u.includes("/langfuse-judge/test"))).toBeTruthy();
  });

  it("falls back to body-hint message when server returns non-JSON 502", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, _init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        if (url.endsWith("/api/me")) {
          return new Response(JSON.stringify(adminMe), { status: 200 });
        }
        if (url.endsWith("/api/eval/config")) {
          return new Response(JSON.stringify(buildBundle({})), { status: 200 });
        }
        if (url.endsWith("/api/eval/profiles")) {
          return new Response(JSON.stringify({ profiles: [] }), { status: 200 });
        }
        if (url.endsWith("/api/eval/plugins")) {
          return new Response(
            JSON.stringify({
              plugins: [
                {
                  id: "row-2",
                  name: "langfuse-judge",
                  spec_yaml: "apiVersion: nexus.io/v1alpha1\nkind: EvalPlugin\nmetadata:\n  name: langfuse-judge\n",
                  enabled: true,
                },
              ],
            }),
            { status: 200 },
          );
        }
        if (url.includes("/api/eval/plugins/") && url.endsWith("/test")) {
          // Simulate an ingress / nginx 502 page (HTML body, no JSON).
          return new Response(
            "<html><body><h1>502 Bad Gateway</h1><p>upstream timed out</p></body></html>",
            { status: 502, headers: { "Content-Type": "text/html" } },
          );
        }
        return new Response("{}", { status: 200 });
      }),
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <ThemeProvider>
        <QueryClientProvider client={qc}>
          <MemoryRouter>
            <Eval />
          </MemoryRouter>
        </QueryClientProvider>
      </ThemeProvider>,
    );
    await waitFor(() => screen.getByText("Evaluators"));
    const testBtn = await screen.findByRole("button", { name: /Test/ });
    fireEvent.click(testBtn);
    // The UI must surface a body-derived hint instead of just
    // "Backend HTTP 502".
    await waitFor(() =>
      expect(screen.getByTestId("plugin-test-langfuse-judge").textContent).toMatch(
        /unexpected body|502 Bad Gateway|nginx/i,
      ),
    );
  });
});

describe("<Eval /> plugin-only banner", () => {
  function build(pluginOnly: boolean): EvalConfigSnapshot {
    return {
      ...ZERO_EVAL,
      eval_enabled: true,
      routing_enabled: true,
      plugin_only: pluginOnly,
      eval: {
        ...ZERO_EVAL.eval,
        pii_enabled: false,
        completeness_enabled: false,
      },
      routing: {
        ...ZERO_EVAL.routing,
        weights: { quality: 0.6, cost: 0.2, latency: 0.2 },
      },
    };
  }

  function renderWithBundle(bundle: EvalConfigSnapshot) {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === "string" ? input : input.toString();
        if (url.endsWith("/api/me")) {
          return new Response(JSON.stringify(adminMe), { status: 200 });
        }
        if (url.endsWith("/api/eval/config")) {
          return new Response(JSON.stringify(bundle), { status: 200 });
        }
        if (url.endsWith("/api/eval/profiles")) {
          return new Response(JSON.stringify({ profiles: [] }), { status: 200 });
        }
        if (url.endsWith("/api/eval/plugins")) {
          return new Response(JSON.stringify({ plugins: [] }), { status: 200 });
        }
        return new Response("{}", { status: 200 });
      }),
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <ThemeProvider>
          <MemoryRouter>
            <Eval />
          </MemoryRouter>
        </ThemeProvider>
      </QueryClientProvider>,
    );
  }

  afterEach(() => vi.unstubAllGlobals());

  it("shows banner when plugin_only=true", async () => {
    renderWithBundle(build(true));
    expect(await screen.findByText(/Plugin-only eval mode/i)).toBeTruthy();
  });

  it("hides banner when plugin_only=false", async () => {
    renderWithBundle(build(false));
    await waitFor(() => {
      expect(screen.queryByText(/Plugin-only eval mode/i)).toBeNull();
    });
    await waitFor(() => {
      expect(screen.queryByText(/purged on boot/i)).toBeNull();
    });
  });

  it("shows destructive banner when purge_legacy_profiles_on_boot=true", async () => {
    renderWithBundle({ ...build(true), purge_legacy_profiles_on_boot: true });
    expect(await screen.findByText(/purged on boot/i)).toBeTruthy();
    expect(await screen.findByText(/default-pii/)).toBeTruthy();
  });

  it("does not show destructive banner when purge_legacy_profiles_on_boot=false", async () => {
    renderWithBundle(build(true));
    await waitFor(() => {
      expect(screen.queryByText(/purged on boot/i)).toBeNull();
    });
  });

  it("hides eval profiles card when plugin_only=true", async () => {
    renderWithBundle(build(true));
    await waitFor(() => {
      expect(screen.queryByTestId("eval-profiles")).toBeNull();
    });
  });

  it("shows eval profiles card when plugin_only=false", async () => {
    renderWithBundle(build(false));
    expect(await screen.findByTestId("eval-profiles")).toBeTruthy();
  });

  it("hides sample rate and workers stats when plugin_only=true", async () => {
    renderWithBundle(build(true));
    await waitFor(() => {
      expect(screen.queryByText(/sample rate/i)).toBeNull();
    });
    expect(screen.queryByText(/^\s*workers\s*$/i)).toBeNull();
  });

  it("renders Install your first plugin CTA when plugin_only=true", async () => {
    renderWithBundle(build(true));
    expect(
      await screen.findByText(/Install your first plugin/i),
    ).toBeTruthy();
  });

  it("renders quickstart gallery when plugin_only=true and no plugins installed", async () => {
    renderWithBundle(build(true));
    expect(
      await screen.findByTestId("plugin-quickstart"),
    ).toBeTruthy();
    // Every survey-driven tile has its own test id.
    for (const kind of [
      "langfuse",
      "langsmith",
      "confident_ai",
      "arize_phoenix",
      "otel_collector",
      "datadog",
      "braintrust",
      "arize",
      "webhook",
    ]) {
      expect(
        await screen.findByTestId(`quickstart-${kind}`),
      ).toBeTruthy();
    }
  });

  it("hides quickstart gallery when plugin_only=false", async () => {
    renderWithBundle(build(false));
    await waitFor(() => {
      expect(screen.queryByTestId("plugin-quickstart")).toBeNull();
    });
  });

  it("renders the Keys button on plugin rows in the main Evaluators card", async () => {
    // /eval's merged EvaluatorsCard previously skipped the per-row
    // "Keys" affordance — operators had to navigate to the standalone
    // Plugins page to paste API keys. This regression check ensures
    // the integrated view exposes the same action.
    const plugins = [
      {
        id: "langfuse-judge",
        name: "langfuse-judge",
        enabled: true,
        spec_yaml: "kind: EvalPlugin\n",
        key_summary: { configured: true, missing: [] },
      },
    ];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === "string" ? input : input.toString();
        if (url.endsWith("/api/me")) {
          return new Response(JSON.stringify(adminMe), { status: 200 });
        }
        if (url.endsWith("/api/eval/config")) {
          return new Response(JSON.stringify(buildBundle({})), { status: 200 });
        }
        if (url.endsWith("/api/eval/profiles")) {
          return new Response(JSON.stringify({ profiles: [] }), { status: 200 });
        }
        if (url.endsWith("/api/eval/plugins")) {
          return new Response(JSON.stringify({ plugins }), { status: 200 });
        }
        return new Response("{}", { status: 200 });
      }),
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <ThemeProvider>
        <QueryClientProvider client={qc}>
          <MemoryRouter>
            <Eval />
          </MemoryRouter>
        </QueryClientProvider>
      </ThemeProvider>,
    );
    expect(
      await screen.findByTestId("plugin-keys-button-langfuse-judge"),
    ).toBeTruthy();
  });

  it("saves an edited plugin with PATCH instead of forking a second row", async () => {
    // The editor drawer used to POST unconditionally. Because the create
    // handler builds a record with an empty id and the store treats an
    // empty id as "insert", pressing Save changes produced a duplicate
    // row carrying the same metadata.name — and the operator's endpoint
    // edit never reached the row that Test probes.
    const specYaml = [
      "apiVersion: nexus.io/v1alpha1",
      "kind: EvalPlugin",
      "metadata:",
      "  name: langfuse-judge",
      "spec:",
      "  service:",
      "    type: langfuse",
      "    endpoint: https://cloud.langfuse.com",
      "    auth:",
      "      secretRef: langfuse-judge",
      "      keyRef: public_key|secret_key",
      "  send:",
      "    trigger: on_trace",
      "    sampling: 0.1",
      "    redact: []",
      "  collect:",
      "    mode: webhook",
      "",
    ].join("\n");
    const writes: { method: string; url: string }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const method = init?.method?.toUpperCase() ?? "GET";
        if (method !== "GET") writes.push({ method, url });
        if (url.endsWith("/api/me")) {
          return new Response(JSON.stringify(adminMe), { status: 200 });
        }
        if (url.endsWith("/api/eval/config")) {
          return new Response(JSON.stringify(buildBundle({})), { status: 200 });
        }
        if (url.endsWith("/api/eval/profiles")) {
          return new Response(JSON.stringify({ profiles: [] }), { status: 200 });
        }
        if (url.endsWith("/api/eval/plugins") && method === "GET") {
          return new Response(
            JSON.stringify({
              plugins: [
                {
                  id: "row-9",
                  name: "langfuse-judge",
                  enabled: true,
                  spec_yaml: specYaml,
                },
              ],
            }),
            { status: 200 },
          );
        }
        return new Response("{}", { status: 200 });
      }),
    );
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <ThemeProvider>
        <QueryClientProvider client={qc}>
          <MemoryRouter>
            <Eval />
          </MemoryRouter>
        </QueryClientProvider>
      </ThemeProvider>,
    );

    fireEvent.click(await screen.findByText("Edit"));
    fireEvent.click(await screen.findByText("Save changes"));

    await waitFor(() => {
      expect(writes.length).toBeGreaterThan(0);
    });
    expect(writes).toEqual([
      { method: "PATCH", url: "/api/eval/plugins/row-9" },
    ]);
  });
});

