import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  createMCPServer,
  fetchMCPServers,
  fetchMCPSettings,
  testMCPServer,
  type PluginTestResult,
} from "../api";
import { Chip } from "../components/Chip";
import { Drawer } from "../components/Drawer";
import { Icon } from "../components/icons";
import { GradientText } from "../components/GradientText";
import {
  applyMcpOrgDefaults,
  MCP_PRESET_CATEGORIES,
  MCP_PRESETS,
  type MCPPreset,
  type MCPPresetCategory,
} from "../lib/mcpPresets";

export function McpLibrary() {
  const qc = useQueryClient();
  const serversQ = useQuery({ queryKey: ["mcp-servers"], queryFn: fetchMCPServers });
  const settingsQ = useQuery({ queryKey: ["mcp-settings"], queryFn: fetchMCPSettings });

  const [category, setCategory] = useState<MCPPresetCategory | "all">("all");
  const [selected, setSelected] = useState<MCPPreset | null>(null);
  const [name, setName] = useState("");
  const [spec, setSpec] = useState("");
  const [message, setMessage] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [testResult, setTestResult] = useState<PluginTestResult | null>(null);

  const existingNames = useMemo(
    () => new Set((serversQ.data?.servers ?? []).map((s) => s.name)),
    [serversQ.data?.servers],
  );

  const tiles = useMemo(() => {
    if (category === "all") return MCP_PRESETS;
    return MCP_PRESETS.filter((p) => p.category === category);
  }, [category]);

  const installMut = useMutation({
    mutationFn: async () => {
      const defaults = settingsQ.data ?? {
        default_timeout_ms: 60000,
        default_sticky_http: true,
      };
      const specYaml = applyMcpOrgDefaults(spec, {
        default_timeout_ms: defaults.default_timeout_ms,
        default_sticky_http: defaults.default_sticky_http,
      });
      const rec = await createMCPServer({
        name: name.trim(),
        spec_yaml: specYaml,
        enabled: true,
      });
      let test: PluginTestResult;
      try {
        test = await testMCPServer(rec.id);
      } catch (e) {
        test = {
          ok: false,
          message: e instanceof Error ? e.message : "test failed",
        };
      }
      return { rec, test };
    },
    onSuccess: ({ rec, test }) => {
      qc.invalidateQueries({ queryKey: ["mcp-servers"] });
      setTestResult(test);
      setError(null);
      const probe = test.ok ? test.message : `Test failed: ${test.message}`;
      setMessage(`Installed "${rec.name}". ${probe}`);
    },
    onError: (e: Error) => {
      setError(e.message);
    },
  });

  function openInstall(preset: MCPPreset) {
    setSelected(preset);
    setName(preset.defaultName);
    setSpec(preset.specYaml);
    setError(null);
    setMessage(null);
    setTestResult(null);
  }

  function closeInstall() {
    setSelected(null);
    setTestResult(null);
  }

  const nameConflict = name.trim() !== "" && existingNames.has(name.trim());

  return (
    <div className="page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> MCP · curated installs
          </div>
          <h1 className="page-title">
            <GradientText as="span">MCP</GradientText> Library
          </h1>
          <p className="page-sub">
            One-click installs from curated presets. Hosted tiles talk to a remote
            HTTP MCP. Local tiles start a process on the gateway host and need
            that command on <code>$PATH</code>. Servers land in the{" "}
            <Link to="/mcp/registry">Registry</Link>.
          </p>
        </div>
      </header>

      {message ? (
        <div className="banner info" role="status">
          {message}{" "}
          <Link to="/mcp/registry" className="btn-ghost">
            Open Registry
          </Link>
        </div>
      ) : null}

      <div className="filter-bar" role="group" aria-label="Preset category">
        <div className="filter-chips">
          {MCP_PRESET_CATEGORIES.map((c) => (
            <Chip
              key={c.id}
              tone={category === c.id ? "accent" : "neutral"}
              active={category === c.id}
              onClick={() => setCategory(c.id)}
            >
              {c.label}
            </Chip>
          ))}
        </div>
      </div>

      <div className="quickstart-gallery mcp-library-gallery" data-testid="mcp-library">
        <div className="quickstart-grid">
          {tiles.map((t) => (
            <button
              key={t.id}
              type="button"
              className="quickstart-tile"
              data-testid={`mcp-preset-${t.id}`}
              onClick={() => openInstall(t)}
            >
              <div className="quickstart-tile-title">{t.label}</div>
              <p className="quickstart-tile-meta">{t.description}</p>
              <div className="quickstart-tile-tags">
                <Chip tone={t.runtime === "local" ? "warn" : "ok"} data-testid={`mcp-runtime-${t.id}`}>
                  {t.runtime === "local" ? "Local" : "Hosted"}
                </Chip>
                {t.requiresEnv?.map((env) => (
                  <Chip key={env} tone="warn">
                    {env}
                  </Chip>
                ))}
                {t.requiresHeaders?.map((h) => (
                  <Chip key={h} tone="warn">
                    {h}
                  </Chip>
                ))}
              </div>
            </button>
          ))}
        </div>
      </div>

      <Drawer
        open={selected !== null}
        onClose={closeInstall}
        title={selected ? `Install ${selected.label}` : "Install"}
        footer={
          selected ? (
            <div className="drawer-footer">
              <button type="button" className="btn-ghost" onClick={closeInstall}>
                {testResult ? "Close" : "Cancel"}
              </button>
              <button
                type="button"
                className="btn-neon"
                disabled={!name.trim() || nameConflict || installMut.isPending || testResult !== null}
                onClick={() => installMut.mutate()}
              >
                <Icon.sparkles size={14} />
                {installMut.isPending ? "Installing…" : "Install"}
              </button>
            </div>
          ) : null
        }
      >
        {selected ? (
          <div className="form-stack">
            <label className="field-row">
              <span className="field-label">Server name</span>
              <span className="field-hint">Must be unique across your MCP registry.</span>
              <input
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder={selected.defaultName}
              />
            </label>
            {nameConflict && !testResult ? (
              <p className="error small">
                A server named <code>{name.trim()}</code> already exists.{" "}
                <Link to="/mcp/registry">Open Registry</Link> to edit it.
              </p>
            ) : null}
            {selected.runtime === "local" ? (
              <p className="field-hint" data-testid="mcp-local-warning">
                Local preset: the gateway starts <code>npx</code> (or the command in
                the spec) inside the Nexus process. That binary must be on the
                gateway <code>$PATH</code>. Cluster images typically do not include{" "}
                <code>npx</code> — Test will fail there.
              </p>
            ) : null}
            {selected.requiresEnv?.length ? (
              <p className="field-hint">
                Edit the spec below and paste secrets into <code>env</code> before saving.
              </p>
            ) : null}
            {selected.requiresHeaders?.length ? (
              <p className="field-hint">
                Edit the spec below and paste secrets into <code>headers</code> before saving.
              </p>
            ) : null}
            <label className="field-row">
              <span className="field-label">Spec (YAML)</span>
              <span className="field-hint">
                {selected.runtime === "local"
                  ? "Stdio command, args, and timeout for this preset."
                  : "HTTP URL, headers, and timeout for this preset."}
              </span>
              <textarea
                rows={14}
                value={spec}
                onChange={(e) => setSpec(e.target.value)}
                spellCheck={false}
              />
            </label>
            {error ? <Chip tone="err">{error}</Chip> : null}
            {testResult ? (
              <p data-testid="mcp-install-test">
                <Chip tone={testResult.ok ? "ok" : "err"}>
                  {name.trim()}: {testResult.ok ? testResult.message : `Failed: ${testResult.message}`}
                </Chip>
              </p>
            ) : null}
          </div>
        ) : null}
      </Drawer>
    </div>
  );
}
