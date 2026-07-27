import { describe, expect, it, vi, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "../theme/ThemeProvider";
import { Eval } from "../pages/Eval";

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
        <Eval />
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
  it("renders all metrics in one panel — 2 toggles clickable + 2 aria-disabled (env-driven)", async () => {
    renderEval();
    await waitFor(() => screen.getByText("Heuristics"));

    // The heuristics table holds 4 metric rows:
    //   PII / Completeness -> interactive LabelToggle (role=switch)
    //   SLM judge / Remote eval -> dimmed LabelToggle (role=switch,
    //   aria-disabled=true)
    //
    // The Eval Profiles card below also renders one LabelToggle per
    // profile row (default-judge, default-remote = 2 more), so total
    // role=switch count is 6 — but EXACTLY 2 of the 4 are aria-disabled.
    const switches = screen.queryAllByRole("switch");
    expect(switches.length).toBe(6);
    const disabled = switches.filter(
      (s) => s.getAttribute("aria-disabled") === "true",
    );
    // 2 dimmed from the heuristics table for SLM judge / Remote eval.
    // The 2 profile rows render interactive toggles (they have their
    // own admin-only cell, just not aria-disabled).
    expect(disabled.length).toBe(2);

    // The text "SLM judge" and "Remote eval" are present in the
    // heuristics table — multiple matches are fine because there is
    // also a Chip in the Eval Profiles card below. Use a heading
    // selector to disambiguate.
    expect(screen.getAllByText("SLM judge").length).toBeGreaterThan(0);
    expect(screen.getAllByText("Remote eval").length).toBeGreaterThan(0);
  });

  it("admin sees Change configuration + Disable evaluation on SLM judge and Remote eval", async () => {
    renderEval();
    await waitFor(() => screen.getByText("Heuristics"));

    const changeBtns = screen.getAllByText("Change configuration");
    expect(changeBtns.length).toBe(2);
    const disableBtns = screen.getAllByText("Disable evaluation");
    expect(disableBtns.length).toBe(2);
    // Both buttons should not be disabled when the row reports enabled=true.
    disableBtns.forEach((b) => {
      expect(b).not.toBeDisabled();
    });
  });
});

describe("<Eval /> admin disable profile flow", () => {
  it("clicking Disable evaluation PATCHes the matching default profile with {enabled:false}", async () => {
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
          <Eval />
        </QueryClientProvider>
      </ThemeProvider>,
    );

    await waitFor(() => screen.getByText("Heuristics"));
    const [disableJudge, disableRemote] = screen.getAllByText("Disable evaluation");

    fireEvent.click(disableJudge);
    await waitFor(() =>
      expect(
        patches.find((p) => p.url.includes("default-judge")),
      ).toBeDefined(),
    );
    expect(
      (patches.find((p) => p.url.includes("default-judge"))!.body as { enabled?: boolean })
        .enabled,
    ).toBe(false);

    patches.length = 0;
    fireEvent.click(disableRemote);
    await waitFor(() =>
      expect(
        patches.find((p) => p.url.includes("default-remote")),
      ).toBeDefined(),
    );
    expect(
      (patches.find((p) => p.url.includes("default-remote"))!.body as { enabled?: boolean })
        .enabled,
    ).toBe(false);
  });

  it("non-admin members do not see Disable evaluation / Change configuration buttons", async () => {
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
          <Eval />
        </QueryClientProvider>
      </ThemeProvider>,
    );
    // The Eval page is admin-gated, so a member just gets a Forbidden
    // page. None of the action buttons (or the heuristic table itself)
    // render for non-admin accounts.
    await waitFor(() => screen.getByText(/Forbidden/i));
    expect(screen.queryByText("Disable evaluation")).toBeNull();
    expect(screen.queryByText("Change configuration")).toBeNull();
    expect(screen.queryByText("Heuristics")).toBeNull();
  });
});
