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
  /** Force the validate POST to fail with this message. */
  validateError?: string;
  /** Reports the push-report GET should answer with. */
  pushReports?: Array<{
    slug: string;
    ok: boolean;
    completed_at: string;
    received_at: string;
  }>;
}

interface Calls {
  launch: Array<Record<string, unknown>>;
  cancelled: string[];
  deleted: string[];
  credential: string[];
  validate: Array<Record<string, unknown>>;
}

function setup(opts: StubOptions = {}) {
  const {
    runs = [completedRun, runningRun],
    configured = true,
    gatewayAvailable = true,
  } = opts;
  const calls: Calls = {
    launch: [],
    cancelled: [],
    deleted: [],
    credential: [],
    validate: [],
  };

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
      if (url === "/api/eval/benchmarks/push-report" && method === "GET") {
        return jsonRes({ reports: opts.pushReports ?? [] });
      }
      if (url === "/api/eval/benchmarks/validate" && method === "POST") {
        calls.validate.push(JSON.parse(String(init?.body ?? "{}")));
        if (opts.validateError) {
          return jsonRes({ ok: false, error: opts.validateError }, 400);
        }
        return jsonRes({ ok: true });
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

  const renderModelCmb = async () => {
  // The form has two <select>s: the env-preset picker has an empty
  // "Add a built-in environment…" placeholder option, the model
  // picker is wrapped in a <label> whose text is "Model". They both
  // expose role=combobox, so use the wrapping label text to
  // disambiguate. Using name alone is too fragile — when models
  // are loading the select is replaced by a free-text field and
  // back, so we wait for the priced options to be present.
  await screen.findByRole("option", { name: /gpt-4\.1-mini/ });
  const cbs = screen.getAllByRole("combobox");
  return cbs[cbs.length - 1];
};

// The custom-slug form's submit button does not submit through a
// plain click in JSDOM (the surrounding <label> interferes with
// the implicit form association), so submit the form element
// directly. This mirrors what hitting Enter inside the input does.
function addCustomSlug(value: string) {
  const inp = screen.getByLabelText("Add a custom environment slug") as HTMLInputElement;
  fireEvent.change(inp, { target: { value } });
  fireEvent.submit(inp.closest("form")!);
}

it("posts the form, combining preset choices with a custom slug", async () => {
    const { calls } = setup({ runs: [] });
    await screen.findByRole("button", { name: "Launch run" });

    // The default preset chip should already be in the list and ready
    // to launch against, removing the need to type a slug for the
    // smoke-test path.
    expect(screen.getByTestId("bench-env-chips")).toHaveTextContent(
      "primeintellect/gsm8k",
    );

    // Combo: take a second preset via the picker...
    const presetPicker = screen.getByLabelText("Add a built-in environment");
    fireEvent.change(presetPicker, {
      target: { value: "primeintellect/mmlu-pro" },
    });
    // ...then add a custom slug through the free-text field.
    addCustomSlug("your-org/alphabet-sort");

    fireEvent.change(await renderModelCmb(), {
      target: { value: "openai/gpt-4.1-mini" },
    });
    fireEvent.change(screen.getByDisplayValue("5"), { target: { value: "10" } });

    fireEvent.click(screen.getByRole("button", { name: "Launch run" }));
    await waitFor(() => expect(calls.launch).toHaveLength(1));
    expect(calls.launch[0]).toMatchObject({
      environments: [
        "primeintellect/gsm8k",
        "primeintellect/mmlu-pro",
        "your-org/alphabet-sort",
      ],
      model: "openai/gpt-4.1-mini",
      num_examples: 10,
      rollouts: 1,
      via_gateway: false,
    });
  });

  it("blocks a run over the sample cap and says so", async () => {
    setup({ runs: [] });
    await screen.findByRole("button", { name: "Launch run" });
    // Remove the default preset to start from a clean slate.
    fireEvent.click(screen.getByRole("button", { name: "Remove primeintellect/gsm8k" }));
    addCustomSlug("your-org/gsm8k");
    fireEvent.change(await renderModelCmb(), {
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
    fireEvent.click(screen.getByRole("button", { name: "Remove primeintellect/gsm8k" }));
    addCustomSlug("your-org/nope");
    fireEvent.change(await renderModelCmb(), {
      target: { value: "openai/gpt-4.1-mini" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Launch run" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("not found (404)");
  });

  it("validates environments before launch with a cheap dry-run", async () => {
    const { calls } = setup({ runs: [] });
    await screen.findByRole("button", { name: "Validate environments" });
    fireEvent.click(screen.getByRole("button", { name: "Remove primeintellect/gsm8k" }));
    addCustomSlug("your-org/gsm8k");
    fireEvent.change(await renderModelCmb(), {
      target: { value: "openai/gpt-4.1-mini" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Validate environments" }));
    await waitFor(() => expect(calls.validate).toHaveLength(1));
    expect(calls.validate[0]).toMatchObject({
      environments: ["your-org/gsm8k"],
      model: "openai/gpt-4.1-mini",
    });
    // On success the UI surfaces an explicit "ok" hint rather than
    // just disappearing.
    expect(
      await screen.findByTestId("bench-validate-ok"),
    ).toHaveTextContent("Safe to launch");
  });

  it("surfaces a failed dry-run so operators fix environment slugs before launch", async () => {
    setup({ runs: [], validateError: "benchmark: not found (404): environment primeintellect/gsm8k " });
    await screen.findByRole("button", { name: "Validate environments" });
    fireEvent.change(await renderModelCmb(), {
      target: { value: "openai/gpt-4.1-mini" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Validate environments" }));
    expect(await screen.findByTestId("bench-validate-err")).toHaveTextContent(
      "not found (404)",
    );
  });

  it("offers the env-push guide on a 404 but not on other failures", async () => {
    // A 404 is the one dry-run failure the guide answers: the slug is
    // not published. A 401 or a balance error needs a different fix, so
    // showing five CLI steps there would send the operator the wrong way.
    setup({ runs: [], validateError: "benchmark: unauthorized (401): bad key" });
    await screen.findByRole("button", { name: "Validate environments" });
    fireEvent.change(await renderModelCmb(), {
      target: { value: "openai/gpt-4.1-mini" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Validate environments" }));
    await screen.findByTestId("bench-validate-err");
    expect(screen.queryByTestId("bench-cli-guide")).not.toBeInTheDocument();
  });

  it("puts the reporting curl in the guide once a 404 exposes it", async () => {
    setup({ runs: [], validateError: "benchmark: not found (404): environment" });
    await screen.findByRole("button", { name: "Validate environments" });
    fireEvent.change(await renderModelCmb(), {
      target: { value: "openai/gpt-4.1-mini" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Validate environments" }));

    const guide = await screen.findByTestId("bench-cli-guide");
    // The commands are split on the slug the operator actually selected:
    // the bare name scaffolds, the owner is what we push to. Getting
    // either half from the placeholder instead would publish the wrong
    // thing.
    expect(guide).toHaveTextContent("prime env init gsm8k -p .");
    expect(guide).toHaveTextContent("prime env push --visibility PRIVATE");
    expect(guide).toHaveTextContent("YOUR-TEAM-SLUG");
    // Both halves of the CLI's path handling are traps that cost real
    // debugging time, so the guide has to keep warning about them:
    // a slug passed to init crashes, and a slug passed to push sends it
    // hunting one directory too deep.
    expect(guide).toHaveTextContent("FileNotFoundError");
    expect(guide).toHaveTextContent("pyproject.toml not found");
    expect(guide).toHaveTextContent("/api/eval/benchmarks/push-report");
    // Output is deliberately absent from the payload: the CLI can echo
    // the API key and this panel is rendered for admins.
    expect(guide).not.toHaveTextContent('"stdout"');
  });

  it("leads the guide with the owner prerequisite", async () => {
    // A fresh Prime account has neither a username nor a team slug, and
    // the push only complains about it at the very end — after the wheel
    // builds and the upload reports success, which reads like it worked.
    // Anyone who misses this loses the time to a build they have to
    // redo, so it goes above the steps rather than inside them.
    setup({ runs: [], validateError: "benchmark: not found (404): environment" });
    await screen.findByRole("button", { name: "Validate environments" });
    fireEvent.change(await renderModelCmb(), {
      target: { value: "openai/gpt-4.1-mini" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Validate environments" }));

    const guide = await screen.findByTestId("bench-cli-guide");
    expect(guide).toHaveTextContent("must already be a username or team slug");
    expect(guide).toHaveTextContent("missing a teamname");
    // And it has to say where: the field is only settable in Prime's own
    // dashboard, so a guide without the link is a dead end.
    expect(
      guide.querySelector('a[href*="app.primeintellect.ai"]'),
    ).toBeInTheDocument();
  });

  it("shows a reported push as reported rather than verified", async () => {
    // The distinction matters: a report is the operator's word, and only
    // Validate asks the vendor. Presenting it as proof would let someone
    // conclude a slug is live when it never was.
    setup({
      runs: [],
      validateError: "benchmark: not found (404): environment",
      pushReports: [
        {
          slug: "primeintellect/gsm8k",
          ok: true,
          completed_at: "2026-08-05T04:00:00Z",
          received_at: "2026-08-05T04:00:05Z",
        },
      ],
    });
    await screen.findByRole("button", { name: "Validate environments" });
    fireEvent.change(await renderModelCmb(), {
      target: { value: "openai/gpt-4.1-mini" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Validate environments" }));

    const banner = await screen.findByTestId("bench-push-reported");
    expect(banner).toHaveTextContent("was reported at");
    expect(banner).toHaveTextContent("not verified here");
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
