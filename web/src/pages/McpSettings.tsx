import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { fetchMCPSettings, patchMCPSettings } from "../api";
import { Chip } from "../components/Chip";
import { GradientText } from "../components/GradientText";
import { LabelToggle } from "../components/LabelToggle";
import { SettingRow } from "../components/SettingRow";

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

      <section className="panel panel-form">
        <div className="panel-form__section">
          <h2 className="panel-form__section-title">Org defaults</h2>
          {settingsQ.isLoading ? (
            <p className="muted">Loading…</p>
          ) : settingsQ.error ? (
            <Chip tone="err">{(settingsQ.error as Error).message}</Chip>
          ) : (
            <>
              <label className="field-row">
                <span className="field-label">Default timeout (ms)</span>
                <span className="field-hint">
                  Applied to new installs from Library presets unless the YAML sets timeout_ms.
                </span>
                <input
                  type="number"
                  min={1000}
                  step={1000}
                  value={effectiveTimeout}
                  onChange={(e) => setTimeoutMs(Number(e.target.value))}
                  aria-label="Default timeout (ms)"
                />
              </label>
              <SettingRow
                label="Sticky HTTP connections"
                hint="Reuse HTTP connections to remote MCP servers when the transport supports it."
              >
                <LabelToggle
                  checked={effectiveSticky}
                  label="default sticky HTTP"
                  onChange={(v) => setSticky(v)}
                />
              </SettingRow>
              <div className="panel-form__actions">
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
              </div>
            </>
          )}
        </div>
      </section>

      <section className="panel panel-form">
        <div className="panel-form__section">
          <h2 className="panel-form__section-title">Gateway reference</h2>
          {snap ? (
            <>
              <p className="panel-form__intro">
                Call MCP tools through the OpenAI-compatible gateway base URL. Virtual key auth
                applies the same as chat completions.
              </p>
              <div className="mcp-ref-grid">
                <div className="mcp-ref-item">
                  <div className="field-label">Base URL</div>
                  <code className="mcp-ref-code">{snap.gateway_base_url}</code>
                </div>
                <div className="mcp-ref-item">
                  <div className="field-label">Routes</div>
                  <ul className="mcp-ref-routes">
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
                </div>
              </div>
              <pre className="code-block">{gatewaySnippet(snap.gateway_base_url)}</pre>
              <div className="panel-form__actions">
                <Link to="/mcp/registry" className="btn-ghost">
                  Registry
                </Link>
                <Link to="/observability/mcp-logs" className="btn-ghost">
                  MCP Logs
                </Link>
              </div>
            </>
          ) : (
            <p className="muted">Gateway routes load with settings above.</p>
          )}
        </div>
      </section>

      <section className="panel panel-form">
        <div className="panel-form__section">
          <h2 className="panel-form__section-title">OAuth &amp; sessions</h2>
          <p className="panel-form__intro">
            MCP OAuth grants and session management are on the roadmap. There is no backend wiring
            yet — this section is informational only.
          </p>
          <Chip tone="info">Coming soon</Chip>
        </div>
      </section>
    </div>
  );
}
