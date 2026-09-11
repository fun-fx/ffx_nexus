import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import {
  fetchMCPLog,
  fetchMCPLogFilterData,
  fetchMCPLogs,
  fetchMCPLogStats,
  type MCPLogDetail,
  type MCPLogQuery,
  type MCPLogSummary,
} from "../api";
import { DataTable, type Column } from "../components/DataTable";
import { Drawer } from "../components/Drawer";
import { Icon } from "../components/icons";
import { StatusPill } from "../components/StatusPill";

function formatTime(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}

function buildQuery(
  status: string,
  server: string,
  tool: string,
  search: string,
  limit: number,
): MCPLogQuery {
  return {
    limit,
    status: status === "all" ? undefined : status,
    server_label: server || undefined,
    tool_name: tool || undefined,
    q: search.trim() || undefined,
  };
}

export function MCPLogs() {
  const [statusFilter, setStatusFilter] = useState<"all" | "success" | "error">("all");
  const [serverFilter, setServerFilter] = useState("");
  const [toolFilter, setToolFilter] = useState("");
  const [search, setSearch] = useState("");
  const [cursor, setCursor] = useState<{ before?: string; since?: string }>({});
  const [items, setItems] = useState<MCPLogSummary[]>([]);
  const [selected, setSelected] = useState<MCPLogDetail | null>(null);

  const baseQuery = useMemo(
    () => buildQuery(statusFilter, serverFilter, toolFilter, search, 50),
    [statusFilter, serverFilter, toolFilter, search],
  );

  const { data: filters } = useQuery({ queryKey: ["mcp-log-filters"], queryFn: fetchMCPLogFilterData });
  const { data: stats } = useQuery({ queryKey: ["mcp-log-stats", baseQuery], queryFn: () => fetchMCPLogStats(baseQuery) });

  const { isLoading, refetch } = useQuery({
    queryKey: ["mcp-logs", baseQuery, cursor.before],
    queryFn: async () => {
      const page = await fetchMCPLogs({ ...baseQuery, ...cursor });
      if (!cursor.before) setItems(page.items);
      else setItems((prev) => [...prev, ...page.items]);
      return page;
    },
  });

  async function reload() {
    setCursor({});
    setItems([]);
    await refetch();
  }

  async function openDetail(row: MCPLogSummary) {
    const detail = await fetchMCPLog(row.id);
    setSelected(detail);
  }

  const columns: Column<MCPLogSummary>[] = [
    { id: "time", header: "Time", cell: (r) => formatTime(r.timestamp) },
    { id: "server", header: "Server", cell: (r) => r.server_label },
    { id: "tool", header: "Tool", cell: (r) => r.tool_name },
    {
      id: "status",
      header: "Status",
      cell: (r) => <StatusPill tone={r.status === "success" ? "ok" : "err"} label={r.status} />,
    },
    { id: "latency", header: "Latency", cell: (r) => `${r.latency_ms} ms` },
    {
      id: "trace",
      header: "LLM trace",
      cell: (r) =>
        r.turn_id ? (
          <Link to={`/traces?turn=${encodeURIComponent(r.turn_id)}`} className="link">
            turn
          </Link>
        ) : r.llm_trace_id ? (
          <span className="mono small">{r.llm_trace_id.slice(0, 8)}…</span>
        ) : (
          "—"
        ),
    },
  ];

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1>MCP Logs</h1>
          <p className="muted">Tool executions proxied through the Nexus MCP gateway.</p>
        </div>
        <button type="button" className="btn-ghost" onClick={() => reload()}>
          <Icon.refresh size={14} />
          Refresh
        </button>
      </header>

      <div className="stat-grid">
        <div className="stat-card">
          <div className="stat-label">Executions</div>
          <div className="stat-value">{stats?.total ?? 0}</div>
        </div>
        <div className="stat-card">
          <div className="stat-label">Success rate</div>
          <div className="stat-value">{((stats?.success_rate ?? 0) * 100).toFixed(1)}%</div>
        </div>
        <div className="stat-card">
          <div className="stat-label">Avg latency</div>
          <div className="stat-value">{(stats?.avg_latency_ms ?? 0).toFixed(0)} ms</div>
        </div>
        <div className="stat-card">
          <div className="stat-label">p95 latency</div>
          <div className="stat-value">{(stats?.p95_latency_ms ?? 0).toFixed(0)} ms</div>
        </div>
      </div>

      <div className="filter-bar">
        <label className="filter-select">
          <select
            value={statusFilter}
            onChange={(e) => setStatusFilter(e.target.value as typeof statusFilter)}
            aria-label="Status filter"
          >
            <option value="all">All statuses</option>
            <option value="success">Success</option>
            <option value="error">Error</option>
          </select>
        </label>
        <label className="filter-select">
          <select
            value={serverFilter}
            onChange={(e) => setServerFilter(e.target.value)}
            aria-label="Server filter"
          >
            <option value="">All servers</option>
            {(filters?.server_labels ?? []).map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </label>
        <label className="filter-select">
          <select
            value={toolFilter}
            onChange={(e) => setToolFilter(e.target.value)}
            aria-label="Tool filter"
          >
            <option value="">All tools</option>
            {(filters?.tool_names ?? []).map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </select>
        </label>
        <label className="filter-search filter-search-grow">
          <Icon.search size={14} />
          <input
            placeholder="Search arguments / results…"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            aria-label="Search arguments or results"
          />
        </label>
      </div>

      <DataTable
        columns={columns}
        rows={items}
        rowKey={(r) => r.id}
        emptyMessage={isLoading ? "Loading…" : "No MCP logs yet."}
        onRowClick={(r) => openDetail(r)}
      />

      <Drawer open={!!selected} onClose={() => setSelected(null)} title="MCP log detail">
        {selected && (
          <div className="detail-stack">
            <div>
              <strong>{selected.tool_name}</strong> on {selected.server_label}
            </div>
            <StatusPill tone={selected.status === "success" ? "ok" : "err"} label={selected.status} />
            <div className="muted">
              {formatTime(selected.timestamp)} · {selected.latency_ms} ms
            </div>
            {selected.turn_id && (
              <Link to={`/traces?turn=${encodeURIComponent(selected.turn_id)}`}>View related LLM turn →</Link>
            )}
            {selected.error_message && <pre className="code-block danger">{selected.error_message}</pre>}
            {selected.arguments && (
              <>
                <h4>Arguments</h4>
                <pre className="code-block">{selected.arguments}</pre>
              </>
            )}
            {selected.result && (
              <>
                <h4>Result</h4>
                <pre className="code-block">{selected.result}</pre>
              </>
            )}
          </div>
        )}
      </Drawer>
    </div>
  );
}
