import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "../theme/ThemeProvider";
import { PluginEditorDrawer } from "../pages/EvalPlugins";

// The preset row used to flip the active highlight but not the form
// beneath it — switching to "Langfuse (self-host)" left the cloud
// endpoint and region select behind. Drawing it out keeps the chip
// and the form on the same page.

interface StubOptions {
  createdName?: string;
}

function setup(opts: StubOptions = {}) {
  const calls: { created: Array<{ name: string; spec_yaml: string }> } = { created: [] };

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

describe("<PluginEditorDrawer /> preset row", () => {
  it("lists all four Langfuse-ish presets as active-toggleable chips", () => {
    setup();
    expect(screen.getByRole("button", { name: /Langfuse \(cloud\)/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Langfuse \(self-host\)/ })).toBeInTheDocument();
  });

  it("starts from the cloud preset — endpoint is cloud.langfuse.com and region select is visible", () => {
    setup();
    const endpoint = screen.getByLabelText(/^Endpoint/) as HTMLInputElement;
    expect(endpoint.value).toBe("https://cloud.langfuse.com");
    expect(screen.getByRole("combobox", { name: /Region \(cloud\)/ })).toBeInTheDocument();
  });

  it("switching to the self-host preset swaps the endpoint and clears the region value", () => {
    setup();
    fireEvent.click(screen.getByRole("button", { name: /Langfuse \(self-host\)/ }));
    const endpoint = screen.getByLabelText(/^Endpoint/) as HTMLInputElement;
    expect(endpoint.value).toBe("https://langfuse.example.internal");
    // Region (<option>) still rendered for kind=langfuse, but the
    // placeholder host is unmapped, so the dropdown reads empty.
    const region = screen.getByRole("combobox", { name: /Region \(cloud\)/ }) as HTMLSelectElement;
    expect(region.value).toBe("");
  });
});
