import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  fetchGatewayModels,
  fetchMyKeys,
  type GatewayModelCatalog,
  type VirtualKey,
} from "../api";
import { Chip } from "../components/Chip";
import { GradientText } from "../components/GradientText";
import { Drawer } from "../components/Drawer";
import { Icon } from "../components/icons";

async function fetchBundle() {
  const [keys, gw] = await Promise.all([fetchMyKeys(), fetchGatewayModels()]);
  return { keys: keys as VirtualKey[], gw: gw as GatewayModelCatalog };
}

export function Playground() {
  const { data } = useQuery({ queryKey: ["playground"], queryFn: fetchBundle });
  const keys = data?.keys?.filter((k) => !k.revoked) ?? [];
  const gw = data?.gw;

  const [model, setModel] = useState("auto");
  const [keyId, setKeyId] = useState<string>("");
  const [prompt, setPrompt] = useState("Hello! Reply in one sentence.");
  const [streamed, setStreamed] = useState("");
  const [thinking, setThinking] = useState(false);
  const [meta, setMeta] = useState<{ tokens?: number; cost?: number; latency?: number }>({});
  const [secret, setSecret] = useState<string>("");
  const [rememberSecret, setRememberSecret] = useState(true);
  const [secretDrawer, setSecretDrawer] = useState(false);

  // Auto-select first virtual key once they load.
  // Also: when the user picks a different virtual key in the dropdown and we
  // already have a remembered secret, we keep it — virtual key secrets are
  // valid until rotated, so there's no need to re-prompt on every selection.
  // We only force a re-prompt if the user explicitly chose "Forget".
  useEffect(() => {
    if (!keyId && keys.length > 0) setKeyId(keys[0].id);
  }, [keys, keyId]);

  const allChoices = useMemo(() => {
    const set = new Set<string>(["auto"]);
    (gw?.chat ?? []).forEach((m) => set.add(m));
    (gw?.user ?? []).forEach((u) => u.models.forEach((m) => set.add(m)));
    (gw?.embed ?? []).forEach((m) => set.add("embed:" + m));
    return Array.from(set);
  }, [gw]);

  async function run() {
    if (!secret) {
      setSecretDrawer(true);
      return;
    }
    setStreamed("");
    setMeta({});
    setThinking(true);
    const t0 = performance.now();
    let inputTokens = 0;
    let outputTokens = 0;
    try {
      const res = await fetch("/v1/chat/completions", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: "Bearer " + secret,
        },
        body: JSON.stringify({
          model,
          stream: true,
          messages: [{ role: "user", content: prompt }],
        }),
      });
      if (!res.ok) {
        const t = await res.text();
        setStreamed(`[error] ${res.status}: ${t}`);
        return;
      }
      const reader = res.body?.getReader();
      if (!reader) return;
      const dec = new TextDecoder();
      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        const chunk = dec.decode(value);
        for (const line of chunk.split("\n")) {
          if (!line.startsWith("data:")) continue;
          const payload = line.slice(5).trim();
          if (payload === "[DONE]") continue;
          try {
            const json = JSON.parse(payload);
            const delta = json.choices?.[0]?.delta?.content ?? "";
            setStreamed((s) => s + delta);
            if (json.usage) {
              inputTokens = json.usage.prompt_tokens ?? inputTokens;
              outputTokens = json.usage.completion_tokens ?? outputTokens;
            }
          } catch {
            // ignore
          }
        }
      }
      if (inputTokens || outputTokens) {
        setMeta({
          tokens: inputTokens + outputTokens,
          latency: Math.round(performance.now() - t0),
        });
      } else {
        setMeta({ latency: Math.round(performance.now() - t0) });
      }
    } finally {
      setThinking(false);
    }
  }

  return (
    <div className="playground-page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Workspace · playground
          </div>
          <h1 className="page-title">
            <GradientText as="span">Playground</GradientText>
          </h1>
          <p className="page-sub">
            Single-shot chat with the model of your choice. Streams live.
          </p>
        </div>
      </header>

      <div className="pg-grid">
        <div className="panel pg-input">
          <div className="pg-form-head">
            <label className="pg-row">
              <span className="pg-label">Virtual key</span>
              <select value={keyId} onChange={(e) => setKeyId(e.target.value)}>
                <option value="">— choose —</option>
                {keys.map((k) => (
                  <option key={k.id} value={k.id}>
                    {k.name} ({k.key_prefix}…)
                  </option>
                ))}
              </select>
            </label>
            <label className="pg-row">
              <span className="pg-label">Model</span>
              <select value={model} onChange={(e) => setModel(e.target.value)}>
                {allChoices.map((m) => (
                  <option key={m} value={m}>
                    {m}
                  </option>
                ))}
              </select>
            </label>
          </div>
          <label className="pg-row">
            <span className="pg-label">Prompt</span>
            <textarea
              value={prompt}
              onChange={(e) => setPrompt(e.target.value)}
              rows={6}
              autoFocus
            />
          </label>
          <div className="pg-actions">
            <button
              type="button"
              className="btn-neon"
              disabled={thinking || !prompt.trim() || !keyId}
              onClick={() => run()}
            >
              <Icon.play size={14} /> {thinking ? "Running…" : "Run"}
            </button>
            <span className="muted small">
              {gw === undefined
                ? "Catalog unavailable — connect the gateway to populate model list."
                : `${gw.chat.length} stock · ${gw.embed.length} embed · ${gw.user.length} user`}
            </span>
          </div>
        </div>

        <div className="panel pg-output">
          <div className="pg-output-head">
            <h3>Response</h3>
            {meta.latency !== undefined && (
              <Chip tone="info">{meta.latency}ms</Chip>
            )}
            {meta.tokens !== undefined && (
              <Chip tone="accent">{meta.tokens} tok</Chip>
            )}
          </div>
          <pre className="pg-stream">
            {streamed || (thinking ? "…" : "Run to see the streamed response.")}
          </pre>
        </div>
      </div>

      <Drawer
        open={secretDrawer}
        onClose={() => setSecretDrawer(false)}
        title="Use a key secret"
        footer={
          <>
            <button
              type="button"
              className="btn-ghost"
              onClick={() => setSecretDrawer(false)}
            >
              Cancel
            </button>
            <button
              type="button"
              className="btn-neon"
              form="pg-secret-form"
            >
              Use
            </button>
          </>
        }
      >
        <form
          id="pg-secret-form"
          className="form-stack"
          onSubmit={(e) => {
            e.preventDefault();
            // Box unchecked ⇒ explicit forget so the next Run re-prompts.
            if (!rememberSecret) setSecret("");
            setSecretDrawer(false);
          }}
        >
          <label className="field-row">
            <span className="field-label">Plaintext secret</span>
            <input
              type="password"
              value={secret}
              onChange={(e) => setSecret(e.target.value)}
              autoFocus
              placeholder="nxs_live_…"
              autoComplete="off"
            />
          </label>
          <label className="pg-remember-row">
            <input
              type="checkbox"
              checked={rememberSecret}
              onChange={(e) => setRememberSecret(e.target.checked)}
            />
            <span>
              Remember in this session
              <span className="muted small">
                {" "}— kept in tab memory only, dropped on tab close. Pick a
                different key from the dropdown without re-typing.
              </span>
            </span>
          </label>
          <p className="muted small">
            Playgrounds intentionally do not read secrets via API — copy from
            the Keys tab after creating or rotating. If you rotated this key
            since the last Run, untick the box to clear the cached secret.
          </p>
        </form>
      </Drawer>
    </div>
  );
}
