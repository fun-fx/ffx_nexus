import { useEffect, useState } from "react";
import {
  createMyCredential,
  createMyKey,
  createUser,
  deleteMyCredential,
  deleteUser,
  fetchMyCredentials,
  fetchMyKeys,
  fetchUsers,
  login,
  logout,
  updateMe,
  type Credential,
  type User,
  type VirtualKey,
} from "./api";

// Account renders the BYOK self-service area: login, my provider keys, my
// virtual keys, the per-user budget toggle, and (for admins) user management.
export function Account({ user, onUser }: { user: User | null; onUser: (u: User | null) => void }) {
  if (!user) return <LoginForm onUser={onUser} />;
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

      <div className="grid-2">
        <MyCredentials />
        <MyKeys />
      </div>

      {user.role === "admin" && <Users />}
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

function MyCredentials() {
  const [creds, setCreds] = useState<Credential[]>([]);
  const [provider, setProvider] = useState("openai");
  const [secret, setSecret] = useState("");
  const [name, setName] = useState("");
  const [err, setErr] = useState("");
  const load = () => fetchMyCredentials().then(setCreds).catch(() => {});
  useEffect(() => {
    load();
  }, []);
  const add = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr("");
    try {
      await createMyCredential({ provider, name, secret });
      setSecret("");
      setName("");
      load();
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  return (
    <section className="panel">
      <h2>My provider keys (BYOK)</h2>
      <form className="form row" onSubmit={add}>
        <select value={provider} onChange={(e) => setProvider(e.target.value)}>
          <option value="openai">openai</option>
          <option value="anthropic">anthropic</option>
          <option value="gemini">gemini</option>
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
      </form>
      {err && <div className="error">{err}</div>}
      <table>
        <thead>
          <tr>
            <th>Provider</th>
            <th>Label</th>
            <th>Key</th>
            <th>Added</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {creds.length === 0 && (
            <tr>
              <td colSpan={5} className="empty">
                No personal keys yet. Add one to call providers with your own account.
              </td>
            </tr>
          )}
          {creds.map((c) => (
            <tr key={c.id}>
              <td>
                <span className="tag">{c.provider}</span>
              </td>
              <td>{c.name || "-"}</td>
              <td className="muted">****{c.secret_last4}</td>
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
      <h2>My virtual keys</h2>
      <form className="form row" onSubmit={add}>
        <input placeholder="key name" value={name} onChange={(e) => setName(e.target.value)} />
        <button className="btn" type="submit">
          Create
        </button>
      </form>
      {created && (
        <div className="notice">
          Copy your key now — it won't be shown again:
          <code>{created}</code>
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
