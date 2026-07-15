import { useEffect, useState } from "react";
import {
  createMyCredential,
  createMyKey,
  createUser,
  deleteMyCredential,
  deleteUser,
  fetchAuthConfig,
  fetchMe,
  fetchMyCredentials,
  fetchMyKeys,
  fetchMyStats,
  fetchMyTraces,
  fetchMyQuality,
  fetchUserQuality,
  fetchUsers,
  login,
  logout,
  register,
  startSSOLogin,
  updateMe,
  type Credential,
  type CredentialModels,
  type MyUsageStats,
  type MyUsageQuality,
  type TraceSummary,
  type User,
  type UserQuality,
  type VirtualKey,
} from "./api";

// Account renders the BYOK self-service area: login, my provider keys, my
// virtual keys, the per-user budget toggle, and (for admins) user management.
export function Account({ user, onUser }: { user: User | null; onUser: (u: User | null) => void }) {
  if (!user) return <AuthPanel onUser={onUser} />;
  return (
    <div className="account">
      <div className="account-head">
        <div>
          Signed in as <strong>{user.email}</strong>{" "}
          <span className="tag">{user.role}</span>
        </div>
        <button
          className="btn ghost"
          onClick={async () => {
            await logout();
            onUser(null);
          }}
        >
          Sign out
        </button>
      </div>

      <EnforceToggle user={user} onUser={onUser} />

      {/* v1.1 onboarding checklist: first-run banner + 3-step indicator.
          Hidden once onboarded_at is stamped (the backend sets it on the
          first successful /api/me/credentials create, PR #87). Hidden for
          admins who already manage keys on behalf of others. */}
      {!user.onboarded_at && user.role !== "admin" && <OnboardingChecklist />}

      <div className="grid-2">
        <MyCredentials onUser={onUser} />
        <MyKeys />
      </div>

      {user.role === "admin" && <UserQualityPanel />}
      {user.role === "admin" && <Users />}
      <MyUsage user={user} />
    </div>
  );
}

// ONBOARDING_V11_ENABLED is set at build time via the Vite env. We default
// to ON — the banner is opt-out via NEXUS_ONBOARDING_V11=false in the env
// the build sees. Defaulting ON matches the per-design behaviour: ship the
// banner, decide based on click-through whether to keep it.
const ONBOARDING_V11_ENABLED: boolean =
  (import.meta.env.VITE_NEXUS_ONBOARDING_V11 ?? "true") !== "false";

// OnboardingChecklist shows the first-run banner + step list for a user who
// has not yet been onboarded (users.onboarded_at IS NULL, per PR #87).
// Clicking "Set up your first provider key" scrolls the page to the BYOK
// form so the user lands right on the input. After the first successful
// /api/me/credentials create, the Account component re-renders because the
// updated User (with onboarded_at) flows through the onUser callback, and
// this component disappears — no extra state to manage here.
function OnboardingChecklist() {
  if (!ONBOARDING_V11_ENABLED) return null;
  const goToCredentials = () => {
    const el = document.getElementById("byok-form");
    if (!el) return;
    el.scrollIntoView({ behavior: "smooth", block: "start" });
    const secretInput = el.querySelector<HTMLInputElement>("input[type='password']");
    secretInput?.focus();
  };
  return (
    <section className="panel onboarding">
      <h2>Welcome — finish setting up your gateway</h2>
      <p className="sub">
        Nexus needs <strong>one provider key</strong> on file before it can route
        your first request. Three short steps:
      </p>
      <ol className="onb-steps">
        <li className="done">
          <strong>Create account</strong>
          <small>You&apos;re here ✓</small>
        </li>
        <li>
          <strong>Add a provider credential</strong>
          <small>Paste an upstream API key (BYOK). Encrypted at rest; only last-4 shown.</small>
        </li>
        <li>
          <strong>Create a virtual key &amp; make your first chat completion</strong>
          <small>Use the SDK with the gateway URL + your virtual key.</small>
        </li>
      </ol>
      <button className="btn" type="button" onClick={goToCredentials}>
        Set up your first provider key
      </button>
    </section>
  );
}

// helpIcon renders a small "?" button that toggles a one-line tooltip. We
// inline this rather than ship a tooltip library — the v1.1 onboarding
// design is explicit about wanting lightweight hints, not a full tour.
function HelpHint({ children }: { children: React.ReactNode }) {
  const [open, setOpen] = useState(false);
  return (
    <span className="help-hint">
      <button
        type="button"
        aria-label="Help"
        className="help-q"
        onClick={() => setOpen((v) => !v)}
      >
        ?
      </button>
      {open && (
        <span className="help-pop" role="tooltip" onClick={() => setOpen(false)}>
          {children}
        </span>
      )}
    </span>
  );
}

function ProviderHelp() {
  return (
    <HelpHint>
      A provider key is the API token you already have with OpenAI /
      Anthropic / etc. Nexus keeps it encrypted so it can route your
      traffic. You&apos;re billed by your provider — Nexus never resells
      tokens.
    </HelpHint>
  );
}

function VirtualKeyHelp() {
  return (
    <HelpHint>
      A virtual key is the token your SDK sends to the Nexus gateway.
      Each one can carry its own RPM cap and monthly budget, so you can
      hand different keys to different apps.
    </HelpHint>
  );
}

// CodeSnippet shows the exact curl a developer can paste into a terminal
// to verify the end-to-end flow after creating their first virtual key.
// gatewayUrl defaults to window.location.origin so it works whether the
// console is on Tailscale or public ingress (v1.1 §4).
function CodeSnippet({
  gatewayUrl,
  virtualKey,
}: {
  gatewayUrl: string;
  virtualKey: string;
}) {
  const [copied, setCopied] = useState(false);
  const payload = JSON.stringify(
    {
      model: "gpt-4o-mini",
      messages: [{ role: "user", content: "hello from Nexus" }],
    },
    null,
    2,
  );
  const snippet =
    `curl -s ${gatewayUrl}/v1/chat/completions \\\n` +
    `  -H "Authorization: Bearer ${virtualKey}" \\\n` +
    `  -H "Content-Type: application/json" \\\n` +
    `  -d '${payload}'`;
  const onCopy = async () => {
    try {
      await navigator.clipboard.writeText(snippet);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      // Older browsers / insecure contexts: skip silently. The user can
      // still select-and-copy manually because <pre> is selectable.
    }
  };
  return (
    <div className="code-snippet">
      <div className="code-snippet-head">
        <span className="sub">Try it from your terminal:</span>
        <button type="button" className="btn small" onClick={onCopy}>
          {copied ? "Copied ✓" : "Copy"}
        </button>
      </div>
      <pre>{snippet}</pre>
    </div>
  );
}

// BUILTIN_PROVIDERS lists the provider names whose adapter is shipped in
// the Go binary. Anything else routes through the dynamic OpenAI-compatible
// path that PR #68 added: the user provides base_url + an optional model
// inventory and the gateway auto-registers a wrapper on the next boot.
const BUILTIN_PROVIDERS = ["openai", "anthropic", "gemini", "groq", "mistral", "the_grid"];

// summarizeModels renders the per-credential model inventory as a compact
// "N chat / M embed" string for the credential table; "—" when the owner
// did not declare a list (the gateway forwards whatever model id the
// caller sends).
function summarizeModels(m: CredentialModels | undefined): string {
  if (!m) return "—";
  const chat = m.chat?.length ?? 0;
  const embed = m.embed?.length ?? 0;
  if (chat === 0 && embed === 0) return "—";
  const parts: string[] = [];
  if (chat > 0) parts.push(`${chat} chat`);
  if (embed > 0) parts.push(`${embed} embed`);
  return parts.join(" / ");
}

// UserQualityPanel is Nexus's eval differentiator surfaced in the console: each
// user's rolling quality score and pass rate alongside their spend — not just
// per-key spend like spend-only gateways.
function UserQualityPanel() {
  const [rows, setRows] = useState<UserQuality[]>([]);
  useEffect(() => {
    fetchUserQuality("24h").then(setRows).catch(() => {});
  }, []);
  return (
    <section className="panel">
      <h2>
        Per-user quality <span className="sub">(24h)</span>
      </h2>
      <table>
        <thead>
          <tr>
            <th>User</th>
            <th>Avg quality</th>
            <th>Pass rate</th>
            <th>Eval samples</th>
            <th>Requests</th>
            <th>Spend</th>
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 && (
            <tr>
              <td colSpan={6} className="empty">
                No per-user eval scores yet. Quality is scored out-of-band as BYOK
                traffic flows.
              </td>
            </tr>
          )}
          {rows.map((q) => (
            <tr key={q.user_id}>
              <td>{q.email || q.user_id}</td>
              <td>{q.avg_quality > 0 ? q.avg_quality.toFixed(2) : "-"}</td>
              <td>{(q.pass_rate * 100).toFixed(0)}%</td>
              <td>{q.samples}</td>
              <td>{q.requests}</td>
              <td>${q.cost_usd.toFixed(4)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}

function AuthPanel({ onUser }: { onUser: (u: User) => void }) {
  const [mode, setMode] = useState<"login" | "signup">("login");
  const [signupEnabled, setSignupEnabled] = useState(false);
  const [ssoEnabled, setSsoEnabled] = useState(false);
  const [ssoLabel, setSsoLabel] = useState("SSO");

  useEffect(() => {
    fetchAuthConfig()
      .then((c) => {
        setSignupEnabled(c.signup_enabled);
        setSsoEnabled(c.sso_enabled);
        setSsoLabel(c.sso_label || "SSO");
      })
      .catch(() => {});
  }, []);

  return (
    <div className="auth-panel">
      {ssoEnabled && (
        <section className="panel">
          <h2>Sign in with your organization</h2>
          <p className="sub">
            Use single sign-on if your company runs an IdP (e.g. {ssoLabel}).
          </p>
          <button className="btn sso" type="button" onClick={() => startSSOLogin()}>
            Sign in with {ssoLabel}
          </button>
        </section>
      )}
      {signupEnabled && (
        <div className="auth-tabs">
          <button
            type="button"
            className={mode === "login" ? "btn" : "btn ghost"}
            onClick={() => setMode("login")}
          >
            Sign in
          </button>
          <button
            type="button"
            className={mode === "signup" ? "btn" : "btn ghost"}
            onClick={() => setMode("signup")}
          >
            Create account
          </button>
        </div>
      )}
      {mode === "signup" && signupEnabled ? (
        <SignupForm onUser={onUser} onSignIn={() => setMode("login")} />
      ) : (
        <LoginForm onUser={onUser} />
      )}
    </div>
  );
}

function LoginForm({ onUser }: { onUser: (u: User) => void }) {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [err, setErr] = useState("");
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr("");
    try {
      onUser(await login(email, password));
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  return (
    <section className="panel">
      <h2>Sign in</h2>
      <form className="form" onSubmit={submit}>
        <input placeholder="email" value={email} onChange={(e) => setEmail(e.target.value)} />
        <input
          type="password"
          placeholder="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
        <button className="btn" type="submit">
          Sign in
        </button>
      </form>
      {err && <div className="error">{err}</div>}
    </section>
  );
}

function SignupForm({
  onUser,
  onSignIn,
}: {
  onUser: (u: User) => void;
  onSignIn: () => void;
}) {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [virtualKey, setVirtualKey] = useState("");
  const [pendingUser, setPendingUser] = useState<User | null>(null);
  const [err, setErr] = useState("");
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr("");
    setVirtualKey("");
    setPendingUser(null);
    try {
      const res = await register({ email, password });
      if (res.warnings?.length) {
        setErr(res.warnings.join(" "));
      }
      if (res.virtual_key) {
        setVirtualKey(res.virtual_key);
        setPendingUser(res.user);
        return;
      }
      onUser(res.user);
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  if (pendingUser && virtualKey) {
    return (
      <section className="panel">
        <h2>Account created</h2>
        <div className="notice">
          Copy your virtual key now — it won&apos;t be shown again:
          <code>{virtualKey}</code>
        </div>
        <p className="sub">
          Next: head to <strong>My provider keys (BYOK)</strong> below and add
          at least one provider key (e.g. <code>gemini</code>) so that Nexus
          can bill your provider instead of the operator. In strict-byok
          (default since v0.1.0) every call needs your own key.
        </p>
        <button className="btn" type="button" onClick={() => onUser(pendingUser)}>
          Continue to dashboard
        </button>
      </section>
    );
  }
  return (
    <section className="panel">
      <h2>Create account</h2>
      <p className="sub">
        Just an email and password. After signing up, you&apos;ll paste at
        least one provider key (BYOK) so Nexus can call upstream providers on
        your behalf — your provider bills you directly, Nexus never pays.
      </p>
      <form className="form" onSubmit={submit}>
        <input placeholder="email" value={email} onChange={(e) => setEmail(e.target.value)} />
        <input
          type="password"
          placeholder="password (min 8 characters)"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
        <button className="btn" type="submit">
          Create account
        </button>
      </form>
      {err && <div className="error">{err}</div>}
      <button type="button" className="btn ghost" onClick={onSignIn}>
        Already have an account? Sign in
      </button>
    </section>
  );
}

function EnforceToggle({ user, onUser }: { user: User; onUser: (u: User) => void }) {
  const [busy, setBusy] = useState(false);
  const toggle = async () => {
    setBusy(true);
    try {
      onUser(await updateMe(!user.enforce_limits));
    } finally {
      setBusy(false);
    }
  };
  return (
    <section className="panel">
      <h2>Usage limits</h2>
      <label className="toggle">
        <input type="checkbox" checked={user.enforce_limits} disabled={busy} onChange={toggle} />
        <span>
          Enforce Nexus-side monthly budget &amp; rate limits on my keys
          <small>
            {user.enforce_limits
              ? "On — Nexus caps apply as a safety guardrail (your provider bill is still yours)."
              : "Off — only your provider's own limits apply."}
          </small>
        </span>
      </label>
    </section>
  );
}

function MyCredentials({ onUser }: { onUser: (u: User) => void }) {
  const [creds, setCreds] = useState<Credential[]>([]);
  // Stock providers are registered out of the box; any value in
  // "BUILTIN_PROVIDERS" is a normal label. Anything else routes through the
  // dynamic OpenAI-compatible path: OBS sends base_url, models becomes
  // /v1/models.
  const [provider, setProvider] = useState<string>("openai");
  const [customName, setCustomName] = useState<string>("");
  const [baseURL, setBaseURL] = useState<string>("");
  const [chatModels, setChatModels] = useState<string>("");
  const [embedModels, setEmbedModels] = useState<string>("");
  const [secret, setSecret] = useState("");
  const [name, setName] = useState("");
  const [err, setErr] = useState("");
  const load = () => fetchMyCredentials().then(setCreds).catch(() => {});
  useEffect(() => {
    load();
  }, []);
  const isBuiltin = BUILTIN_PROVIDERS.includes(provider);
  const add = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr("");
    const finalProvider = isBuiltin ? provider : customName.trim();
    if (!finalProvider) {
      setErr("Provider is required.");
      return;
    }
    if (!isBuiltin && !baseURL.trim()) {
      setErr("base_url is required for custom providers.");
      return;
    }
    // Build a clean models payload only when something was provided; the
    // backend stores {} otherwise and the gateway uses its built-in catalog.
    const parseList = (raw: string): string[] =>
      raw
        .split(/[\s,]+/)
        .map((s) => s.trim())
        .filter(Boolean);
    const models =
      !isBuiltin && (chatModels.trim() || embedModels.trim())
        ? { chat: parseList(chatModels), embed: parseList(embedModels) }
        : undefined;
    try {
      await createMyCredential({
        provider: finalProvider,
        name,
        base_url: isBuiltin ? undefined : baseURL.trim(),
        secret,
        ...(models ? { models } : {}),
      });
      setSecret("");
      setName("");
      setBaseURL("");
      setChatModels("");
      setEmbedModels("");
      setCustomName("");
      // Re-read /api/me so the freshly-stamped onboarded_at flows into the
      // parent <Account /> and the first-run banner disappears. PR #87 sets
      // users.onboarded_at on the server side; without this re-read the UI
      // would still see the stale user object and keep showing the banner.
      try {
        const me = await fetchMe();
        if (me) onUser(me);
      } catch {
        // Non-fatal: even if /api/me fails, the credential table below has
        // already been refreshed by load(). The banner staying one render
        // longer is a graceful-failure mode we can live with.
      }
      load();
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  return (
    <section className="panel">
      <h2>
        My provider keys (BYOK) <ProviderHelp />
      </h2>
      <p className="sub">
        Nexus stores each provider key encrypted under its own KEK. Strict-byok
        (default) rejects gateway calls from anyone who hasn&apos;t registered a
        key for the target provider — register at least one here before sending
        traffic.
      </p>
      <form className="form" id="byok-form" onSubmit={add}>
        <div className="form row">
          <select
            value={provider}
            onChange={(e) => setProvider(e.target.value)}
          >
            {BUILTIN_PROVIDERS.map((p) => (
              <option key={p} value={p}>
                {p}
              </option>
            ))}
            <option value="_custom">Custom (OpenAI-compatible)…</option>
          </select>
          <input placeholder="label (optional)" value={name} onChange={(e) => setName(e.target.value)} />
          <input
            placeholder="API key"
            value={secret}
            onChange={(e) => setSecret(e.target.value)}
            type="password"
          />
          <button className="btn" type="submit">
            Add
          </button>
        </div>
        {!isBuiltin && (
          <>
            <input
              placeholder="provider name (e.g. openrouter, together, fireworks)"
              value={customName}
              onChange={(e) => setCustomName(e.target.value)}
            />
            <input
              placeholder="base URL (https://api.example.com/v1)"
              value={baseURL}
              onChange={(e) => setBaseURL(e.target.value)}
              required
            />
            <input
              placeholder="chat models (comma-separated, optional)"
              value={chatModels}
              onChange={(e) => setChatModels(e.target.value)}
            />
            <input
              placeholder="embed models (comma-separated, optional)"
              value={embedModels}
              onChange={(e) => setEmbedModels(e.target.value)}
            />
            <p className="sub">
              Models are exposed at <code>/v1/models</code> under{" "}
              <code>user/&lt;provider&gt;/&lt;model&gt;</code>; pass any model id
              the upstream accepts if you&apos;d rather skip the inventory list.
            </p>
          </>
        )}
      </form>
      {err && <div className="error">{err}</div>}
      <table>
        <thead>
          <tr>
            <th>Provider</th>
            <th>Label</th>
            <th>Key</th>
            <th>Models</th>
            <th>Added</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {creds.length === 0 && (
            <tr>
              <td colSpan={6} className="empty">
                No personal keys yet. Add one to call providers with your own account.
              </td>
            </tr>
          )}
          {creds.map((c) => (
            <tr key={c.id}>
              <td>
                <span className="tag">{c.provider}</span>
                {c.base_url && <small className="muted"> {c.base_url}</small>}
              </td>
              <td>{c.name || "-"}</td>
              <td className="muted">****{c.secret_last4}</td>
              <td className="muted">{summarizeModels(c.models)}</td>
              <td>{new Date(c.created_at).toLocaleDateString()}</td>
              <td>
                <button
                  className="btn ghost small"
                  onClick={async () => {
                    await deleteMyCredential(c.id);
                    load();
                  }}
                >
                  Delete
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}

function MyKeys() {
  const [keys, setKeys] = useState<VirtualKey[]>([]);
  const [name, setName] = useState("");
  const [created, setCreated] = useState<string>("");
  const load = () => fetchMyKeys().then(setKeys).catch(() => {});
  useEffect(() => {
    load();
  }, []);
  const add = async (e: React.FormEvent) => {
    e.preventDefault();
    const res = await createMyKey(name || "my-key");
    setCreated(res.secret);
    setName("");
    load();
  };
  return (
    <section className="panel">
      <h2>
        My virtual keys <VirtualKeyHelp />
      </h2>
      <form className="form row" onSubmit={add}>
        <input placeholder="key name" value={name} onChange={(e) => setName(e.target.value)} />
        <button className="btn" type="submit">
          Create
        </button>
      </form>
      {created && (
        <div className="notice">
          Copy your key now — it won&apos;t be shown again:
          <code>{created}</code>
          <p className="sub">
            Paste it into your SDK as <code>apiKey</code>, and point the SDK
            base URL at the gateway. Sample:
          </p>
          <CodeSnippet
            gatewayUrl={typeof window !== "undefined" ? window.location.origin : ""}
            virtualKey={created}
          />
        </div>
      )}
      <table>
        <thead>
          <tr>
            <th>Name</th>
            <th>Key</th>
            <th>Created</th>
          </tr>
        </thead>
        <tbody>
          {keys.length === 0 && (
            <tr>
              <td colSpan={3} className="empty">
                No virtual keys yet.
              </td>
            </tr>
          )}
          {keys.map((k) => (
            <tr key={k.id}>
              <td>{k.name}</td>
              <td className="muted">
                {k.key_prefix}…{k.key_last4}
              </td>
              <td>{new Date(k.created_at).toLocaleDateString()}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}

function MyUsage({ user }: { user: User }) {
  const [stats, setStats] = useState<MyUsageStats | null>(null);
  const [quality, setQuality] = useState<MyUsageQuality[]>([]);
  const [traces, setTraces] = useState<TraceSummary[]>([]);
  const [err, setErr] = useState("");

  const load = () => {
    fetchMyStats("1h")
      .then(setStats)
      .catch((e) => setErr((e as Error).message));
    fetchMyQuality("24h")
      .then(setQuality)
      .catch((e) => setErr((e as Error).message));
    fetchMyTraces(20)
      .then(setTraces)
      .catch((e) => setErr((e as Error).message));
  };

  useEffect(() => {
    load();
    const id = setInterval(load, 15000);
    return () => clearInterval(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const q = quality[0];
  return (
    <section className="panel">
      <h2>
        My usage <span className="sub">(last hour / last 24h quality)</span>
      </h2>
      <p className="sub">
        {user.email} only — your own requests, cost, and rolling quality score.
      </p>
      {err && <div className="error">{err}</div>}
      <div className="cards mini">
        <UsageCard label="My requests (1h)" value={(stats?.total_requests ?? 0).toLocaleString()} />
        <UsageCard label="My error rate" value={`${((stats?.error_rate ?? 0) * 100).toFixed(1)}%`} />
        <UsageCard label="My avg latency" value={`${Math.round(stats?.avg_latency_ms ?? 0)} ms`} />
        <UsageCard label="My tokens (1h)" value={(stats?.total_tokens ?? 0).toLocaleString()} />
        <UsageCard label="My cost (1h)" value={`$${(stats?.total_cost_usd ?? 0).toFixed(4)}`} />
        <UsageCard
          label="My quality (24h)"
          value={q ? (q.avg_quality > 0 ? q.avg_quality.toFixed(2) : "-") : "-"}
        />
      </div>
      <h3>My recent traces</h3>
      <table>
        <thead>
          <tr>
            <th>Time</th>
            <th>Provider</th>
            <th>Model</th>
            <th>Tokens (in/out)</th>
            <th>Latency</th>
            <th>Cost</th>
            <th>Status</th>
          </tr>
        </thead>
        <tbody>
          {traces.length === 0 && (
            <tr>
              <td colSpan={7} className="empty">
                No traffic under your account yet.
              </td>
            </tr>
          )}
          {traces.map((t) => (
            <tr key={t.trace_id}>
              <td>{new Date(t.timestamp).toLocaleTimeString()}</td>
              <td>{t.provider_name}</td>
              <td>{t.request_model}</td>
              <td>
                {t.input_tokens}/{t.output_tokens}
              </td>
              <td>{t.latency_ms} ms</td>
              <td>${t.cost_usd.toFixed(5)}</td>
              <td>{t.status_code}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}

function UsageCard({ label, value }: { label: string; value: string }) {
  return (
    <div className="card">
      <div className="card-label">{label}</div>
      <div className="card-value">{value}</div>
    </div>
  );
}

function Users() {
  const [users, setUsers] = useState<User[]>([]);
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState("member");
  const [err, setErr] = useState("");
  const load = () => fetchUsers().then(setUsers).catch(() => {});
  useEffect(() => {
    load();
  }, []);
  const add = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr("");
    try {
      await createUser({ email, password, role });
      setEmail("");
      setPassword("");
      load();
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  return (
    <section className="panel">
      <h2>Users (admin)</h2>
      <form className="form row" onSubmit={add}>
        <input placeholder="email" value={email} onChange={(e) => setEmail(e.target.value)} />
        <input
          type="password"
          placeholder="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
        <select value={role} onChange={(e) => setRole(e.target.value)}>
          <option value="member">member</option>
          <option value="admin">admin</option>
        </select>
        <button className="btn" type="submit">
          Add user
        </button>
      </form>
      {err && <div className="error">{err}</div>}
      <table>
        <thead>
          <tr>
            <th>Email</th>
            <th>Role</th>
            <th>Limits</th>
            <th>Created</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {users.map((u) => (
            <tr key={u.id}>
              <td>{u.email}</td>
              <td>
                <span className="tag">{u.role}</span>
              </td>
              <td>{u.enforce_limits ? "enforced" : "off"}</td>
              <td>{new Date(u.created_at).toLocaleDateString()}</td>
              <td>
                <button
                  className="btn ghost small"
                  onClick={async () => {
                    await deleteUser(u.id);
                    load();
                  }}
                >
                  Delete
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}
