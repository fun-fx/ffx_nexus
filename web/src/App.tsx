import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";
import { ThemeProvider } from "./theme/ThemeProvider";
import { AppShell } from "./components/AppShell";
import { Overview } from "./pages/Overview";
import { Login } from "./pages/Login";
import { Traces } from "./pages/Traces";
import { Routing } from "./pages/Routing";
import { RoutingDetail } from "./pages/RoutingDetail";
import { Keys } from "./pages/Keys";
import { Users } from "./pages/Users";
import { Credentials } from "./pages/Credentials";
import { Eval } from "./pages/Eval";
import { Audit } from "./pages/Audit";
import { Playground } from "./pages/Playground";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // Keep stat-style feeds cached briefly but always refetch on mount
      // so a fresh navigation into a gated page (Eval, Users, Audit) does
      // not inherit a stale role-less `me` from the previous page.
      staleTime: 5_000,
      gcTime: 5 * 60_000,
      retry: 1,
      refetchOnWindowFocus: true,
      refetchOnMount: "always",
    },
  },
});

export function App() {
  return (
    <ThemeProvider>
      <QueryClientProvider client={queryClient}>
        <BrowserRouter>
          <Routes>
            <Route path="/login" element={<Login />} />
            <Route element={<AppShell />}>
              <Route index element={<Overview />} />
              <Route path="traces" element={<Traces />} />
              <Route path="routing" element={<Routing />} />
              <Route path="routing/:alias" element={<RoutingDetail />} />
              <Route path="eval" element={<Eval />} />
              <Route path="users" element={<Users />} />
              <Route path="keys" element={<Keys />} />
              <Route path="credentials" element={<Credentials />} />
              <Route path="audit" element={<Audit />} />
              <Route path="playground" element={<Playground />} />
              <Route path="*" element={<Navigate to="/" replace />} />
            </Route>
          </Routes>
        </BrowserRouter>
      </QueryClientProvider>
    </ThemeProvider>
  );
}
