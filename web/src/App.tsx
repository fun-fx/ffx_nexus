import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter, Navigate, Route, Routes, useParams } from "react-router-dom";
import { ThemeProvider } from "./theme/ThemeProvider";
import { AppShell } from "./components/AppShell";
import { RequireAuth } from "./components/RequireAuth";
import { Overview } from "./pages/Overview";
import { Login } from "./pages/Login";
import { Traces } from "./pages/Traces";
import { TracesDashboard } from "./pages/TracesDashboard";
import { Routing } from "./pages/Routing";
import { RoutingDetail } from "./pages/RoutingDetail";
import { Keys } from "./pages/Keys";
import { Users } from "./pages/Users";
import { Credentials } from "./pages/Credentials";
import { EvalOverview, EvalRoutingIntegration } from "./pages/Eval";
import { EvalProfilesPage } from "./pages/EvalProfilesPage";
import { EvalPlugins } from "./pages/EvalPlugins";
import { Benchmarks } from "./pages/Benchmarks";
import { Audit } from "./pages/Audit";
import { Playground } from "./pages/Playground";
import { Spend } from "./pages/Spend";
import { Docs } from "./pages/Docs";
import { Observability } from "./pages/Observability";
import { MCPRegistry } from "./pages/MCPRegistry";
import { McpLibrary } from "./pages/McpLibrary";
import { McpSettings } from "./pages/McpSettings";
import { Guardrails } from "./pages/Guardrails";
import { SemanticCache } from "./pages/SemanticCache";
import { Alerting } from "./pages/Alerting";
import { MCPLogs } from "./pages/MCPLogs";
import { ObservabilityLayout } from "./layouts/ObservabilityLayout";
import { GatewayLayout } from "./layouts/GatewayLayout";
import { McpLayout } from "./layouts/McpLayout";
import { EvalLayout } from "./layouts/EvalLayout";
import { GovernanceLayout } from "./layouts/GovernanceLayout";
import { DevelopLayout } from "./layouts/DevelopLayout";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 5_000,
      gcTime: 5 * 60_000,
      retry: 1,
      refetchOnWindowFocus: true,
      refetchOnMount: "always",
    },
  },
});

function LegacyRoutingDetailRedirect() {
  const { alias } = useParams();
  return <Navigate to={`/gateway/routing/${alias ?? ""}`} replace />;
}

function LegacyDocsRedirect() {
  const params = useParams();
  const slug = (params["*"] as string | undefined) ?? "";
  return <Navigate to={slug ? `/develop/docs/${slug}` : "/develop/docs"} replace />;
}

export function App() {
  return (
    <ThemeProvider>
      <QueryClientProvider client={queryClient}>
        <BrowserRouter>
          <Routes>
            <Route path="/login" element={<Login />} />
            <Route element={<RequireAuth />}>
              <Route element={<AppShell />}>
                <Route index element={<Overview />} />

                <Route path="observability" element={<ObservabilityLayout />}>
                  <Route index element={<Navigate to="dashboard" replace />} />
                  <Route path="dashboard" element={<TracesDashboard />} />
                  <Route path="traces" element={<Traces />} />
                  <Route path="spend" element={<Spend />} />
                  <Route path="connectors" element={<Observability />} />
                  <Route path="mcp-logs" element={<MCPLogs />} />
                </Route>

                <Route path="gateway" element={<GatewayLayout />}>
                  <Route index element={<Navigate to="routing" replace />} />
                  <Route path="routing" element={<Routing />} />
                  <Route path="routing/:alias" element={<RoutingDetail />} />
                  <Route path="providers" element={<Credentials />} />
                  <Route path="keys" element={<Keys />} />
                  <Route path="guardrails" element={<Guardrails />} />
                  <Route path="cache" element={<SemanticCache />} />
                  <Route path="alerting" element={<Alerting />} />
                </Route>

                <Route path="mcp" element={<McpLayout />}>
                  <Route index element={<Navigate to="registry" replace />} />
                  <Route path="registry" element={<MCPRegistry />} />
                  <Route path="library" element={<McpLibrary />} />
                  <Route path="settings" element={<McpSettings />} />
                </Route>

                <Route path="eval" element={<EvalLayout />}>
                  <Route index element={<EvalOverview />} />
                  <Route path="profiles" element={<EvalProfilesPage />} />
                  <Route path="plugins" element={<EvalPlugins />} />
                  <Route path="benchmarks" element={<Benchmarks />} />
                  <Route path="routing" element={<EvalRoutingIntegration />} />
                </Route>

                <Route path="governance" element={<GovernanceLayout />}>
                  <Route index element={<Navigate to="users" replace />} />
                  <Route path="users" element={<Users />} />
                  <Route path="audit" element={<Audit />} />
                </Route>

                <Route path="develop" element={<DevelopLayout />}>
                  <Route index element={<Navigate to="playground" replace />} />
                  <Route path="playground" element={<Playground />} />
                  <Route path="docs" element={<Docs />} />
                  <Route path="docs/*" element={<Docs />} />
                </Route>

                {/* Legacy path redirects */}
                <Route path="spend" element={<Navigate to="/observability/spend" replace />} />
                <Route path="traces" element={<Navigate to="/observability/traces" replace />} />
                <Route path="mcp/logs" element={<Navigate to="/observability/mcp-logs" replace />} />
                <Route path="routing" element={<Navigate to="/gateway/routing" replace />} />
                <Route path="routing/:alias" element={<LegacyRoutingDetailRedirect />} />
                <Route path="credentials" element={<Navigate to="/gateway/providers" replace />} />
                <Route path="keys" element={<Navigate to="/gateway/keys" replace />} />
                <Route path="benchmarks" element={<Navigate to="/eval/benchmarks" replace />} />
                <Route path="users" element={<Navigate to="/governance/users" replace />} />
                <Route path="audit" element={<Navigate to="/governance/audit" replace />} />
                <Route path="playground" element={<Navigate to="/develop/playground" replace />} />
                <Route path="docs" element={<Navigate to="/develop/docs" replace />} />
                <Route path="docs/*" element={<LegacyDocsRedirect />} />

                <Route path="*" element={<Navigate to="/" replace />} />
              </Route>
            </Route>
          </Routes>
        </BrowserRouter>
      </QueryClientProvider>
    </ThemeProvider>
  );
}
