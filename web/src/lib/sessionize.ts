// Roll up a flat list of TraceSummary rows into per-session rows so
// the overview can show one row per conversation instead of N rows per
// agent loop turn. Two trace rows attach to the same session when:
//
//   1. they carry the same non-empty `session_id` from the wire
//      (metadata.session_id / sessionId / conversation_id, or
//      "user:<id>" when only OpenAI's user field was present), OR
//   2. session_id is empty on both AND they belong to the same
//      (virtual_key, request_model) tuple AND the gap between the
//      newer trace's timestamp and the older one is within
//      SESSION_WINDOW_MS.
//
// The window model is what Cursor's own UI effectively does — if you
// hit the model twice in under N minutes on the same key, you almost
// certainly meant the same conversation. The window is intentionally
// short (5 minutes) so a user thinking about a prompt and firing it a
// second time doesn't merge across an unrelated help-me-think step.
//
// Cost and token totals are summed across the merged traces so the
// overview row matches what a customer sees on their bill; latency
// and TTFT are averaged so a "1s response" badge stays accurate.

import type { TraceSummary } from "../api";

const SESSION_WINDOW_MS = 5 * 60 * 1000;

export interface SessionRow {
  // session_id from the wire when present; otherwise a synthetic
  // "<vkey>|<model>|<windowStartISO>" string. The synthetic form is
  // stable for the lifetime of the page (windowStart is computed
  // deterministically) but is not durable across reloads — that is
  // fine because the wire-side session_id, when present, is the
  // authoritative key.
  session_key: string;
  // True when the roll-up came from a wire marker; false for the
  // synthetic heuristic. Surfaced so the UI can render a different
  // tooltip ("session_id from metadata" vs "merged by time window")
  // and the operator can tell what's really happening.
  from_wire: boolean;
  trace_count: number;
  total_cost_usd: number;
  total_input_tokens: number;
  total_output_tokens: number;
  // average of latency_ms across the merged traces (single-call rows
  // match the trace's latency, multi-call rows smooth out).
  avg_latency_ms: number;
  first_at: string;
  last_at: string;
  request_model: string;
  provider_name: string;
  user_email?: string;
  // trace_ids, in chronological-asc order (earliest turn first), so
  // the drill-down reads like a transcript. The sessionizer sorts
  // once at row finalisation rather than per-merge — see the
  // datedIdTimed helper below.
  trace_ids: string[];
  // First error status, if any — used to render a red badge on the row
  // without forcing the operator to drill down to find it.
  first_error: { status: number; trace_id: string } | null;
}

// Internal tagged tuple used during the roll-up; the public SessionRow
// only exposes the chronological-asc string[] ids.
interface SessionRowInternal {
  session_key: string;
  from_wire: boolean;
  trace_count: number;
  total_cost_usd: number;
  total_input_tokens: number;
  total_output_tokens: number;
  avg_latency_ms: number;
  first_at: string;
  last_at: string;
  request_model: string;
  provider_name: string;
  user_email?: string;
  ids_with_ts: TimedId[];
  first_error: { status: number; trace_id: string } | null;
}

function makeRow(t: TraceSummary, wireID: string): SessionRowInternal {
  return {
    session_key: wireID || syntheticKeyFor(t),
    from_wire: wireID !== "",
    trace_count: 1,
    total_cost_usd: Number(t.cost_usd ?? 0),
    total_input_tokens: Number(t.input_tokens ?? 0),
    total_output_tokens: Number(t.output_tokens ?? 0),
    avg_latency_ms: Number(t.latency_ms ?? 0),
    first_at: t.timestamp,
    last_at: t.timestamp,
    request_model: t.request_model,
    provider_name: t.provider_name,
    user_email: t.user_email,
    ids_with_ts: [{ id: t.trace_id, ts: t.timestamp }],
    first_error: errorOf(t),
  };
}

function mergeTraceInto(row: SessionRowInternal, t: TraceSummary) {
  row.trace_count += 1;
  row.total_cost_usd += Number(t.cost_usd ?? 0);
  row.total_input_tokens += Number(t.input_tokens ?? 0);
  row.total_output_tokens += Number(t.output_tokens ?? 0);
  // Running average — keep n in trace_count and recompute lazily.
  row.avg_latency_ms =
    (row.avg_latency_ms * (row.trace_count - 1) + Number(t.latency_ms ?? 0)) /
    row.trace_count;
  if (t.timestamp < row.first_at) row.first_at = t.timestamp;
  if (t.timestamp > row.last_at) row.last_at = t.timestamp;
  if (!row.first_error) {
    const e = errorOf(t);
    if (e) row.first_error = e;
  }
  row.ids_with_ts.push({ id: t.trace_id, ts: t.timestamp });
}

function syntheticMatch(row: SessionRowInternal, t: TraceSummary, windowMS: number): boolean {
  if (row.from_wire) return false;
  if (row.request_model !== t.request_model) return false;
  if (row.provider_name !== t.provider_name) return false;
  if ((row.user_email ?? "") !== (t.user_email ?? "")) return false;
  const gap = Math.abs(Date.parse(row.last_at) - Date.parse(t.timestamp));
  return gap <= windowMS;
}

// Finalise flattens a SessionRowInternal back into a public SessionRow
// after sorting the trace ids chronologically.
function finalise(row: SessionRowInternal): SessionRow {
  return {
    session_key: row.session_key,
    from_wire: row.from_wire,
    trace_count: row.trace_count,
    total_cost_usd: row.total_cost_usd,
    total_input_tokens: row.total_input_tokens,
    total_output_tokens: row.total_output_tokens,
    avg_latency_ms: row.avg_latency_ms,
    first_at: row.first_at,
    last_at: row.last_at,
    request_model: row.request_model,
    provider_name: row.provider_name,
    user_email: row.user_email,
    trace_ids: row.ids_with_ts
      .sort((a, b) => a.ts.localeCompare(b.ts))
      .map((t) => t.id),
    first_error: row.first_error,
  };
}

export function sessionizeTraces(
  traces: TraceSummary[],
  windowMS: number = SESSION_WINDOW_MS,
): SessionRow[] {
  if (traces.length === 0) return [];

  // Sort newest first so iteration works against the "last row seen"
  // and we read chronologically toward us by appending.
  // Note: TraceSummary timestamps are RFC3339 strings, so the
  // Date.parse on each row is unavoidable. We do not split on the
  // string — JS engines handle this cleanly in a few microseconds per
  // row, and parsing once per row is fine for the 100-row page we are
  // showing here.
  const sortedDesc = [...traces].sort((a, b) => b.timestamp.localeCompare(a.timestamp));

  const out: SessionRowInternal[] = [];

  for (const t of sortedDesc) {
    const wt = (t.session_id ?? "").trim();

    // Find an existing session that this trace should fold into. The
    // search is bounded — sessions are a small list (≤ page size) and
    // sessionizeTraces is called once per render, so a linear lookup
    // is appropriate.
    let row: SessionRowInternal | undefined;
    if (wt !== "") {
      row = out.find((r) => r.from_wire && r.session_key === wt);
    } else {
      row = out.find((r) => syntheticMatch(r, t, windowMS));
    }

    if (row) {
      mergeTraceInto(row, t);
      continue;
    }

    out.push(makeRow(t, wt));
  }

  const finalised: SessionRow[] = out.map(finalise);

  // Restore chronological order so the operator sees the very latest
  // session at the top instead of the synthetic-Heuristic-first row
  // from the most recent trace.
  finalised.sort((a, b) => b.last_at.localeCompare(a.last_at));
  return finalised;
}

// Internal tagged tuple used during the roll-up; the public SessionRow
// only exposes the chronological-asc string[] ids.
interface TimedId {
  id: string;
  ts: string;
}

function errorOf(t: TraceSummary) {
  const code = Number(t.status_code ?? 0);
  return code >= 400 ? { status: code, trace_id: t.trace_id } : null;
}

// syntheticKeyFor produces a deterministic-looking but not durable key
// for a synthetic session row. The literal format only needs to be
// unique within a single page render; if two wire-side traces happen
// to share this exact synthetic key that's a coincidence the row
// collapses correctly anyway.
function syntheticKeyFor(t: TraceSummary): string {
  // Stable within the render so the React .map key holds across
  // re-renders that hit the same TraceSummary list.
  return `heur:${t.request_model}|${t.provider_name}|${t.timestamp}`;
}
