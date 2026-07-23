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
        enabled: false,
        base_url: "",
        model: "",
        api_key_set: false,
      },
      remote: { enabled: false, url: "", metrics: [], timeout: "" },
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

function renderEval(o: Overrides = {}) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      if (init?.method === "PATCH") return new Response("{}", { status: 200 });
      if (url.endsWith("/api/me")) {
        return new Response(JSON.stringify(adminMe), { status: 200 });
      }
      if (url.endsWith("/api/eval/config")) {
        return new Response(JSON.stringify(buildBundle(o)), { status: 200 });
      }
      return new Response("{}", { status: 200 });
    }),
  );
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <Eval />
      </QueryClientProvider>
    </ThemeProvider>,
  );
}

afterEach(() => vi.unstubAllGlobals());

describe("<Eval /> weights sliders", () => {
  it("clamps a server-supplied negative weight to 0 on render", async () => {
    renderEval({ quality: -0.2 });

    // Find the Quality label (the slider row, not the "Quality knobs" h1).
    const allMatches = await screen.findAllByText("Quality");
    // The slider row label is the second "Quality" instance (the first is the
    // gradient text in the page title "Quality knobs").
    const sliderLabel = allMatches.find(
      (el) => el.classList.contains("weight-slider-label"),
    )!;
    expect(sliderLabel).toBeInTheDocument();

    const qualityCard = sliderLabel.parentElement!;
    expect(qualityCard.textContent).toMatch(/Quality\s*0%/);

    const sliders = document.querySelectorAll<
      HTMLInputElement
    >("input[type=range]");
    expect(sliders[0].value).toBe("0");
  });

  it("auto-balances the remaining two axes so the sum stays at 1 on slider moves", async () => {
    renderEval();
    await waitFor(() => screen.getByText("Routing weights"));

    const sliders = document.querySelectorAll<HTMLInputElement>(
      "input[type=range]",
    );

    // Quality → 0 — the remaining 1.0 is split 50/50 across cost and latency
    // (equal share — their current values are both 0.2 so identical ratios).
    fireEvent.change(sliders[0], { target: { value: "0" } });
    await waitFor(() => {
      const labels = Array.from(
        document.querySelectorAll<HTMLElement>(".weight-slider-value"),
      );
      expect(labels[0].textContent).toMatch(/^0%$/);
      // Both partners get 50% (1.0 / 2 each).
      expect(labels[1].textContent).toMatch(/^50%$/);
      expect(labels[2].textContent).toMatch(/^50%$/);
    });

    // Push quality back to 1 — both partners drop to 0% but the *displayed*
    // sum stays a tight band around 100% (±rounding noise).
    fireEvent.change(sliders[0], { target: { value: "1" } });
    await waitFor(() => {
      const labels = Array.from(
        document.querySelectorAll<HTMLElement>(".weight-slider-value"),
      );
      expect(labels[0].textContent).toMatch(/^100%$/);
      expect(labels[1].textContent).toMatch(/^0%$/);
      expect(labels[2].textContent).toMatch(/^0%$/);
    });

    // Now widen one partner: move latency to 0.4. Cost must take the rest
    // (1 - 1 - 0.4 = -0.4 in absolute arithmetic, but redistribute keeps cost
    // at 0 because the only remaining slice for the secondary axis is 0
    // and latency already consumed the entire remaining budget).
    fireEvent.change(sliders[2], { target: { value: "0.4" } });
    await waitFor(() => {
      const labels = Array.from(
        document.querySelectorAll<HTMLElement>(".weight-slider-value"),
      );
      expect(labels[2].textContent).toMatch(/^40%$/);
      // Cost remains 0 because the q=1 ceiling leaves no residual.
      expect(labels[1].textContent).toMatch(/^0%$/);
    });
  });

  it("scales the partner axes in proportion to their existing ratio", async () => {
    renderEval();
    await waitFor(() => screen.getByText("Routing weights"));

    const sliders = document.querySelectorAll<HTMLInputElement>(
      "input[type=range]",
    );

    // Drive latency up to 0.4 first: quality (0.6) and cost (0.2) share the
    // remaining 0.6 in proportion to their original weights — q gets 3× cost
    // so q = 0.45, cost = 0.15.
    fireEvent.change(sliders[2], { target: { value: "0.4" } });
    await waitFor(() => {
      const labels = Array.from(
        document.querySelectorAll<HTMLElement>(".weight-slider-value"),
      );
      expect(labels[0].textContent).toMatch(/^45%$/);
      expect(labels[1].textContent).toMatch(/^15%$/);
      expect(labels[2].textContent).toMatch(/^40%$/);
    });
  });
});
