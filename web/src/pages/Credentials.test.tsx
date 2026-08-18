import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "../theme/ThemeProvider";
import { Credentials } from "./Credentials";

interface PreflightStub {
  ok: boolean;
  status?: number;
  message?: string;
  detected_provider?: string;
  provider_label?: string;
  latency_ms?: number;
}

interface PreflightCall {
  provider: string;
  secret: string;
  base_url?: string;
}

interface MeCalls {
  create: Array<Record<string, unknown>>;
  preflight: PreflightCall[];
  preflightResponse: PreflightStub | null;
}

async function setup(opts: { preflight?: PreflightStub | null } = {}) {
  const calls: MeCalls = {
    create: [],
    preflight: [],
    // Each Test button click re-runs preflight; tdefault to "ok"
    // unless the test overrides it.
    preflightResponse: opts.preflight ?? {
      ok: true,
      status: 200,
      provider_label: "OpenAI",
      latency_ms: 42,
      message: "Connected to OpenAI",
    },
  };

  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = (init?.method ?? "GET").toUpperCase();
      if (url.endsWith("/api/me/credentials") && method === "GET") {
        return jsonRes(200, []);
      }
      if (url.endsWith("/api/me/credentials") && method === "POST") {
        const body = init?.body ? JSON.parse(String(init.body)) : {};
        calls.create.push(body);
        return jsonRes(201, {
          id: "cred-1",
          provider: body.provider,
          name: body.name,
          secret_last4: "...1234",
          enabled: true,
          created_at: new Date().toISOString(),
        });
      }
      if (url.endsWith("/api/me/credentials/preflight") && method === "POST") {
        const body = init?.body ? JSON.parse(String(init.body)) : {};
        const rec: PreflightCall = {
          provider: String(body.provider ?? ""),
          secret: String(body.secret ?? ""),
        };
        if (body.base_url) rec.base_url = String(body.base_url);
        calls.preflight.push(rec);
        const stub =
          calls.preflightResponse ?? {
            ok: false,
            status: 401,
            message: "missing",
          };
        return jsonRes(200, {
          ok: stub.ok,
          provider: rec.provider,
          provider_label: stub.provider_label ?? (rec.provider === "openai" ? "OpenAI" : "Anthropic"),
          status: stub.status,
          latency_ms: stub.latency_ms ?? 42,
          message: stub.message,
          detected_provider: stub.detected_provider ?? "",
        });
      }
      return jsonRes(404, { error: "unknown" });
    }),
  );

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <Credentials />
      </ThemeProvider>
    </QueryClientProvider>,
  );

  await screen.findByText("Provider credentials");
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Add credential/i }));
  });
  return calls;
}

function jsonRes(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("Credentials drawer pre-flight", () => {
  it("disables Save until a successful pre-flight runs", async () => {
    const calls = await setup({ preflight: null });
    const save = screen.getByRole("button", { name: /Save/i }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);

    const secret = screen.getByPlaceholderText(/sk-/i);
    await act(async () => {
      fireEvent.change(secret, { target: { value: "sk-test-1234567890abcdef" } });
    });
    expect(save.disabled).toBe(true);

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /Test connection/i }));
    });
    await waitFor(() => {
      expect(calls.preflight.length).toBeGreaterThan(0);
    });
    // Probe resolved ok=true → Save becomes enabled and Save call goes through.
    await waitFor(() => {
      expect(save.disabled).toBe(false);
    });
    await act(async () => {
      fireEvent.click(save);
    });
    await waitFor(() => {
      expect(calls.create.length).toBe(1);
    });
  });

  it("keeps Save disabled when the pre-flight returns ok=false", async () => {
    const calls = await setup({
      preflight: {
        ok: false,
        status: 401,
        message: "invalid key",
      },
    });
    const secret = screen.getByPlaceholderText(/sk-/i);
    await act(async () => {
      fireEvent.change(secret, { target: { value: "sk-bogus" } });
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /Test connection/i }));
    });
    await waitFor(() => {
      expect(calls.preflight.length).toBe(1);
    });
    const save = screen.getByRole("button", { name: /Save/i }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
  });

  it("shows a switch-provider hint when the secret shape disagrees", async () => {
    const calls = await setup({
      preflight: {
        ok: true,
        status: 200,
        provider_label: "Anthropic",
        latency_ms: 30,
      },
    });
    // Provider dropdown starts at openai; pasting sk-ant-… should
    // surface the hint.
    const secret = screen.getByPlaceholderText(/sk-/i);
    await act(async () => {
      fireEvent.change(secret, { target: { value: "sk-ant-api03-checksum" } });
    });
    // The hint is rendered as a button with the suggestion text.
    const switchBtn = screen.getByRole("button", { name: /switch provider to anthropic/i });
    expect(switchBtn).toBeTruthy();
    await act(async () => {
      fireEvent.click(switchBtn);
    });
    // After the switch, the dropdown should read "anthropic".
    const select = screen.getByRole("combobox") as HTMLSelectElement;
    expect(select.value).toBe("anthropic");
    // Run probe again — provider sent on wire should now be "anthropic".
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /Test connection/i }));
    });
    await waitFor(() => {
      expect(calls.preflight.length).toBeGreaterThanOrEqual(1);
    });
    expect(calls.preflight.some((p) => p.provider === "anthropic")).toBe(true);
  });

  it("exposes 'thegrid' in the provider dropdown as the grid option", async () => {
    await setup();
    const select = screen.getByRole("combobox") as HTMLSelectElement;
    // The label is the marketing-friendly "the grid"; the wire ID is
    // the canonical "grid" so the gateway registry can route to
    // providers.NewGrid.
    const option = within(select).getByRole("option", { name: /the grid/i });
    expect(option).toBeTruthy();
    expect(option.getAttribute("value")).toBe("grid");
  });

  it("auto-fills the canonical Grid base URL when the operator picks thegrid", async () => {
    await setup();
    const select = screen.getByRole("combobox") as HTMLSelectElement;
    await act(async () => {
      fireEvent.change(select, { target: { value: "grid" } });
    });
    // The Base URL field becomes visible and pre-populated with the
    // canonical Grid consumption URL.
    const baseUrl = screen.getByDisplayValue("https://api.thegrid.ai/v1") as HTMLInputElement;
    expect(baseUrl).toBeTruthy();
    expect(baseUrl.tagName).toBe("INPUT");
    // The hint copy is shown so the operator understands what the
    // default is for.
    expect(screen.getByText(/OpenAI-compatible consumption API/i)).toBeTruthy();
  });

  it("sends provider=grid and the canonical base URL on the wire when the operator saves a thegrid credential", async () => {
    const calls = await setup();
    const select = screen.getByRole("combobox") as HTMLSelectElement;
    await act(async () => {
      fireEvent.change(select, { target: { value: "grid" } });
    });
    const secret = screen.getByPlaceholderText(/sk-/i);
    await act(async () => {
      fireEvent.change(secret, { target: { value: "grid-l-very-real" } });
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /Test connection/i }));
    });
    await waitFor(() => {
      expect(calls.preflight.length).toBe(1);
    });
    expect(calls.preflight[0].provider).toBe("grid");
    expect(calls.preflight[0].base_url).toBe("https://api.thegrid.ai/v1");
    const save = screen.getByRole("button", { name: /Save/i }) as HTMLButtonElement;
    await waitFor(() => expect(save.disabled).toBe(false));
    await act(async () => {
      fireEvent.click(save);
    });
    await waitFor(() => {
      expect(calls.create.length).toBe(1);
    });
    expect(calls.create[0].provider).toBe("grid");
    expect(calls.create[0].base_url).toBe("https://api.thegrid.ai/v1");
  });
});
