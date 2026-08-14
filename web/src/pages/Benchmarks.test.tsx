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
  /**
   * Empty both halves of the model catalog. Lets tests assert the
   * free-text input fallback in the picker. The default leaves both
   * halves populated with a single Prime entry and two router
   * aliases, which is enough for the picker-grouping tests to grow
   * against without each test remocking.
   */
  emptyModelCatalog?: boolean;
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
  credential: Array<Record<string, unknown>>;
  validate: Array<Record<string, unknown>>;
  schedulesCreated: Array<Record<string, unknown>>;
  schedulesPaused: string[];
  schedulesResumed: string[];
  schedulesDeleted: string[];
}

function setup(opts: StubOptions = {}) {
  const {
    runs = [completedRun, runningRun],
    configured = true,
    gatewayAvailable = true,
    emptyModelCatalog = false,
  } = opts;
  const calls: Calls = {
    launch: [],
    cancelled: [],
    deleted: [],
    credential: [],
    validate: [],
    schedulesCreated: [],
    schedulesPaused: [],
    schedulesResumed: [],
    schedulesDeleted: [],
  };
  // Sticky mirror of the server's credential state so PUT/GET
  // interactions within the same test agree on what is "stored".
  // Without this, PUT would report configured=true while the
  // re-issued GET kept reporting configured=false, and the panel's
  // collapse logic would never settle.
  const runtime = {
    configured,
    teamId: configured ? "team_stored" : "",
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
          calls.credential.push(JSON.parse(String(init?.body ?? "{}")));
          // Mirror the server's PUT response shape so the panel's
          // collapse logic sees a fresh `configured` flag. Once the
          // PUT has succeeded, flip the in-stub state so the next
          // GET reflects the now-configured cluster — otherwise the
          // credentials query would keep reporting configured=false
          // and the panel never collapses to a saved summary.
          runtime.configured = true;
          const savedTeam = (JSON.parse(String(init?.body ?? "{}"))).team_id ?? "";
          if (savedTeam) runtime.teamId = savedTeam;
          return jsonRes({ ok: true, configured: true, team_id: runtime.teamId });
        }
        return jsonRes({
          provider: "primeintellect",
          configured: runtime.configured,
          // The server only returns a team_id when one is stored. The
          // test mirror keeps that contract; without it the "collapse
          // after save" path would still see team_stored even when
          // the panel was supposed to start unconfigured.
          team_id: runtime.teamId,
        });
      }
      if (url === "/api/eval/benchmarks/models") {
        if (emptyModelCatalog) {
          return jsonRes({ models: [] });
        }
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
      if (url === "/api/me/playground/catalog") {
        if (emptyModelCatalog) {
          return jsonRes({ chat: [], embed: [], user: [] });
        }
        // Default router catalog: a single alias (`code-prime`)
        // sitting under a synthetic user-provider. The console's
        // catalog consumer recognises "user/<provider>/<model>" ids
        // and exposes them grouped separately from Prime base models.
        return jsonRes({
          chat: ["openai/gpt-4.1-mini"],
          embed: [],
          user: [
            {
              provider: "thegrid",
              models: ["text-prime", "code-prime"],
              scope: "org",
              owner_id: "org-fixture",
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
      // Schedules: list, create, and the explicit pause/resume/delete
      // actions wired through the schedule_handlers endpoints. The
      // stub answers with the row the operator just typed in so the
      // UI can render the in-place list update without a fetch round.
      if (url === "/api/eval/benchmarks/schedules" && method === "GET") {
        return jsonRes({ schedules: [] });
      }
      const scheduleCreate = url === "/api/eval/benchmarks/schedules" && method === "POST";
      if (scheduleCreate) {
        calls.schedulesCreated.push(JSON.parse(String(init?.body ?? "{}")));
        const body = JSON.parse(String(init?.body ?? "{}"));
        return jsonRes({
          id: "schd-1",
          org_id: "o1",
          name: body.name ?? "",
          environments: body.environments ?? [],
          model: body.model ?? "",
          num_examples: body.num_examples ?? 0,
          rollouts: body.rollouts ?? 0,
          via_gateway: body.via_gateway ?? false,
          cadence_seconds: body.cadence_seconds ?? 0,
          next_launch_at: new Date(Date.now() + (body.cadence_seconds ?? 0) * 1000).toISOString(),
          enabled: body.enabled ?? true,
          created_at: new Date().toISOString(),
          updated_at: new Date().toISOString(),
        }, 201);
      }
      const schedulePause = url.match(/^\/api\/eval\/benchmarks\/schedules\/([^/]+)\/pause$/);
      if (schedulePause && method === "POST") {
        calls.schedulesPaused.push(schedulePause[1]);
        return jsonRes({ ok: true, id: schedulePause[1], enabled: false, next_launch_at: new Date().toISOString() });
      }
      const scheduleResume = url.match(/^\/api\/eval\/benchmarks\/schedules\/([^/]+)\/resume$/);
      if (scheduleResume && method === "POST") {
        calls.schedulesResumed.push(scheduleResume[1]);
        return jsonRes({ ok: true, id: scheduleResume[1], enabled: true, next_launch_at: new Date().toISOString() });
      }
      const scheduleDel = url.match(/^\/api\/eval\/benchmarks\/schedules\/([^/]+)$/);
      if (scheduleDel && method === "DELETE") {
        calls.schedulesDeleted.push(scheduleDel[1]);
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

// Cleaning up after every setup() call keeps two renders in the
// same Vitest file from clobbering each other: testing-library
// appends to a single jsdom DOM by default, so an unmounted prior
// render would still own the screen. Without this, the second
// `setup()` only stubs a new fetch and re-renders, leaving the
// previous component's state (and DOM nodes) on screen.
afterEach(() => {
  // The setup helper exposes its render-result via utils; a
  // "global" cleanup that walks the document is the safest
  // backstop in case a test forgoes the helper.
  document.body.innerHTML = "";
});

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

    // "Need a custom dataset?" callout should be visible the moment
    // the New run panel mounts — so operators using their own
    // organisation's templates notice the custom path before they
    // hunt for it. The callout's key tokens (`prime env push`,
    // `your-org/<dataset-slug>`, `Add`) map 1:1 to the UI so a
    // future rewording intentionally breaks the test and forces a
    // code-review comment.
    const callout = screen.getByText(/Need a custom dataset\?/);
    expect(callout).toBeVisible();
    expect(callout.parentElement).toHaveTextContent("prime env push your-org/");
    expect(callout.parentElement).toHaveTextContent("your-org/<slug>");

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

describe("<Benchmarks /> model picker", () => {
  it("groups Prime base models and router aliases under separate optgroups", async () => {
    // The launcher pulls two catalogs and merges them client-side:
    //   - /api/eval/benchmarks/models  (Prime's hosted-evaluations
    //     catalogue — priced entries the verifier can call directly)
    //   - /api/me/playground/catalog   (Nexus's gateway router —
    //     user-prefixed aliases like `code-prime` that round-trip
    //     through the gateway's routing layer).
    //
    // Both halves must appear in the picker, and the picker must
    // group them so an operator scanning the dropdown can tell at a
    // glance which is the underlying provider model and which is a
    // local alias.
    setup({ runs: [] });
    // Wait for both groups to mount — the Prime group has the
    // base-model pricing, the router group has the user-prefixed ids.
    await screen.findByRole("option", { name: /openai\/gpt-4\.1-mini/ });
    await screen.findByRole("option", { name: /code-prime/ });

    const primeGroup = screen.getByRole("group", { name: /Prime base models/ });
    expect(primeGroup).toBeInTheDocument();
    expect(primeGroup).toHaveTextContent("openai/gpt-4.1-mini");

    const routerGroup = screen.getByRole("group", {
      name: /Router aliases/,
    });
    expect(routerGroup).toBeInTheDocument();
    // Aliases are exposed with their `user/<provider>/<model>` schema
    // so an operator can paste them straight into other launch flows.
    expect(routerGroup).toHaveTextContent("user/thegrid/code-prime");
    expect(routerGroup).toHaveTextContent("user/thegrid/text-prime");
  });

  it("falls back to a free-text model input when both catalogs are empty", async () => {
    // A misconfigured cluster may have neither catalog available —
    // the picker should not leave the operator staring at a disabled
    // select. The free-text fallback preserves the smoke-test path
    // because the operator can still paste a Prime model id (or a
    // router alias id) by hand and the same launch wire shape
    // accepts both.
    setup({ runs: [], emptyModelCatalog: true });
    await screen.findByRole("button", { name: "Launch run" });
    const modelInput = screen.getByPlaceholderText("openai/gpt-4.1-mini");
    expect(modelInput).toBeInTheDocument();
    // The placeholder is the only guidance the free-text input has,
    // so this test pins it: a future rewrite that drops the hint
    // would silently degrade the smoke-testability of an unconfigured
    // cluster.
    expect(modelInput.tagName).toBe("INPUT");
    // The model dropdown must NOT exist in this branch — it would
    // be a UI inconsistency to ship both a select and a free-text
    // input for the same field.
    expect(screen.queryByRole("combobox", { name: "" })).toBeNull();
  });
});

describe("<Benchmarks /> credential", () => {
  it("collapses the panel once a key is stored and exposes Change/Remove in place of the form", async () => {
    // Once the API key + team id have been saved, the form should
    // collapse to a one-line summary — never showing the password
    // input or the saved team id in a plain input box. Change/Remove
    // are the only valid follow-up actions from the collapsed view;
    // opening the form again is the explicit way to rotate.
    const { calls } = setup();
    const collapsed = await screen.findByTestId("bench-credential-collapsed");
    expect(collapsed).toHaveTextContent("API key stored");
    expect(collapsed).toHaveTextContent("billing team team_stored");
    // The password input and the saved team input must NOT be in
    // the DOM while collapsed — a spectator who glances at the
    // screen should never see the stored values again.
    expect(screen.queryByPlaceholderText("Replace API key (optional)")).toBeNull();
    expect(screen.queryByDisplayValue("team_stored")).toBeNull();
    expect(screen.getByRole("button", { name: "Change" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Remove" })).toBeInTheDocument();

    // Clicking Change re-opens the form and surfaces the saved team
    // id so the operator can tweak it without re-typing from memory.
    fireEvent.click(screen.getByRole("button", { name: "Change" }));
    const field = await screen.findByPlaceholderText("Replace API key (optional)");
    expect(field).toHaveAttribute("type", "password");
    expect(screen.getByDisplayValue("team_stored")).toBeInTheDocument();

    fireEvent.change(field, { target: { value: "pit_abc" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(calls.credential).toEqual([{ api_key: "pit_abc", team_id: "team_stored" }]),
    );
  });

  it("keeps the form expanded when no credential is stored yet", async () => {
    // The opposite of the collapse test: an unconfigured cluster
    // never gets a "Change" button (there is nothing to change) and
    // instead shows the empty-state form so the operator can paste
    // a key without an extra click.
    setup({ configured: false });
    expect(await screen.findByText("not set")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("pit_…")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
    expect(screen.queryByRole("button", { name: "Change" })).toBeNull();
  });

  it("collapses the panel automatically after a successful save", async () => {
    // Even from the unconfigured state, saving a key should drive
    // the panel back to the compact summary without requiring a
    // reload — the in-flight cache invalidation in the parent feeds
    // a fresh storedTeamId that the panel reacts to.
    const { calls } = setup({ configured: false });
    await screen.findByText("not set");
    const field = screen.getByPlaceholderText("pit_…");
    fireEvent.change(field, { target: { value: "pit_new" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(calls.credential).toEqual([{ api_key: "pit_new", team_id: "" }]),
    );
    const collapsed = await screen.findByTestId("bench-credential-collapsed");
    // The summary calls out where the API key lives and whether a
    // billing team is attached. After the first save without a team
    // id, "personal wallet" must appear so the operator can see at
    // a glance whether hosted runs will bill their team or their
    // personal Prime account — both are valid, but they are not
    // identical.
    expect(collapsed).toHaveTextContent("API key stored");
    expect(collapsed).toHaveTextContent("personal wallet");
  });

  it("lets the operator discard a half-typed rotation via Cancel", async () => {
    // A rotation flow: open with Change, type a new key, decide not
    // to commit, hit Cancel. The panel must collapse back without
    // sending the partial value to the server. This guards against
    // a regression where Cancel would still POST.
    setup();
    fireEvent.click(await screen.findByRole("button", { name: "Change" }));
    fireEvent.change(await screen.findByPlaceholderText("Replace API key (optional)"), {
      target: { value: "pit_partial" },
    });
    // Scope to the credential row's Cancel button via a testid —
    // there are other "Cancel" surfaces around the page (drawer
    // footers, role navigation, etc.) and we want to assert this
    // exact button collapses the panel without sending.
    fireEvent.click(screen.getByTestId("bench-credential-cancel"));
    const collapsed = await screen.findByTestId("bench-credential-collapsed");
    expect(collapsed).toHaveTextContent("billing team team_stored");
    // The password input and the saved team input must disappear —
    // otherwise a half-typed rotation would still be visible to the
    // next person who glances at the screen, defeating the point
    // of pressing Cancel.
    expect(screen.queryByPlaceholderText("Replace API key (optional)")).toBeNull();
  });
});

describe("<Benchmarks /> recipient hint", () => {
  it("surfaces the recipient-model note only when gateway routing is available", async () => {
    // The hint explains "with via gateway, the Model field is the
    // recipient id", and that explanation only makes sense when
    // gateway routing is part of the form. Showing it on an
    // unconfigured cluster would be page-furniture for nothing.
    setup({ runs: [] });
    expect(await screen.findByTestId("bench-recipient-hint")).toBeInTheDocument();
  });

  it("hides the recipient-model note when gateway routing is unavailable", async () => {
    setup({ runs: [], gatewayAvailable: false });
    await screen.findByRole("button", { name: "Launch run" });
    expect(screen.queryByTestId("bench-recipient-hint")).not.toBeInTheDocument();
  });
});

describe("<Benchmarks /> schedules panel", () => {
  it("renders the Schedules section header and an empty-state hint", async () => {
    setup({ runs: [] });
    const header = await screen.findByRole("heading", { name: "Schedules", level: 2 });
    expect(header).toBeInTheDocument();
    expect(screen.getByTestId("bench-schedule-open-drawer")).toBeInTheDocument();
  });

  it("renders an existing schedule row with cadence description and status chip", async () => {
    vi.unstubAllGlobals();
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/api/eval/benchmarks") {
        return jsonRes({ runs: [], gateway_routing_available: true, max_total_samples: 500 });
      }
      if (url === "/api/eval/benchmarks/schedules") {
        return jsonRes({
          schedules: [
            {
              id: "schd-1",
              org_id: "o1",
              name: "nightly gsm8k",
              environments: ["primeintellect/gsm8k"],
              model: "openai/gpt-4o-mini",
              num_examples: 5,
              rollouts: 1,
              via_gateway: false,
              cadence_seconds: 86_400,
              next_launch_at: new Date(Date.now() + 86_400 * 1000).toISOString(),
              enabled: true,
              created_at: new Date().toISOString(),
              updated_at: new Date().toISOString(),
            },
          ],
        });
      }
      if (url === "/api/eval/benchmarks/push-report") return jsonRes({ reports: [] });
      if (url === "/api/eval/benchmarks/credential") {
        return jsonRes({ provider: "primeintellect", configured: true, team_id: "team_stored" });
      }
      if (url === "/api/eval/benchmarks/models") return jsonRes({ models: [] });
      return jsonRes({});
    });
    vi.stubGlobal("fetch", fetchMock);

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <ThemeProvider>
        <QueryClientProvider client={qc}>
          <Benchmarks />
        </QueryClientProvider>
      </ThemeProvider>,
    );

    const list = await screen.findByTestId("bench-schedule-list");
    expect(list).toHaveTextContent("nightly gsm8k");
    expect(list).toHaveTextContent("every 1d");
    expect(list).toHaveTextContent("armed");
  });
});
