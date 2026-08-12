import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { PluginKeysModal } from "./PluginKeysModal";

vi.mock("../api", () => ({
  fetchPluginKeys: vi.fn(async (name: string) => ({
    plugin: name,
    configured: false,
    required_key_names: ["public_key", "secret_key"],
    keys: [
      { name: "public_key", set: false },
      { name: "secret_key", set: false },
    ],
  })),
  putPluginKeys: vi.fn(async (_name: string, kv: Record<string, string>) => ({
    plugin: _name,
    configured: Object.values(kv).some(Boolean),
    required_key_names: ["public_key", "secret_key"],
    keys: [
      { name: "public_key", set: !!kv["public_key"] },
      { name: "secret_key", set: !!kv["secret_key"] },
    ],
  })),
  deletePluginKeys: vi.fn(async (name: string) => ({
    plugin: name,
    configured: false,
    required_key_names: ["public_key", "secret_key"],
    keys: [
      { name: "public_key", set: false },
      { name: "secret_key", set: false },
    ],
  })),
}));

function renderModal(open: boolean, name = "langfuse-judge") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <PluginKeysModal pluginName={name} open={open} onClose={() => {}} />
    </QueryClientProvider>,
  );
}

describe("PluginKeysModal", () => {
  beforeEach(() => vi.clearAllMocks());

  it("renders nothing when closed", () => {
    renderModal(false);
    expect(screen.queryByTestId("plugin-keys-backdrop")).toBeNull();
  });

  it("renders required key inputs when open", async () => {
    renderModal(true);
    await waitFor(() => {
      expect(screen.getByTestId("plugin-keys-input-langfuse-judge-public_key")).toBeInTheDocument();
      expect(screen.getByTestId("plugin-keys-input-langfuse-judge-secret_key")).toBeInTheDocument();
    });
  });

  it("submits both keys on save", async () => {
    const { getByTestId } = renderModal(true);
    await waitFor(() => screen.getByTestId("plugin-keys-save-langfuse-judge"));
    fireEvent.change(getByTestId("plugin-keys-input-langfuse-judge-public_key"), {
      target: { value: "pk-lf-1234567890abcd" },
    });
    fireEvent.change(getByTestId("plugin-keys-input-langfuse-judge-secret_key"), {
      target: { value: "sk-lf-1234567890abcd" },
    });
    await waitFor(() => {
      // Wrap the click in waitFor so React's intentional batched
      // setState calls (draft update + onSuccess setState here) all
      // land before our assertion evaluates — prevents the
      // "act() warning: state update not wrapped" diagnostic from
      // running in the test logs.
      fireEvent.click(getByTestId("plugin-keys-save-langfuse-judge"));
    });
    const { putPluginKeys } = await import("../api");
    await waitFor(() =>
      expect(putPluginKeys).toHaveBeenCalledWith("langfuse-judge", {
        public_key: "pk-lf-1234567890abcd",
        secret_key: "sk-lf-1234567890abcd",
      }),
    );
  });

  it("hides the clear button when state is unconfigured", async () => {
    renderModal(true);
    await waitFor(() => screen.getByTestId("plugin-keys-save-langfuse-judge"));
    expect(screen.queryByTestId("plugin-keys-clear-langfuse-judge")).toBeNull();
  });
});
