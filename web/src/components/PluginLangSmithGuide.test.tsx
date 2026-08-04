import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "../theme/ThemeProvider";
import { PluginEditorDrawer } from "../pages/EvalPlugins";

// LangSmith's REST API has no "give me back the scores I sent" path
// of its own. The operator owns the wire back: they must create one
// Automation rule in smith.langchain.com that POSTs the score
// payload to /api/eval/plugins/<name>/webhook. Without it traces
// land but scores never return — the most common "why isn't this
// working?" support case for the LangSmith kind.
//
// These tests pin the in-form guide so the operator has the exact
// URL the backend registered (with the chosen plugin name) and the
// exact body contract Nexus accepts, right inside the drawer.

interface StubOptions {
  createdName?: string;
}

function setup(opts: StubOptions = {}) {
  const calls: {
    created: Array<{ name: string; spec_yaml: string }>;
    automation: Array<{ name: string; session_id: string }>;
  } = { created: [], automation: [] };

  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = (init?.method ?? "GET").toUpperCase();
      if (url.endsWith("/api/eval/plugins") && method === "POST") {
        const body = JSON.parse(String(init?.body ?? "{}"));
        calls.created.push(body);
        return new Response(
          JSON.stringify({
            id: "plugin-1",
            name: opts.createdName ?? body.name,
            ...body,
          }),
          { status: 201, headers: { "Content-Type": "application/json" } },
        );
      }
      if (url.includes("/automation") && method === "POST") {
        const body = JSON.parse(String(init?.body ?? "{}"));
        const name = url
          .split("/api/eval/plugins/")[1]
          ?.split("/")[0];
        calls.automation.push({ name, session_id: body.session_id });
        return new Response(
          JSON.stringify({ ok: true, message: "rule mocked" }),
          {
            status: 200,
            headers: { "Content-Type": "application/json" },
          },
        );
      }
      return new Response("{}", { status: 200 });
    }),
  );

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <PluginEditorDrawer
          open
          onClose={() => {}}
          onSaved={() => {}}
        />
        <div data-testid="sentinel" />
      </QueryClientProvider>
    </ThemeProvider>,
  );
  return { ...utils, calls };
}

afterEach(() => vi.unstubAllGlobals());

function selectLangsmith() {
  // Service kind is a select; switch to langsmith so the guide mounts.
  const kind = screen.getByRole("combobox", { name: /Service kind/ }) as HTMLSelectElement;
  fireEvent.change(kind, { target: { value: "langsmith" } });
}

function setPluginName(name: string) {
  const input = screen.getByLabelText(/^Plugin name/) as HTMLInputElement;
  fireEvent.change(input, { target: { value: name } });
}

describe("<PluginEditorDrawer /> LangSmith automation rule", () => {
  it("renders the automation guide once Service kind flips to LangSmith", () => {
    setup();
    expect(screen.queryByTestId("langsmith-automation-guide")).not.toBeInTheDocument();
    selectLangsmith();
    expect(screen.getByTestId("langsmith-automation-guide")).toBeInTheDocument();
  });

  it("shows the score body contract in the drawer so the operator can copy it verbatim", () => {
    setup();
    selectLangsmith();
    const body = screen.getByTestId("automation-rule-body").textContent ?? "";
    // Contract fields the collector expects on POST. Keep these in
    // sync with internal/evalplugin/collector.go.
    expect(body).toMatch(/"name"/);
    expect(body).toMatch(/"trace_id"/);
    expect(body).toMatch(/"score"/);
    expect(body).toMatch(/0 \.\. 1/);
    expect(body).toMatch(/"explanation"/);
  });

  it("interpolates the operator-typed plugin name into the webhook URL", () => {
    setup();
    selectLangsmith();
    setPluginName("smith-judge");
    const url = screen.getByTestId("incoming-webhook-url").textContent ?? "";
    // window.location.origin in jsdom is http://localhost:3000.
    expect(url).toContain("/api/eval/plugins/smith-judge/webhook");
  });

  it("the guide does not appear for unrelated service kinds", () => {
    setup();
    // Langfuse is the default kind — guide must stay out of the way.
    expect(screen.queryByTestId("langsmith-automation-guide")).not.toBeInTheDocument();
    selectLangsmith();
    // Sanity: the guide is in DOM while langsmith is active.
    expect(screen.getByTestId("langsmith-automation-guide")).toBeInTheDocument();
  });

  it("renders the project-id input and Create automation rule button", () => {
    setup();
    selectLangsmith();
    expect(
      screen.getByTestId("langsmith-session-id-input"),
    ).toBeInTheDocument();
    expect(
      screen.getByTestId("langsmith-automation-create"),
    ).toBeInTheDocument();
  });

  it("keeps the button disabled until the operator types a project id", () => {
    setup();
    selectLangsmith();
    const btn = screen.getByTestId(
      "langsmith-automation-create",
    ) as HTMLButtonElement;
    // Empty session id → disabled.
    expect(btn.disabled).toBe(true);
    fireEvent.change(screen.getByTestId("langsmith-session-id-input"), {
      target: {
        value: "11111111-1111-1111-1111-111111111111",
      },
    });
    expect(btn.disabled).toBe(false);
  });

  it("calls /api/eval/plugins/<name>/automation with the typed session id", async () => {
    const env = setup({ createdName: "thegrid-judge" });
    selectLangsmith();
    setPluginName("thegrid-judge");
    fireEvent.change(screen.getByTestId("langsmith-session-id-input"), {
      target: { value: "abcdef00-1111-2222-3333-444444444444" },
    });
    fireEvent.click(screen.getByTestId("langsmith-automation-create"));
    // Wait for the result line to render — the success path
    // sources langsmithAutoResult from the fetch response.
    await screen.findByTestId("langsmith-automation-result");
    expect(env.calls.automation).toHaveLength(1);
    expect(env.calls.automation[0]).toEqual({
      name: "thegrid-judge",
      session_id: "abcdef00-1111-2222-3333-444444444444",
    });
    const resultLine = screen.getByTestId("langsmith-automation-result");
    expect(resultLine.textContent).toContain("rule mocked");
  });

  it("surfaces an AlreadyConfigured response as a warn line", async () => {
    // Override the auto-stub to send ok:false + already_configured:true.
    setup({ createdName: "thegrid-judge" });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (url.includes("/automation") && init?.method === "POST") {
          return new Response(
            JSON.stringify({
              ok: false,
              already_configured: true,
              webhook_url:
                "https://x.example.com/api/eval/plugins/thegrid-judge/webhook",
              message:
                "LangSmith already has a rule with this display name",
            }),
            { status: 200, headers: { "Content-Type": "application/json" } },
          );
        }
        if (url.endsWith("/api/eval/plugins") && init?.method === "POST") {
          return new Response(
            JSON.stringify({
              id: "plugin-1",
              name: "thegrid-judge",
              ...JSON.parse(String(init.body ?? "{}")),
            }),
            { status: 201, headers: { "Content-Type": "application/json" } },
          );
        }
        return new Response("{}", { status: 200 });
      }),
    );
    selectLangsmith();
    setPluginName("thegrid-judge");
    fireEvent.change(screen.getByTestId("langsmith-session-id-input"), {
      target: { value: "abcdef00-1111-2222-3333-444444444444" },
    });
    fireEvent.click(screen.getByTestId("langsmith-automation-create"));
    const resultLine = await screen.findByTestId(
      "langsmith-automation-result",
    );
    expect(resultLine.className).toContain("plugin-row-test");
    expect(resultLine.textContent).toContain("already has a rule");
  });
});
