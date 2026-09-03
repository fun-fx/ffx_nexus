import { Navigate, Outlet, useLocation, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";
import { fetchMe } from "../api";
import { safeNext } from "../lib/safeNext";

/**
 * Guards the authenticated console shell.
 *
 * On first paint with a fresh session (orally unauthenticated) we briefly
 * render a skeleton until `/api/me` resolves, then either continue rendering
 * the nested route (`<Outlet/>`) or bounce the user to `/login`.
 *
 * Once authenticated, we periodically re-validate the session in the
 * background. If a previously good session becomes invalid (cookie expired,
 * rotated, revoked) we force a navigation back to the login screen with
 * ?next=<current path>, preserving the user's place.
 *
 * Keeping the layout mounted (instead of returning null during the network
 * round-trip) prevents the "render with stale data -> crash on
 * stats.total_requests.toLocaleString()" flash we previously saw on
 * incognito opens.
 */
const SESSION_CHECK_MS = 60_000;

// Fall back to the console root when the current location is not a target we
// would redirect to. safeNext returns null in that case; here that means
// "send them home after login" rather than "do not redirect", because the
// user is on their way to the login screen either way.
function nextFrom(pathAndSearch: string): string {
  return safeNext(pathAndSearch) ?? "/";
}

export function RequireAuth() {
  const location = useLocation();
  const navigate = useNavigate();
  const qc = useQueryClient();

  const { data, isLoading, isError } = useQuery({
    queryKey: ["me", "guard"],
    queryFn: fetchMe,
    // Never cache this guard for a long time — the role bit drives admin
    // route visibility elsewhere.
    staleTime: 0,
    retry: false,
    refetchOnWindowFocus: false,
    refetchInterval: SESSION_CHECK_MS,
    refetchIntervalInBackground: true,
  });

  // When the periodic check transitions to "no user", kick off a redirect so
  // the page is no longer rendered with stale assumptions.
  useEffect(() => {
    if (isError) {
      const target = nextFrom(location.pathname + location.search);
      qc.clear();
      navigate(`/login?next=${encodeURIComponent(target)}`, { replace: true });
    }
  }, [isError, location.pathname, location.search, navigate, qc]);

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
    const target = nextFrom(location.pathname + location.search);
    return <Navigate to={`/login?next=${encodeURIComponent(target)}`} replace />;
  }

  return <Outlet />;
}
