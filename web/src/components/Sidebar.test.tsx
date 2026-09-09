import { describe, expect, it } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import axe from "axe-core";
import { ThemeProvider } from "../theme/ThemeProvider";
import { Sidebar } from "./Sidebar";
import { AppShell } from "./AppShell";

function WithProviders({ children, route = "/observability/traces" }: { children: React.ReactNode; route?: string }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return (
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={[route]}>{children}</MemoryRouter>
      </QueryClientProvider>
    </ThemeProvider>
  );
}

describe("Sidebar", () => {
  it("brand links to Overview", async () => {
    render(
      <WithProviders route="/gateway/routing">
        <Sidebar />
      </WithProviders>,
    );
    const home = await screen.findByRole("link", { name: /nexus home — overview/i });
    expect(home).toHaveAttribute("href", "/");
  });

  it("lists workspace links", async () => {
    render(
      <WithProviders>
        <Sidebar />
      </WithProviders>,
    );
    await waitFor(() => {
      expect(screen.getByRole("link", { name: /traces/i })).toBeInTheDocument();
    });
  });

  it("hides admin links when role is not admin", async () => {
    render(
      <WithProviders>
        <Sidebar />
      </WithProviders>,
    );
    await waitFor(() => {
      expect(screen.queryByRole("link", { name: /users/i })).toBeNull();
      expect(screen.queryByRole("button", { name: /^eval$/i })).toBeNull();
    });
  });

  it("no axe violations on default sidebar", async () => {
    const { container } = render(
      <WithProviders>
        <Sidebar />
      </WithProviders>,
    );
    await waitFor(() => screen.getByRole("link", { name: /traces/i }));
    const violations = await axe.run(container, {
      rules: { "color-contrast": { enabled: false } },
    });
    expect(violations.violations).toEqual([]);
  });
});

describe("AppShell", () => {
  it("renders sidebar + topbar + outlet area", async () => {
    render(
      <WithProviders route="/observability/traces">
        <AppShell />
      </WithProviders>,
    );
    await waitFor(() => {
      expect(screen.getByRole("link", { name: /traces/i })).toBeInTheDocument();
    });
    expect(screen.getByRole("banner")).toBeInTheDocument(); // topbar header
  });
});
