import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { fetchMCPSettings, patchMCPSettings } from "../api";
import { Chip } from "../components/Chip";
import { GradientText } from "../components/GradientText";
import { LabelToggle } from "../components/LabelToggle";

function gatewaySnippet(base: string, serverId = "<server-id>", tool = "tool_name") {
  return `curl -s ${base}/v1/mcp/servers/${serverId}/tools/call \\
  -H "Authorization: Bearer $NEXUS_VIRTUAL_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{"name":"${tool}","arguments":{},"llm_trace_id":"<optional-trace-id>"}'`;
}

export function McpSettings() {
  const qc = useQueryClient();
  const settingsQ = useQuery({ queryKey: ["mcp-settings"], queryFn: fetchMCPSettings });
  const snap = settingsQ.data;

  const [timeoutMs, setTimeoutMs] = useState<number | null>(null);
  const [sticky, setSticky] = useState<boolean | null>(null);

  const effectiveTimeout = timeoutMs ?? snap?.default_timeout_ms ?? 60000;
  const effectiveSticky = sticky ?? snap?.default_sticky_http ?? true;

  const saveMut = useMutation({
    mutationFn: () =>
      patchMCPSettings({
        default_timeout_ms: effectiveTimeout,
        default_sticky_http: effectiveSticky,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["mcp-settings"] });
      setTimeoutMs(null);
      setSticky(null);
    },
  });

  return (
    <div className="page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> MCP · org defaults
          </div>
          <h1 className="page-title">
            <GradientText as="span">MCP</GradientText> Settings
          </h1>
          <p className="page-sub">
            Defaults applied when installing from the{" "}
            <Link to="/mcp/library">Library</Link> or creating servers without explicit
            timeout values.
          </p>
        </div>
      </header>

      <section className="panel" style={{ padding: "1.25rem", marginBottom: "1rem" }}>
        <h2 className="section-title">Org defaults</h2>
        {settingsQ.isLoading ? (
          <p className="muted">Loading…</p>
        ) : settingsQ.error ? (
          <Chip tone="err">{(settingsQ.error as Error).message}</Chip>
        ) : (
          <>
            <label className="field">
              <span>Default timeout (ms)</span>
              <input
                type="number"
                min={1000}
                step={1000}
                value={effectiveTimeout}
                onChange={(e) => setTimeoutMs(Number(e.target.value))}
              />
            </label>
            <div className="field">
              <span>Sticky HTTP connections</span>
              <LabelToggle
                checked={effectiveSticky}
                label="default sticky HTTP"
                onChange={(v) => setSticky(v)}
              />
            </div>
            <button
              type="button"
              className="btn-neon"
              disabled={saveMut.isPending}
              onClick={() => saveMut.mutate()}
            >
              {saveMut.isPending ? "Saving…" : "Save defaults"}
            </button>
            {saveMut.error ? (
              <Chip tone="err">{(saveMut.error as Error).message}</Chip>
            ) : null}
          </>
        )}
      </section>

      <section className="panel" style={{ padding: "1.25rem", marginBottom: "1rem" }}>
        <h2 className="section-title">Gateway reference</h2>
        {snap ? (
          <>
            <p className="muted small">
              Base URL: <code>{snap.gateway_base_url}</code>
            </p>
            <ul className="muted small">
              <li>
                <code>{snap.mcp_routes.list_servers}</code>
              </li>
              <li>
                <code>{snap.mcp_routes.list_tools}</code>
              </li>
              <li>
                <code>{snap.mcp_routes.call_tool}</code>
              </li>
            </ul>
            <pre className="code-block">{gatewaySnippet(snap.gateway_base_url)}</pre>
            <p style={{ marginTop: "0.75rem" }}>
              <Link to="/mcp/registry" className="btn ghost">
                Registry
              </Link>{" "}
              <Link to="/observability/mcp-logs" className="btn ghost">
                MCP Logs
              </Link>
            </p>
          </>
        ) : (
          <p className="muted">Gateway routes load with settings above.</p>
        )}
      </section>

      <section className="tier-card" role="status">
        <h2 className="tier-card-title">OAuth &amp; sessions</h2>
        <p className="tier-card-desc">
          MCP OAuth grants and session management are on the roadmap. There is no backend
          wiring yet — this card is informational only.
        </p>
      </section>
    </div>
  );
}
