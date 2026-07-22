import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { fetchAudit, type AuditEntry } from "../api";
import { DataTable, type Column } from "../components/DataTable";
import { Chip } from "../components/Chip";
import { GradientText } from "../components/GradientText";
import { Icon } from "../components/icons";

const ACTIONS = ["all", "user", "auth", "vkey", "credential", "me", "sso"] as const;
type ActionGroup = typeof ACTIONS[number];

function classify(action: string): ActionGroup {
  if (action.startsWith("user")) return "user";
  if (action.startsWith("auth") || action.startsWith("sso")) return "auth";
  if (action.startsWith("vkey")) return "vkey";
  if (action.startsWith("credential")) return "credential";
  if (action.startsWith("me")) return "me";
  if (action.startsWith("sso")) return "sso";
  return "all";
}

export function Audit() {
  const [group, setGroup] = useState<ActionGroup>("all");
  const q = useQuery({
    queryKey: ["audit", group],
    queryFn: async () => {
      const list = await fetchAudit({ limit: 500 }).catch(() => [] as AuditEntry[]);
      return group === "all"
        ? list
        : list.filter((e) => classify(e.action).startsWith(group === "sso" ? "sso" : group));
    },
    refetchInterval: 15_000,
  });

  const rows = q.data ?? [];

  const cols: Column<AuditEntry>[] = [
    {
      id: "time",
      header: "Time",
      width: "170px",
      cell: (e) => <span className="mono">{new Date(e.created_at).toLocaleString()}</span>,
      sortValue: (e) => e.created_at,
    },
    {
      id: "action",
      header: "Action",
      width: "170px",
      cell: (e) => <code>{e.action}</code>,
      sortValue: (e) => e.action,
    },
    {
      id: "actor",
      header: "Actor",
      cell: (e) => <span className="muted mono">{e.actor || "system"}</span>,
      sortValue: (e) => e.actor,
    },
    {
      id: "detail",
      header: "Detail",
      cell: (e) => <span className="muted small">{e.detail}</span>,
    },
    {
      id: "target",
      header: "Target",
      width: "180px",
      cell: (e) =>
        e.target_id ? (
          <span className="mono target">{e.target_id}</span>
        ) : (
          <span className="muted">—</span>
        ),
      sortValue: (e) => e.target_id ?? "",
    },
  ];

  return (
    <div className="audit-page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Admin · audit
          </div>
          <h1 className="page-title">
            <GradientText as="span">Audit log</GradientText>
          </h1>
          <p className="page-sub">
            Control-plane changes. Anything that can affect billing, identity, or
            routing. Use it for SOC2 / postmortems.
          </p>
        </div>
        <div className="page-stats">
          <div className="page-stat">
            <div className="page-stat-label">rows</div>
            <div className="page-stat-value">{rows.length}</div>
          </div>
        </div>
      </header>

      <div className="filter-bar">
        <div className="filter-chips" role="group" aria-label="Action filter">
          {ACTIONS.map((g) => (
            <Chip
              key={g}
              tone={group === g ? "accent" : "neutral"}
              active={group === g}
              onClick={() => setGroup(g)}
              icon={<Icon.dash size={10} />}
            >
              {g}
            </Chip>
          ))}
        </div>
      </div>

<div className="panel">
          <DataTable
            rows={rows}
            columns={cols}
            rowKey={(e) => String(e.id)}
            emptyMessage={q.isLoading ? "Loading…" : "Nothing logged yet."}
            initialSort={{ id: "time", dir: "desc" }}
          />
        </div>
    </div>
  );
}
