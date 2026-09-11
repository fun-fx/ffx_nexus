import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  createMCPServer,
  deleteMCPServer,
  fetchMCPServers,
  fetchMCPSettings,
  patchMCPServer,
  reconnectMCPServer,
  testMCPServer,
  type MCPServerRecord,
  type MCPServerStatus,
} from "../api";
import { Link } from "react-router-dom";
import { applyMcpOrgDefaults } from "../lib/mcpPresets";
import { Chip } from "../components/Chip";
import { DataTable, type Column } from "../components/DataTable";
import { Drawer } from "../components/Drawer";
import { GradientText } from "../components/GradientText";
import { Icon } from "../components/icons";
import { StatusPill } from "../components/StatusPill";

const DEFAULT_SPEC = `connection:
  type: stdio
  command: npx
  args:
    - -y
    - "@modelcontextprotocol/server-filesystem"
    - /tmp
timeout_ms: 60000
`;

function statusFor(id: string, statuses: MCPServerStatus[]): MCPServerStatus | undefined {
  return statuses.find((s) => s.id === id);
}

function gatewaySnippet(serverId: string, tool: string): string {
  const base =
    typeof window !== "undefined"
      ? window.location.origin.replace(":8081", ":8080")
      : "http://127.0.0.1:8080";
  return `curl -s ${base}/v1/mcp/servers/${serverId}/tools/call \\
  -H "Authorization: Bearer $NEXUS_VIRTUAL_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{"name":"${tool}","arguments":{},"llm_trace_id":"<optional-trace-id>"}'`;
}

export function MCPRegistry() {
  const qc = useQueryClient();
  const { data, isLoading, error } = useQuery({
    queryKey: ["mcp-servers"],
    queryFn: fetchMCPServers,
  });
  const settingsQ = useQuery({ queryKey: ["mcp-settings"], queryFn: fetchMCPSettings });
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [editing, setEditing] = useState<MCPServerRecord | null>(null);
  const [name, setName] = useState("");
  const [spec, setSpec] = useState(DEFAULT_SPEC);
  const [enabled, setEnabled] = useState(true);
  const [testMsg, setTestMsg] = useState<string | null>(null);

  const saveMut = useMutation({
    mutationFn: async () => {
      if (editing?.id) {
        return patchMCPServer(editing.id, { name, spec_yaml: spec, enabled });
      }
      const defaults = settingsQ.data ?? {
        default_timeout_ms: 60000,
        default_sticky_http: true,
      };
      const specYaml = applyMcpOrgDefaults(spec, {
        default_timeout_ms: defaults.default_timeout_ms,
        default_sticky_http: defaults.default_sticky_http,
      });
      return createMCPServer({ name, spec_yaml: specYaml, enabled });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["mcp-servers"] });
      setDrawerOpen(false);
      setEditing(null);
    },
  });

  const reconnectMut = useMutation({
    mutationFn: reconnectMCPServer,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["mcp-servers"] }),
  });
  const testMut = useMutation({
    mutationFn: testMCPServer,
    onSuccess: (res) => setTestMsg(res.ok ? res.message : `Failed: ${res.message}`),
  });
  const deleteMut = useMutation({
    mutationFn: deleteMCPServer,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["mcp-servers"] }),
  });

  const rows = useMemo(() => {
    const servers = data?.servers ?? [];
    const statuses = data?.statuses ?? [];
    return servers.map((s) => ({
      record: s,
      status: statusFor(s.id, statuses),
    }));
  }, [data]);

  function openCreate() {
    setEditing(null);
    setName("");
    setSpec(DEFAULT_SPEC);
    setEnabled(true);
    setDrawerOpen(true);
  }

  function openEdit(rec: MCPServerRecord) {
    setEditing(rec);
    setName(rec.name);
    setSpec(rec.spec_yaml);
    setEnabled(rec.enabled);
    setDrawerOpen(true);
  }

  const columns: Column<(typeof rows)[number]>[] = [
    {
      id: "name",
      header: "Server",
      cell: (r) => (
        <div>
          <strong>{r.record.name}</strong>
          <div className="muted small">{r.status?.connection_type || "—"}</div>
        </div>
      ),
    },
    {
      id: "state",
      header: "Status",
      cell: (r) => (
        <StatusPill
          tone={
            r.status?.state === "healthy"
              ? "ok"
              : r.status?.state === "disabled" || !r.record.enabled
                ? "neutral"
                : "err"
          }
          label={r.record.enabled ? r.status?.state || "unknown" : "disabled"}
        />
      ),
    },
    {
      id: "tools",
      header: "Tools",
      cell: (r) => String(r.status?.tool_count ?? 0),
    },
    {
      id: "error",
      header: "Last error",
      cell: (r) => (
        <span className="muted small truncate" title={r.status?.last_error}>
          {r.status?.last_error || "—"}
        </span>
      ),
    },
    {
      id: "actions",
      header: "",
      cell: (r) => (
        <div className="row-actions">
          <button type="button" className="btn-ghost btn-small" onClick={() => openEdit(r.record)}>
            Edit
          </button>
          <button
            type="button"
            className="btn-ghost btn-small"
            onClick={() => reconnectMut.mutate(r.record.id)}
          >
            Reconnect
          </button>
          <button type="button" className="btn-ghost btn-small" onClick={() => testMut.mutate(r.record.id)}>
            Test
          </button>
          <button
            type="button"
            className="btn-ghost btn-small row-action-danger"
            onClick={() => deleteMut.mutate(r.record.id)}
          >
            Delete
          </button>
        </div>
      ),
    },
  ];

  const sampleServer = rows[0];
  const sampleTool = sampleServer?.status?.tools?.[0]?.name ?? "tool_name";

  return (
    <div className="page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> MCP · server registry
          </div>
          <h1 className="page-title">
            <GradientText as="span">MCP</GradientText> Registry
          </h1>
          <p className="page-sub">
            Register stdio or HTTP MCP servers for the gateway to proxy.
          </p>
        </div>
        <div className="page-stats">
          <div className="page-stat">
            <div className="page-stat-label">servers</div>
            <div className="page-stat-value">{rows.length}</div>
          </div>
          <button type="button" className="btn-neon" onClick={openCreate}>
            <Icon.sparkles size={14} />
            Add server
          </button>
        </div>
      </header>

      {testMsg ? <div className="banner info">{testMsg}</div> : null}
      {error ? <div className="banner danger">{(error as Error).message}</div> : null}

      {rows.length === 0 && !isLoading ? (
        <div className="empty-card">
          <h2 className="section-title">No MCP servers yet</h2>
          <p className="muted">Add a server, then call tools through the gateway API.</p>
          <div className="panel-form__actions">
            <button type="button" className="btn-neon" onClick={openCreate}>
              <Icon.sparkles size={14} />
              Add server
            </button>
            <Link to="/mcp/library" className="btn-ghost">
              Browse Library
            </Link>
          </div>
          <pre className="code-block">{gatewaySnippet("<server-id>", sampleTool)}</pre>
        </div>
      ) : (
        <div className="panel">
          <DataTable
            columns={columns}
            rows={rows}
            rowKey={(r) => r.record.id}
            emptyMessage={isLoading ? "Loading…" : undefined}
          />
        </div>
      )}

      <Drawer
        open={drawerOpen}
        onClose={() => setDrawerOpen(false)}
        title={editing ? "Edit MCP server" : "Add MCP server"}
        footer={
          <div className="drawer-footer">
            <button type="button" className="btn-ghost" onClick={() => setDrawerOpen(false)}>
              Cancel
            </button>
            <button
              type="button"
              className="btn-neon"
              disabled={!name.trim() || saveMut.isPending}
              onClick={() => saveMut.mutate()}
            >
              {saveMut.isPending ? "Saving…" : "Save"}
            </button>
          </div>
        }
      >
        <div className="form-stack">
          <label className="field-row">
            <span className="field-label">Name</span>
            <span className="field-hint">Short identifier used in the gateway API path.</span>
            <input value={name} onChange={(e) => setName(e.target.value)} placeholder="filesystem" />
          </label>
          <label className="field-row">
            <span className="field-label">Spec (YAML)</span>
            <span className="field-hint">Connection type, command, args, and timeout for this server.</span>
            <textarea rows={14} value={spec} onChange={(e) => setSpec(e.target.value)} spellCheck={false} />
          </label>
          <label className="trace-dashboard__check">
            <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
            <span>Enabled</span>
          </label>
          {saveMut.error ? <Chip tone="err">{(saveMut.error as Error).message}</Chip> : null}
        </div>
      </Drawer>
    </div>
  );
}
