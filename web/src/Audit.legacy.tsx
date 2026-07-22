import { useEffect, useMemo, useState } from "react";
import { fetchAudit, type AuditEntry } from "./api";

// Audit renders the admin audit-log table with simple filters. The backend
// already restricts /api/audit to admins; we still guard the UI to avoid
// showing the tab to non-admins and to keep the route tidy.
export function Audit() {
  const [rows, setRows] = useState<AuditEntry[]>([]);
  const [action, setAction] = useState("");
  const [userId, setUserId] = useState("");
  const [since, setSince] = useState<"1h" | "24h" | "7d" | "all">("24h");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string>("");

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError("");
    fetchAudit({
      limit: 200,
      action: action || undefined,
      user_id: userId || undefined,
      since: since === "all" ? undefined : since,
    })
      .then((data) => {
        if (cancelled) return;
        setRows(data);
      })
      .catch((e) => {
        if (cancelled) return;
        setError(String(e?.message || e));
        setRows([]);
      })
      .finally(() => {
        if (cancelled) return;
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [action, userId, since]);

  // De-duplicate action values from loaded rows for the datalist. Sorted so
  // common ones ("auth.login", "vkey.create", ...) float up if seen.
  const actionOptions = useMemo(() => {
    const set = new Set<string>();
    for (const r of rows) set.add(r.action);
    return Array.from(set).sort();
  }, [rows]);

  return (
    <section className="panel">
      <h2>
        Audit log{" "}
        <span className="muted" style={{ fontWeight: 500, fontSize: 12 }}>
          {rows.length} {loading ? "loading…" : "events"}
        </span>
      </h2>

      <div className="form row" style={{ marginBottom: 12 }}>
        <label className="toggle" style={{ alignItems: "center" }}>
          <span className="muted" style={{ fontSize: 12 }}>Action</span>
          <input
            list="audit-actions"
            value={action}
            onChange={(e) => setAction(e.target.value)}
            placeholder="any"
            style={{ minWidth: 180 }}
          />
          <datalist id="audit-actions">
            {actionOptions.map((a) => (
              <option key={a} value={a} />
            ))}
          </datalist>
        </label>

        <label className="toggle" style={{ alignItems: "center" }}>
          <span className="muted" style={{ fontSize: 12 }}>User ID</span>
          <input
            value={userId}
            onChange={(e) => setUserId(e.target.value)}
            placeholder="any"
            style={{ minWidth: 220, fontFamily: "ui-monospace, monospace" }}
          />
        </label>

        <label className="toggle" style={{ alignItems: "center" }}>
          <span className="muted" style={{ fontSize: 12 }}>Since</span>
          <select value={since} onChange={(e) => setSince(e.target.value as typeof since)}>
            <option value="1h">last 1h</option>
            <option value="24h">last 24h</option>
            <option value="7d">last 7d</option>
            <option value="all">all time</option>
          </select>
        </label>

        <button
          className="btn ghost small"
          onClick={() => {
            setAction("");
            setUserId("");
            setSince("24h");
          }}
        >
          Clear
        </button>
      </div>

      {error && <div className="error">{error}</div>}

      <table>
        <thead>
          <tr>
            <th style={{ width: 170 }}>Time</th>
            <th style={{ width: 220 }}>Actor</th>
            <th style={{ width: 180 }}>Action</th>
            <th style={{ width: 220 }}>Target</th>
            <th>Detail</th>
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 && !loading && (
            <tr>
              <td colSpan={5} className="empty">
                No audit events yet. State-changing actions (key create, login, …) appear here.
              </td>
            </tr>
          )}
          {rows.map((r) => (
            <tr key={r.id}>
              <td title={r.created_at}>
                {formatTime(r.created_at)}
              </td>
              <td style={{ fontFamily: "ui-monospace, monospace", fontSize: 12 }}>
                {r.actor || "system"}
              </td>
              <td>
                <span className="tag">{r.action}</span>
              </td>
              <td
                style={{
                  fontFamily: "ui-monospace, monospace",
                  fontSize: 12,
                  color: "var(--muted)",
                  maxWidth: 220,
                  overflow: "hidden",
                  textOverflow: "ellipsis",
                  whiteSpace: "nowrap",
                }}
                title={r.target_id}
              >
                {r.target_id || "-"}
              </td>
              <td style={{ fontSize: 12, color: "var(--muted)" }}>
                {r.detail || <span className="muted">-</span>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}

// formatTime shows HH:MM:SS in local time with a date prefix when older than
// 24h, which keeps the table scannable but still readable for old rows.
function formatTime(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  const now = new Date();
  const sameDay =
    d.getFullYear() === now.getFullYear() &&
    d.getMonth() === now.getMonth() &&
    d.getDate() === now.getDate();
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  const ss = String(d.getSeconds()).padStart(2, "0");
  if (sameDay) return `${hh}:${mm}:${ss}`;
  const yyyy = d.getFullYear();
  const mo = String(d.getMonth() + 1).padStart(2, "0");
  const dd = String(d.getDate()).padStart(2, "0");
  return `${yyyy}-${mo}-${dd} ${hh}:${mm}:${ss}`;
}
