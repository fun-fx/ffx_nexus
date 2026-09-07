import { useMemo, useState } from "react";
import type { AuthConfig } from "../api";
import { gatewayBase } from "../lib/setup";

type Tab = "curl" | "openai" | "anthropic";

export function FirstRequestSnippets({
  cfg,
  heading = "Send your first request",
}: {
  cfg?: AuthConfig | null;
  heading?: string;
}) {
  const [tab, setTab] = useState<Tab>("curl");
  const [copied, setCopied] = useState(false);
  const base = gatewayBase(cfg);

  const snippets = useMemo(() => {
    const curl = `curl ${base}/v1/chat/completions \\
  -H "Authorization: Bearer nxs_live_..." \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "auto",
    "messages": [{"role": "user", "content": "Hello, Nexus!"}]
  }'`;
    const openai = `from openai import OpenAI

client = OpenAI(
    api_key="nxs_live_...",
    base_url="${base}/v1",
)
print(client.chat.completions.create(
    model="auto",
    messages=[{"role": "user", "content": "Hello, Nexus!"}],
))`;
    const anthropic = `import anthropic

client = anthropic.Anthropic(
    api_key="nxs_live_...",
    base_url="${base}",
)
print(client.messages.create(
    model="auto",
    max_tokens=64,
    messages=[{"role": "user", "content": "Hello, Nexus!"}],
))`;
    return { curl, openai, anthropic } as const;
  }, [base]);

  const body = snippets[tab];

  async function copy() {
    try {
      await navigator.clipboard.writeText(body);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      /* clipboard can be denied in tests / http */
    }
  }

  return (
    <div className="first-req" data-testid="first-request-snippets">
      <div className="first-req-head">
        <h3>{heading}</h3>
        <p className="muted">
          Point any OpenAI-compatible SDK at the gateway. Replace{" "}
          <code>nxs_live_...</code> with a key from Keys.
        </p>
      </div>
      <div className="first-req-tabs" role="tablist">
        {(
          [
            ["curl", "cURL"],
            ["openai", "OpenAI SDK"],
            ["anthropic", "Anthropic SDK"],
          ] as const
        ).map(([id, label]) => (
          <button
            key={id}
            type="button"
            role="tab"
            aria-selected={tab === id}
            className={"first-req-tab" + (tab === id ? " is-active" : "")}
            onClick={() => setTab(id)}
          >
            {label}
          </button>
        ))}
        <button type="button" className="btn-ghost first-req-copy" onClick={copy}>
          {copied ? "Copied" : "Copy"}
        </button>
      </div>
      <pre className="first-req-code">{body}</pre>
    </div>
  );
}
