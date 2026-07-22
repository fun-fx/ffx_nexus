import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { DataTable, type Column } from "../components/DataTable";
import { Drawer } from "../components/Drawer";
import { Chip } from "../components/Chip";
import { StatusPill } from "../components/StatusPill";
import { Icon } from "../components/icons";
import { fetchMe, fetchTraces, type TraceSummary, type User } from "../api";

async function fetchTraceBundle() {
  const [me, list] = await Promise.allSettled([fetchMe(), fetchTraces(500)]);
  return {
    me: me.status === "fulfilled" ? (me.value as User | null) : null,
    traces: list.status === "fulfilled" ? (list.value as TraceSummary[]) : [],
  };
}

export function Traces() {
  const { data, isLoading } = useQuery({
    queryKey: ["traces"],
    queryFn: fetchTraceBundle,
    refetchInterval: 15_000,
  });

  const traces = data?.traces ?? [];
  const user = data?.me ?? null;
  const isAdmin = user?.role === "admin";

  const [statusFilter, setStatusFilter] = useState<"all" | "ok" | "err">("all");
  const [providerFilter, setProviderFilter] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  const [selected, setSelected] = useState<TraceSummary | null>(null);

  const providers = useMemo(
    () => Array.from(new Set(traces.map((t) => t.provider_name))).sort(),
    [traces],
  );

  const filtered = useMemo(() => {
    return traces.filter((t) => {
      if (statusFilter === "ok" && t.status_code >= 400) return false;
      if (statusFilter === "err" && t.status_code < 400) return false;
      if (providerFilter && t.provider_name !== providerFilter) return false;
      if (search.trim()) {
        const q = search.trim().toLowerCase();
        const hay =
          t.request_model +
          " " +
          t.provider_name +
          " " +
          (t.user_email ?? "") +
          " " +
          (t.guardrail_action ?? "");
        if (!hay.toLowerCase().includes(q)) return false;
      }
      return true;
    });
  }, [traces, statusFilter, providerFilter, search]);

  const columns: Column<TraceSummary>[] = [
    {
      id: "time",
      header: "Time",
      width: "120px",
      cell: (t) => (
        <span className="mono">
          {new Date(t.timestamp).toLocaleTimeString()}
        </span>
      ),
      sortValue: (t) => new Date(t.timestamp).getTime(),
      align: "left",
    },
    {
      id: "provider",
      header: "Provider",
      width: "120px",
      cell: (t) => <span className="provider-tag">{t.provider_name}</span>,
      sortValue: (t) => t.provider_name,
    },
    {
      id: "model",
      header: "Model",
      cell: (t) => <span className="mono ellipsis">{t.request_model}</span>,
      sortValue: (t) => t.request_model,
    },
    {
      id: "status",
      header: "Status",
      width: "90px",
      cell: (t) => (
        <StatusPill
          label={t.status_code.toString()}
          tone={t.status_code >= 400 ? "err" : "ok"}
        />
      ),
      sortValue: (t) => t.status_code,
    },
    {
      id: "latency",
      header: "Latency",
      width: "90px",
      align: "right",
      cell: (t) => <span className="mono">{t.latency_ms} ms</span>,
      sortValue: (t) => t.latency_ms,
    },
    {
      id: "tokens",
      header: "Tokens (in/out)",
      width: "120px",
      align: "right",
      cell: (t) => (
        <span className="mono">
          {t.input_tokens}/{t.output_tokens}
        </span>
      ),
      sortValue: (t) => (t.input_tokens ?? 0) + (t.output_tokens ?? 0),
    },
    {
      id: "cost",
      header: "Cost",
      width: "100px",
      align: "right",
      cell: (t) => <span className="mono">${t.cost_usd.toFixed(5)}</span>,
      sortValue: (t) => t.cost_usd,
    },
    {
      id: "flags",
      header: "Flags",
      width: "180px",
      cell: (t) => (
        <span className="flag-row">
          {t.cache_hit ? (
            <Chip tone="accent">cache</Chip>
          ) : null}
          {t.guardrail_action ? (
            <Chip tone="warn">{t.guardrail_action.split(":")[0]}</Chip>
          ) : null}
          {t.credential_source && t.credential_source !== "env" ? (
            <Chip tone="info">
              {t.credential_source === "user" ? "byok" : t.credential_source}
            </Chip>
          ) : null}
          {!t.cache_hit &&
          !t.guardrail_action &&
          (!t.credential_source || t.credential_source === "env") ? (
            <span className="muted">-</span>
          ) : null}
        </span>
      ),
    },
    ...(isAdmin
      ? [
          {
            id: "user",
            header: "User",
            width: "180px",
            cell: (t: TraceSummary) => t.user_email || "-",
            sortValue: (t: TraceSummary) => t.user_email ?? "",
          } as Column<TraceSummary>,
        ]
      : []),
  ];

  return (
    <div className="traces-page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Workspace · live
          </div>
          <h1 className="page-title">Traces</h1>
          <p className="page-sub">
            Gateway traffic. Filter, sort, and click a row to inspect.
          </p>
        </div>
        <div className="page-stats">
          <div className="page-stat">
            <div className="page-stat-label">rows</div>
            <div className="page-stat-value">{filtered.length}</div>
          </div>
          <div className="page-stat">
            <div className="page-stat-label">error rate</div>
            <div className="page-stat-value">
              {filtered.length === 0
                ? "—"
                : `${(
                    (filtered.filter((t) => t.status_code >= 400).length /
                      filtered.length) *
                    100
                  ).toFixed(1)}%`}
            </div>
          </div>
          <div className="page-stat">
            <div className="page-stat-label">avg p95 latency</div>
            <div className="page-stat-value">
              {filtered.length === 0
                ? "—"
                : `${Math.round(
                    Math.max(...filtered.map((t) => t.latency_ms)),
                  )} ms`}
            </div>
          </div>
        </div>
      </header>

      <div className="filter-bar">
        <div className="filter-search">
          <Icon.zap size={14} />
          <input
            placeholder="Search model, provider, or guardrail…"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            aria-label="Search traces"
          />
        </div>
        <div className="filter-chips" role="group" aria-label="Status filter">
          <Chip
            tone={statusFilter === "all" ? "accent" : "neutral"}
            active={statusFilter === "all"}
            onClick={() => setStatusFilter("all")}
          >
            All
          </Chip>
          <Chip
            tone={statusFilter === "ok" ? "ok" : "neutral"}
            active={statusFilter === "ok"}
            onClick={() => setStatusFilter("ok")}
          >
            2xx/3xx
          </Chip>
          <Chip
            tone={statusFilter === "err" ? "err" : "neutral"}
            active={statusFilter === "err"}
            onClick={() => setStatusFilter("err")}
          >
            4xx/5xx
          </Chip>
        </div>
        <div className="filter-chips" role="group" aria-label="Provider filter">
          <Chip
            tone={providerFilter === null ? "accent" : "neutral"}
            active={providerFilter === null}
            onClick={() => setProviderFilter(null)}
          >
            All providers
          </Chip>
          {providers.map((p) => (
            <Chip
              key={p}
              tone={providerFilter === p ? "accent" : "neutral"}
              active={providerFilter === p}
              onClick={() => setProviderFilter(p)}
            >
              {p}
            </Chip>
          ))}
        </div>
      </div>

      <div className="panel">
        <DataTable
          rows={filtered}
          columns={columns}
          rowKey={(t) => t.trace_id}
          onRowClick={(t) => setSelected(t)}
          emptyMessage={
            isLoading ? "Loading traces…" : "No traces match the filters."
          }
          initialSort={{ id: "time", dir: "desc" }}
        />
      </div>

      <Drawer
        open={Boolean(selected)}
        onClose={() => setSelected(null)}
        title={
          selected ? (
            <span className="mono">{selected.request_model}</span>
          ) : null
        }
        footer={
          selected ? (
            <button
              type="button"
              className="btn-ghost"
              onClick={() => setSelected(null)}
            >
              Close
            </button>
          ) : null
        }
      >
        {selected && <TraceDetail t={selected} />}
      </Drawer>
    </div>
  );
}

function TraceDetail({ t }: { t: TraceSummary }) {
  return (
    <div className="trace-detail">
      <div className="kv-grid">
        <KV label="Trace ID" value={<span className="mono">{t.trace_id}</span>} />
        <KV label="Time" value={new Date(t.timestamp).toLocaleString()} />
        <KV label="Provider" value={<span className="provider-tag">{t.provider_name}</span>} />
        <KV label="Requested model" value={<span className="mono">{t.request_model}</span>} />
        <KV label="Status" value={
          <StatusPill
            label={t.status_code.toString()}
            tone={t.status_code >= 400 ? "err" : "ok"}
          />
        } />
        <KV label="Latency" value={`${t.latency_ms} ms`} />
        <KV label="Cost" value={`$${t.cost_usd.toFixed(5)}`} />
        <KV label="TTFT" value={t.ttft_ms ? `${t.ttft_ms} ms` : "—"} />
        <KV label="Tokens" value={<span className="mono">{t.input_tokens}/{t.output_tokens}</span>} />
        <KV label="Streamed" value={t.streamed ? "yes" : "no"} />
        <KV label="Cache hit" value={t.cache_hit ? "yes" : "no"} />
        <KV label="Credential source" value={t.credential_source || "env"} />
        <KV label="User" value={t.user_email || "-"} />
      </div>
      {t.guardrail_action && (
        <>
          <h3 className="kv-section">Guardrail</h3>
          <div className="kv-grid">
            <KV label="Action" value={<Chip tone="warn">{t.guardrail_action}</Chip>} />
          </div>
        </>
      )}
    </div>
  );
}

function KV({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="kv">
      <div className="kv-label">{label}</div>
      <div className="kv-value">{value}</div>
    </div>
  );
}
