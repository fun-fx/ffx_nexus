import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  createMCPServer,
  fetchMCPServers,
  fetchMCPSettings,
  testMCPServer,
} from "../api";
import { Chip } from "../components/Chip";
import { Drawer } from "../components/Drawer";
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
      try {
        await testMCPServer(rec.id);
      } catch {
        /* stdio/npx may fail in dev — row still created */
      }
      return rec;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["mcp-servers"] });
      setMessage(`Installed "${name.trim()}". Open Registry to reconnect or edit.`);
      setSelected(null);
      setError(null);
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
            One-click installs from curated presets. Servers land in the{" "}
            <Link to="/mcp/registry">Registry</Link> and are callable through the gateway API.
          </p>
        </div>
      </header>

      {message ? (
        <div className="banner info" role="status">
          {message}{" "}
          <Link to="/mcp/registry" className="btn ghost">
            Open Registry
          </Link>
        </div>
      ) : null}

      <div className="quickstart-gallery" data-testid="mcp-library">
        <header className="quickstart-head">
          <div className="section-tabs" style={{ border: "none", marginBottom: 0, paddingBottom: 0 }}>
            {MCP_PRESET_CATEGORIES.map((c) => (
              <button
                key={c.id}
                type="button"
                className={"section-tab" + (category === c.id ? " is-active" : "")}
                onClick={() => setCategory(c.id)}
              >
                {c.label}
              </button>
            ))}
          </div>
        </header>
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
              <div className="quickstart-tile-meta muted small">{t.description}</div>
              {t.requiresEnv?.length ? (
                <div className="quickstart-tile-meta muted small">
                  env: {t.requiresEnv.join(", ")}
                </div>
              ) : null}
            </button>
          ))}
        </div>
      </div>

      <Drawer
        open={selected !== null}
        onClose={() => setSelected(null)}
        title={selected ? `Install ${selected.label}` : "Install"}
      >
        {selected ? (
          <>
            <label className="field">
              <span>Server name</span>
              <input
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder={selected.defaultName}
              />
            </label>
            {nameConflict ? (
              <p className="error small">
                A server named <code>{name.trim()}</code> already exists.{" "}
                <Link to="/mcp/registry">Open Registry</Link> to edit it.
              </p>
            ) : null}
            {selected.requiresEnv?.length ? (
              <p className="muted small">
                Edit the spec below and paste secrets into{" "}
                <code>env</code> before saving.
              </p>
            ) : null}
            <label className="field">
              <span>Spec (YAML)</span>
              <textarea
                rows={14}
                value={spec}
                onChange={(e) => setSpec(e.target.value)}
                spellCheck={false}
              />
            </label>
            {error ? <Chip tone="err">{error}</Chip> : null}
            <div className="drawer-actions">
              <button type="button" className="btn ghost" onClick={() => setSelected(null)}>
                Cancel
              </button>
              <button
                type="button"
                className="btn-neon"
                disabled={!name.trim() || nameConflict || installMut.isPending}
                onClick={() => installMut.mutate()}
              >
                {installMut.isPending ? "Installing…" : "Install"}
              </button>
            </div>
          </>
        ) : null}
      </Drawer>
    </div>
  );
}
