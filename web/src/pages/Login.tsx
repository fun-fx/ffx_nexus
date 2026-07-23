import { useEffect, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { fetchAuthConfig, fetchMe, login, register, type AuthConfig, type User } from "../api";
import { GradientText } from "../components/GradientText";
import { TierCard } from "../components/TierCard";
import { Icon } from "../components/icons";

export function Login() {
  const [cfg, setCfg] = useState<AuthConfig | null>(null);
  const [, setUser] = useState<User | null>(null);
  const [mode, setMode] = useState<"login" | "register">("login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const nav = useNavigate();
  const [searchParams] = useSearchParams();
  // If the user clicked a deep link while signed-out, RequireAuth redirected
  // us here with ?next=<original-path>. Honor it after a successful login so
  // the operator lands back where they intended.
  const nextTarget = (() => {
    const raw = searchParams.get("next");
    if (!raw) return null;
    // Only allow same-origin relative paths to avoid open-redirect issues.
    if (!raw.startsWith("/") || raw.startsWith("//")) return null;
    return raw;
  })();

  useEffect(() => {
    fetchAuthConfig().then(setCfg).catch(() => setCfg(null));
    refetchMe();
    // Refetch `me` whenever a new login flow finishes (other tabs log in,
    // session expires, etc.) so the auto-redirect kicks in without a manual
    // page reload.
    const onFocus = () => refetchMe();
    window.addEventListener("focus", onFocus);
    return () => window.removeEventListener("focus", onFocus);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function refetchMe() {
    try {
      const u = await fetchMe();
      if (u) nav(nextTarget ?? "/", { replace: true });
    } catch {
      /* server unreachable while offline — leave the form visible */
    }
  }

  const submit: React.FormEventHandler = async (e) => {
    e.preventDefault();
    setBusy(true);
    setErr("");
    try {
      if (mode === "login") {
        const u = await login(email, password);
        setUser(u);
      } else {
        await register({ email, password });
        const u = await login(email, password);
        setUser(u);
      }
      nav(nextTarget ?? "/", { replace: true });
    } catch (e2) {
      setErr(String((e2 as Error).message ?? e2));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="auth-page">
      <div className="auth-glow" aria-hidden="true" />
      <header className="auth-topbar">
        <div className="brand">
          <span className="logo-mark">◆</span>
          <span>
            Nexus
            <span className="brand-sub">LLM Gateway</span>
          </span>
        </div>
      </header>

      <div className="auth-grid">
        <section className="auth-hero">
          <div className="auth-eyebrow">
            <span className="dot" aria-hidden="true" /> Internal build · v0.5.1
          </div>
          <h1 className="auth-headline">
            One gateway. <br />
            <GradientText as="span">Every model.</GradientText>
            <br />
            Unified quality.
          </h1>
          <p className="auth-sub">
            Quality-aware routing, eval-aware failover, and a single pane of
            glass for keys, traces, and BYOK providers across the FFX Nexus
            stack.
          </p>

          <div className="auth-tier-row">
            <TierCard
              eyebrow="Code · agent"
              title="code-prime"
              metric="minimax-m3"
              description="Workhorse code + agent routing for day-to-day workload."
              glow="violet"
              onClick={() => (window.location.href = "/")}
              ctaLabel="Use as default"
              accent="#a855f7"
            />
            <TierCard
              eyebrow="Reasoning"
              title="text-max"
              metric="frontier"
              description="Deep reasoning, long context for hard workflows."
              glow="pink"
              onClick={() => (window.location.href = "/")}
              ctaLabel="Try in Playground"
              accent="#ec4899"
            />
            <TierCard
              eyebrow="Burst friendly"
              title="text-standard"
              metric="<0.04/IU"
              description="Price-optimized throughput for high-volume traffic."
              glow="cyan"
              onClick={() => (window.location.href = "/")}
              ctaLabel="Use as default"
              accent="#22d3ee"
            />
          </div>
        </section>

        <section className="auth-card">
          <div className="auth-tabs" role="tablist" aria-label="Sign-in mode">
            <button
              type="button"
              role="tab"
              aria-selected={mode === "login"}
              className={"auth-tab" + (mode === "login" ? " is-active" : "")}
              onClick={() => setMode("login")}
            >
              Sign in
            </button>
            <button
              type="button"
              role="tab"
              aria-selected={mode === "register"}
              className={"auth-tab" + (mode === "register" ? " is-active" : "")}
              onClick={() => setMode("register")}
              disabled={cfg?.signup_enabled === false}
              title={
                cfg?.signup_enabled === false
                  ? "Sign-up is disabled on this org"
                  : undefined
              }
            >
              Create account
            </button>
          </div>

          {cfg?.sso_enabled && (
            <a className="sso-btn" href="/api/auth/sso/login">
              <Icon.google size={16} />
              Continue with {cfg.sso_label ?? "SSO"}
            </a>
          )}

          <div className="divider" role="separator">
            <span>or</span>
          </div>

          <form className="auth-form" onSubmit={submit}>
            <label className="field-row">
              <span className="field-label">Email</span>
              <input
                type="email"
                autoComplete="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                disabled={busy}
                required
              />
            </label>
            <label className="field-row">
              <span className="field-label">Password</span>
              <input
                type="password"
                autoComplete={mode === "login" ? "current-password" : "new-password"}
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                disabled={busy}
                required
              />
            </label>
            {err && <div className="auth-error" role="alert">{err}</div>}
            <button className="btn-neon" type="submit" disabled={busy || !email || !password}>
              {busy ? "Signing in…" : mode === "login" ? "Sign in →" : "Create account →"}
            </button>
          </form>

          <div className="auth-foot">
            <span>Need access?</span>
            <a href="mailto:ops@nexus.local">Contact ops</a>
          </div>
        </section>
      </div>
    </div>
  );
}

export {};
