import { Navigate, Outlet, useLocation, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";
import { fetchMe } from "../api";

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

// safeNext keeps /login's ?next= parameter constrained to a same-origin
// relative path. GHSA-jjmj-jmhj-qwj2 (open-redirect leading to XSS) in
// react-router 6.x allows attacker-supplied navigate() targets to reach
// external URLs; we want any user whose session expired mid-action to land
// back on their original internal page, never on a phishing decoy. Returning
// "/" as the default when the path is malformed ensures the login flow still
// works without ever surfacing attacker-controlled bytes to react-router's
// navigate().
function safeNext(path: string): string {
  if (!path.startsWith("/") || path.startsWith("//")) {
    return "/";
  }
  return path;
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
      const target = safeNext(location.pathname + location.search);
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
    const target = safeNext(location.pathname + location.search);
    return <Navigate to={`/login?next=${encodeURIComponent(target)}`} replace />;
  }

  return <Outlet />;
}
