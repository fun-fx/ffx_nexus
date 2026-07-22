import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Ticker } from "./Ticker";
import { ThemeToggle } from "../theme/ThemeToggle";
import { fetchMe, logout } from "../api";

interface LiveSummary {
  requestsPerMin: number;
  p95Ms: number;
  cacheHitRate: number;
  errorRate: number;
}

export function Topbar() {
  const navigate = useNavigate();
  const [live, setLive] = useState(false);
  const [sum, setSum] = useState<LiveSummary | null>(null);
  const [email, setEmail] = useState<string | null>(null);
  const [signingOut, setSigningOut] = useState(false);

  // Lightweight stats polling for the ticker; honors reduced motion.
  useEffect(() => {
    let cancelled = false;
    const tick = async () => {
      try {
        const res = await fetch("/api/stats?window=1h", {
          credentials: "same-origin",
        });
        if (!res.ok) {
          if (!cancelled) setLive(false);
          return;
        }
        const data = await res.json();
        if (cancelled) return;
        setSum({
          requestsPerMin: Math.round((data.total_requests ?? 0) / 60),
          p95Ms: Math.round(data.p95_latency_ms ?? 0),
          cacheHitRate: (data.cache_hit_rate ?? 0) * 100,
          errorRate: (data.error_rate ?? 0) * 100,
        });
        setLive(true);
      } catch {
        if (!cancelled) setLive(false);
      }
    };
    tick();
    const id = setInterval(tick, 15_000);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, []);

  // Pull the current user once on mount so the logout control can show the
  // signed-in email without diverting through the Sidebar's effect.
  useEffect(() => {
    let cancelled = false;
    fetchMe()
      .then((u) => {
        if (!cancelled) setEmail(u?.email ?? null);
      })
      .catch(() => {
        if (!cancelled) setEmail(null);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const onSignOut = async () => {
    if (signingOut) return;
    setSigningOut(true);
    try {
      await logout();
    } catch {
      // Even on network failure we still bounce to /login — the session cookie
      // is the source of truth; if it's gone the gateway will reject the next
      // request.
    }
    navigate("/login", { replace: true });
  };

  const tickerItems = sum
    ? [
        `${sum.requestsPerMin} req/min`,
        `p95 ${sum.p95Ms} ms`,
        `cache ${sum.cacheHitRate.toFixed(1)}%`,
        `err ${sum.errorRate.toFixed(2)}%`,
        `LIVE • ${new Date().toLocaleTimeString()}`,
      ]
    : ["Live trace stream offline"];

  return (
    <header className="topbar">
      <div className="topbar-left">
        <ThemeToggle />
        <div className="ticker-wrap">
          <Ticker items={tickerItems} />
        </div>
      </div>
      <div className="topbar-right">
        <span
          className={`live-pill ${live ? "is-live" : "is-offline"}`}
          role="status"
          aria-live="polite"
        >
          <span className="dot" aria-hidden="true" />
          {live ? "LIVE" : "OFFLINE"}
        </span>
        {email && (
          <button
            type="button"
            className="btn-ghost logout-btn"
            onClick={onSignOut}
            disabled={signingOut}
            title={email}
            aria-label={`Sign out (${email})`}
          >
            {signingOut ? "Signing out…" : "Sign out"}
          </button>
        )}
      </div>
    </header>
  );
}
