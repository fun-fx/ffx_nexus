import { Navigate, Outlet, useLocation } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { fetchMe } from "../api";

/**
 * Guards the authenticated console shell.
 *
 * On first paint with a fresh session (orally unauthenticated) we briefly
 * render a skeleton until `/api/me` resolves, then either continue rendering
 * the nested route (`<Outlet/>`) or bounce the user to `/login`.
 *
 * Keeping the layout mounted (instead of returning null during the network
 * round-trip) prevents the "render with stale data -> crash on
 * stats.total_requests.toLocaleString()" flash we previously saw on
 * incognito opens.
 */
export function RequireAuth() {
  const location = useLocation();
  const { data, isLoading } = useQuery({
    queryKey: ["me", "guard"],
    queryFn: fetchMe,
    // Never cache this guard for a long time — middleware relies on a fresh
    // role check on every navigation.
    staleTime: 0,
    retry: false,
    refetchOnWindowFocus: false,
  });

  if (isLoading) {
    return (
      <div className="auth-page" data-testid="auth-loading">
        <div className="auth-glow" aria-hidden="true" />
        <div className="auth-loading-card" role="status">
          <span className="logo-mark">◆</span>
          <span>Loading session…</span>
        </div>
      </div>
    );
  }

  if (!data) {
    const target = location.pathname + location.search;
    return <Navigate to={`/login?next=${encodeURIComponent(target)}`} replace />;
  }

  return <Outlet />;
}
