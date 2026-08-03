import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "../theme/ThemeProvider";
import { Benchmarks } from "./Benchmarks";
import type { BenchmarkRun } from "../api";

const completedRun: BenchmarkRun = {
  id: "run-1",
  org_id: "o1",
  provider: "primeintellect",
  external_id: "ev-1",
  name: "nightly gsm8k",
  environments: ["acme/gsm8k"],
  model: "openai/gpt-4.1-mini",
  num_examples: 5,
  rollouts: 1,
  via_gateway: true,
  status: "completed",
  external_status: "COMPLETED",
  avg_score: 0.8125,
  min_score: 0,
  max_score: 1,
  total_samples: 5,
  viewer_url: "https://app.primeintellect.ai/eval/ev-1",
  created_at: "2026-08-01T00:00:00Z",
  updated_at: "2026-08-01T00:10:00Z",
  started_at: "2026-08-01T00:01:00Z",
  completed_at: "2026-08-01T00:10:00Z",
};

const runningRun: BenchmarkRun = {
  ...completedRun,
  id: "run-2",
  name: "in flight",
  status: "running",
  external_status: "PROCESSING",
  avg_score: null,
  min_score: null,
  max_score: null,
  total_samples: null,
  viewer_url: "",
  completed_at: undefined,
};

interface StubOptions {
  runs?: BenchmarkRun[];
  configured?: boolean;
  gatewayAvailable?: boolean;
  /** Force the launch POST to fail with this message. */
  launchError?: string;
}

interface Calls {
  launch: Array<Record<string, unknown>>;
  cancelled: string[];
  deleted: string[];
  credential: string[];
}

function setup(opts: StubOptions = {}) {
  const {
    runs = [completedRun, runningRun],
    configured = true,
    gatewayAvailable = true,
  } = opts;
  const calls: Calls = { launch: [], cancelled: [], deleted: [], credential: [] };

  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = (init?.method ?? "GET").toUpperCase();

      if (url === "/api/eval/benchmarks" && method === "GET") {
        return jsonRes({
          runs,
          gateway_routing_available: gatewayAvailable,
          max_total_samples: 500,
        });
      }
      if (url === "/api/eval/benchmarks" && method === "POST") {
        calls.launch.push(JSON.parse(String(init?.body ?? "{}")));
        if (opts.launchError) {
          return jsonRes({ error: opts.launchError }, 400);
        }
        return jsonRes(completedRun, 201);
      }
      if (url === "/api/eval/benchmarks/credential") {
        if (method === "PUT") {
          calls.credential.push(JSON.parse(String(init?.body ?? "{}")).api_key);
          return jsonRes({ ok: true, configured: true });
        }
        return jsonRes({ provider: "primeintellect", configured });
      }
      if (url === "/api/eval/benchmarks/models") {
        return jsonRes({
          models: [
            {
              id: "openai/gpt-4.1-mini",
              name: "GPT-4.1 mini",
              provider: "openai",
              pricing: { prompt: 0.4, completion: 1.6 },
            },
          ],
        });
      }
      const cancel = url.match(/^\/api\/eval\/benchmarks\/(.+)\/cancel$/);
      if (cancel && method === "POST") {
        calls.cancelled.push(cancel[1]);
        return jsonRes({ ok: true });
      }
      const del = url.match(/^\/api\/eval\/benchmarks\/([^/]+)$/);
      if (del && method === "DELETE") {
        calls.deleted.push(del[1]);
        return jsonRes({ ok: true });
      }
      return jsonRes({});
    }),
  );

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <Benchmarks />
      </QueryClientProvider>
    </ThemeProvider>,
  );
  return { ...utils, calls };
}

function jsonRes(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => vi.unstubAllGlobals());

describe("<Benchmarks /> run list", () => {
  it("shows each run with its status and average score", async () => {
    setup();
    expect(await screen.findByText("nightly gsm8k")).toBeInTheDocument();
    expect(screen.getByText("0.813")).toBeInTheDocument();
    expect(screen.getByText("completed")).toBeInTheDocument();
    // An unsettled run has no score yet, and must not read as zero.
    expect(screen.getByText("in flight")).toBeInTheDocument();
    expect(screen.getByText("—")).toBeInTheDocument();
  });

  it("offers Cancel only while a run is unsettled", async () => {
    const { calls } = setup();
    await screen.findByText("in flight");
    const cancels = screen.getAllByRole("button", { name: "Cancel" });
    expect(cancels).toHaveLength(1);
    fireEvent.click(cancels[0]);
    await waitFor(() => expect(calls.cancelled).toEqual(["run-2"]));
  });

  it("warns before deleting a run that is still going", async () => {
    const confirm = vi.fn(() => true);
    vi.stubGlobal("confirm", confirm);
    const { calls } = setup();
    await screen.findByText("in flight");
    fireEvent.click(screen.getAllByRole("button", { name: "Delete" })[1]);
    await waitFor(() => expect(calls.deleted).toEqual(["run-2"]));
    expect(String(confirm.mock.calls[0])).toContain("cancel it first");
  });

  it("labels which serving path a run measured", async () => {
    setup();
    await screen.findByText("nightly gsm8k");
    expect(screen.getAllByText("this gateway")).toHaveLength(2);
  });
});

describe("<Benchmarks /> launching", () => {
  it("refuses to launch until a credential, an environment and a model are present", async () => {
    setup({ configured: false, runs: [] });
    const launch = await screen.findByRole("button", { name: "Launch run" });
    expect(launch).toBeDisabled();
    expect(
      screen.getByText(/Paste a PrimeIntellect API key above/),
    ).toBeInTheDocument();
  });

  it("posts the form, splitting environments on newlines and commas", async () => {
    const { calls } = setup({ runs: [] });
    await screen.findByRole("button", { name: "Launch run" });

    fireEvent.change(
      screen.getByPlaceholderText(/your-org\/gsm8k/),
      { target: { value: "acme/gsm8k, acme/sort\nacme/math" } },
    );
    // The picker replaces the free-text field once the catalogue loads.
    const model = await screen.findByRole("combobox");
    fireEvent.change(model, { target: { value: "openai/gpt-4.1-mini" } });
    fireEvent.change(screen.getByDisplayValue("5"), { target: { value: "10" } });

    fireEvent.click(screen.getByRole("button", { name: "Launch run" }));
    await waitFor(() => expect(calls.launch).toHaveLength(1));
    expect(calls.launch[0]).toMatchObject({
      environments: ["acme/gsm8k", "acme/sort", "acme/math"],
      model: "openai/gpt-4.1-mini",
      num_examples: 10,
      rollouts: 1,
      via_gateway: false,
    });
  });

  it("blocks a run over the sample cap and says so", async () => {
    setup({ runs: [] });
    await screen.findByRole("button", { name: "Launch run" });
    fireEvent.change(screen.getByPlaceholderText(/your-org\/gsm8k/), {
      target: { value: "acme/gsm8k" },
    });
    fireEvent.change(await screen.findByRole("combobox"), {
      target: { value: "openai/gpt-4.1-mini" },
    });
    fireEvent.change(screen.getByDisplayValue("5"), { target: { value: "1000" } });

    expect(screen.getByText(/over the 500 cap/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Launch run" })).toBeDisabled();
  });

  it("explains why gateway routing is unavailable instead of failing on submit", async () => {
    setup({ runs: [], gatewayAvailable: false });
    await screen.findByRole("button", { name: "Launch run" });
    const box = screen.getByRole("checkbox");
    expect(box).toBeDisabled();
    expect(screen.getByText(/NEXUS_PUBLIC_GATEWAY_URL is not set/)).toBeInTheDocument();
  });

  it("surfaces a launch the provider refused", async () => {
    setup({ runs: [], launchError: "benchmark: not found (404): environment" });
    await screen.findByRole("button", { name: "Launch run" });
    fireEvent.change(screen.getByPlaceholderText(/your-org\/gsm8k/), {
      target: { value: "acme/nope" },
    });
    fireEvent.change(await screen.findByRole("combobox"), {
      target: { value: "openai/gpt-4.1-mini" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Launch run" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("not found (404)");
  });
});

describe("<Benchmarks /> credential", () => {
  it("reports a stored key without revealing it and can replace it", async () => {
    const { calls } = setup();
    expect(await screen.findByText("configured")).toBeInTheDocument();
    const field = screen.getByPlaceholderText("pit_…");
    expect(field).toHaveAttribute("type", "password");

    fireEvent.change(field, { target: { value: "pit_abc" } });
    fireEvent.click(screen.getByRole("button", { name: "Replace" }));
    await waitFor(() => expect(calls.credential).toEqual(["pit_abc"]));
  });

  it("prompts to add a key when none is stored", async () => {
    setup({ configured: false });
    expect(await screen.findByText("not set")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
  });
});
