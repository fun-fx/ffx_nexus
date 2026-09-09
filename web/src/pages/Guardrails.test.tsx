/* @vitest-environment jsdom */
import "@testing-library/jest-dom";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { ThemeProvider } from "../theme/ThemeProvider";
import { vi } from "vitest";
import { Guardrails } from "./Guardrails";

function wrap() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 0, gcTime: 0 } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <MemoryRouter>
          <Guardrails />
        </MemoryRouter>
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

describe("<Guardrails />", () => {
  it("renders when deny_patterns is null from API", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url.endsWith("/api/gateway/config")) {
          return new Response(
            JSON.stringify({
              guardrails: {
                enabled: false,
                block_pii_input: false,
                redact_pii_output: false,
                max_input_chars: 0,
                deny_patterns: null,
                validate_json_output: false,
                self_correction_enabled: false,
                self_correction_max_retries: 1,
              },
              semantic_cache: {
                enabled: false,
                ttl: "24h",
                threshold: 0.92,
                max_entries: 500,
                redis_configured: false,
                embeddings_configured: false,
              },
              alerting: {
                failover_webhook_set: false,
                failover_slack_set: false,
                cooldown: "0s",
              },
              restart_required: null,
            }),
            { status: 200 },
          );
        }
        return new Response("{}", { status: 404 });
      }),
    );
    wrap();
    await waitFor(() => {
      expect(screen.getByRole("button", { name: /save guardrails/i })).toBeInTheDocument();
    });
  });
});
