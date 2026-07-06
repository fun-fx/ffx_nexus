import { useEffect, useMemo, useState } from "react";
import { fetchMyCredentials, fetchMyKeys } from "./api";

// Playground is a one-shot chat completion panel modeled on LiteLLM's
// in-console playground. It uses the user's own virtual key (BYOK) directly
// against the gateway — no Nexus-mediated shortcut — so traces still flow
// through the gateway hot path and into /api/traces just like any other call.
//
// The virtual key is captured once into sessionStorage. Closing the tab
// erases it; a fresh tab prompts again. This keeps the dashboard usable
// without forcing a new backend proxy route just for the playground.

const VKEY_STORAGE = "nx.playground.vkey";

export function Playground() {
  const [creds, setCreds] = useState<{ provider: string; id: string; secret_last4: string }[]>([]);
  const [keys, setKeys] = useState<{ id: string; name: string; key_prefix: string }[]>([]);
  const [vkey, setVkey] = useState<string>(() => sessionStorage.getItem(VKEY_STORAGE) ?? "");
  const [model, setModel] = useState("gemini-2.5-flash");
  const [prompt, setPrompt] = useState("Say hi in five words.");
  const [response, setResponse] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [latencyMs, setLatencyMs] = useState<number | null>(null);

  // Read-only glance at the user's own keys & creds so they can pick the
  // right key without leaving the panel.
  useEffect(() => {
    fetchMyKeys().then(setKeys).catch(() => setKeys([]));
    fetchMyCredentials().then((rows) => {
      setCreds(rows.map((c) => ({ provider: c.provider, id: c.id, secret_last4: c.secret_last4 })));
    }).catch(() => setCreds([]));
  }, []);

  // Suggest a default model from the providers we see registered for the user.
  const knownProviders = useMemo(() => Array.from(new Set(creds.map((c) => c.provider))), [creds]);

  const send = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr("");
    setResponse("");
    setLatencyMs(null);
    if (!vkey) {
      setErr("Paste your virtual key (nxs_live_...) to use the playground.");
      return;
    }
    if (!prompt.trim()) {
      setErr("Type a prompt first.");
      return;
    }
    sessionStorage.setItem(VKEY_STORAGE, vkey);
    setBusy(true);
    const t0 = performance.now();
    try {
      const res = await fetch(`/v1/chat/completions`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${vkey}`,
        },
        body: JSON.stringify({ model, messages: [{ role: "user", content: prompt }] }),
      });
      const dt = performance.now() - t0;
      setLatencyMs(Math.round(dt));
      const text = await res.text();
      if (!res.ok) {
        setErr(`HTTP ${res.status} — ${text || res.statusText}`);
        return;
      }
      try {
        const data = JSON.parse(text);
        const content =
          data?.choices?.[0]?.message?.content ??
          data?.content?.[0]?.text ??
          "(empty response)";
        setResponse(String(content));
      } catch {
        setResponse(text);
      }
    } catch (e) {
      setErr(`Network error: ${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="panel playground">
      <h2>Playground</h2>
      <p className="sub">
        Send one chat completion through your gateway. Nexus traces every call —
        the request will appear in <strong>Recent traces</strong> on the Overview
        within a few seconds.
      </p>

      {knownProviders.length === 0 ? (
        <div className="notice">
          You haven't registered a provider key yet. Head to <strong>Account → My
          provider keys (BYOK)</strong> and add one (e.g. <code>gemini</code>) so
          Nexus can bill your provider instead of the operator. In strict-byok
          (default since v0.1.0) the call will return 403
          <code>missing_byok_key</code> otherwise.
        </div>
      ) : (
        <div className="sub">
          Registered providers for your account:{" "}
          {knownProviders.map((p) => (
            <span className="tag" key={p}>{p}</span>
          ))}
        </div>
      )}

      <form className="form" onSubmit={send}>
        <label className="row">
          <span>Virtual key</span>
          <input
            type="password"
            placeholder="nxs_live_..."
            value={vkey}
            onChange={(e) => setVkey(e.target.value)}
            autoComplete="off"
          />
        </label>
        <label className="row">
          <span>Model</span>
          <input
            placeholder="gemini-2.5-flash, gpt-4o-mini, auto, fast, ..."
            value={model}
            onChange={(e) => setModel(e.target.value)}
          />
        </label>
        <label className="row column">
          <span>Prompt</span>
          <textarea
            rows={4}
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
          />
        </label>
        <div className="row">
          <button className="btn" type="submit" disabled={busy}>
            {busy ? "Sending…" : "Send"}
          </button>
          {latencyMs !== null && <span className="sub">Last call: {latencyMs} ms</span>}
        </div>
      </form>

      {err && <div className="error">{err}</div>}
      {response && (
        <div className="notice response">
          <strong>Response</strong>
          <pre>{response}</pre>
        </div>
      )}

      {keys.length > 0 && (
        <div className="sub small">
          Your virtual keys:{" "}
          {keys.map((k) => (
            <span className="tag" key={k.id}>{k.name} ({k.key_prefix}…)</span>
          ))}
        </div>
      )}
    </section>
  );
}
