// API client + types for the Nexus console.

export interface Stats {
  total_requests: number;
  error_rate: number;
  avg_latency_ms: number;
  p95_latency_ms: number;
  // total_tokens is the prompt + completion aggregate, kept for
  // backwards compatibility with clients that read it as a single
  // number. New code should use total_input_tokens / total_output_tokens
  // so the dashboard can show Prompt / Completion as separate cards.
  total_tokens: number;
  total_input_tokens: number;
  total_output_tokens: number;
  total_cost_usd: number;
  cache_hits: number;
  cache_hit_rate: number;
  guardrail_events: number;
}

export interface TraceSummary {
  trace_id: string;
  timestamp: string;
  provider_name: string;
  request_model: string;
  input_tokens: number;
  output_tokens: number;
  latency_ms: number;
  ttft_ms: number;
  cost_usd: number;
  status_code: number;
  streamed: number;
  finish_reason: string;
  cache_hit: number;
  guardrail_action: string;
  credential_source: string;
  user_id?: string;
  user_email?: string;
  // session_id is the per-conversation marker the gateway extracted
  // from metadata.session_id / sessionId / conversation_id on the
  // request, or "user:<id>" when only the OpenAI user field was
  // present. Empty when none of those were on the wire, which is the
  // common case — no client we serve sends one today, which is why
  // turn_id exists.
  session_id?: string;
  // turn_id groups the calls an agent made answering one user question.
  // Derived gateway-side from the request payload rather than read off
  // the wire; empty on traces recorded before the column existed. The
  // overview groups on this and drills down with fetchTraces({ turn }).
  turn_id?: string;
  // total_tokens is the prompt + completion total the gateway
  // recorded (matches gen_ai.usage.total_tokens). Surfaced on the
  // Recent sessions row so the operator can size a conversation
  // without expanding it; the per-turn drill-down splits it into
  // input / output columns.
  total_tokens?: number;
  // response_model is the model that actually served the response,
  // which may differ from request_model when routing aliases or
  // fallbacks are in play (e.g. claude-opus-latest dispatched
  // through The Grid surfaces as `anthropic/claude-opus-5` here).
  // Surfaced on the Recent sessions row alongside request_model so
  // multi-vendor fan-outs are visible even when the credential's
  // provider_name only tells us which Nexus vkey answered.
  response_model?: string;
}

// TraceCursor is the server-issued "what to fetch next" handle. Empty
// `before` means start at "now"; empty `since` means the floor is the
// table TTL (90 days). Both RFC3339Nano UTC strings; the client passes
// them unchanged back into fetchTraces.
export interface TraceCursor {
  before: string;
  since: string;
}

// TracePage is one cursor-paged, server-filter-narrowed view of
// /api/traces. Items that match all of {before/since window, status,
// provider, q} come back here; an empty `next_cursor` means there is
// no further page in this window under the current filter set.
export interface TracePage {
  items: TraceSummary[];
  next_cursor: TraceCursor;
}

// TraceQuery describes one request to the trace listing endpoint. All
// fields are optional; omitted means "no filter" / "newest first" /
// "page size 100".
export interface TraceQuery {
  before?: string;
  since?: string;
  status?: "ok" | "err" | "";
  provider?: string;
  q?: string;
  limit?: number;
  // turn narrows the page to a single agent turn's calls. Exact match on
  // turn_id, used by the overview's expand-a-row drill-down.
  turn?: string;
}

// TurnSummary is one agent turn — a user question plus every model call
// made while answering it — as returned by /api/turns. trace_count of 1
// means the agent answered in a single call.
export interface TurnSummary {
  turn_id: string;
  first_at: string;
  last_at: string;
  trace_count: number;
  provider_name: string;
  request_model: string;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  cost_usd: number;
  // latency_ms is the summed wall time of the turn's calls, which is what
  // the caller actually waited through on a sequential agent loop.
  latency_ms: number;
  // status_code is the worst status in the turn, so one failed call in an
  // otherwise-green loop still surfaces.
  status_code: number;
  user_id?: string;
  user_email?: string;
}

export interface User {
  id: string;
  org_id: string;
  email: string;
  role: string;
  enforce_limits: boolean;
  created_at: string;
  onboarded_at?: string; // v1.1 — set after first successful /api/me/credentials create
}

export interface CredentialModels {
  chat?: string[];
  embed?: string[];
}

export interface Credential {
  id: string;
  provider: string;
  name: string;
  base_url?: string;
  models?: CredentialModels;
  secret_last4: string;
  enabled: boolean;
  created_at: string;
  rotated_at?: string;
}

export interface VirtualKey {
  id: string;
  name: string;
  key_prefix: string;
  key_last4: string;
  allowed_models: string[];
  rpm_limit: number;
  monthly_budget_usd: number;
  min_quality_score: number;
  revoked: boolean;
  created_at: string;
}

export interface RoutingModel {
  model: string;
  quality: number;
  quality_samples: number;
  pass_rate: number;
  safety_pass_rate: number;
  safety_samples: number;
  avg_latency_ms: number;
  avg_cost_usd: number;
  samples: number;
  eff_quality: number;
}

export async function fetchStats(window = "1h"): Promise<Stats> {
  // fetchStats is read by Overview before RequireAuth resolves. When the
  // caller is unauthenticated the server returns 401 with a JSON error body
  // that does NOT match the Stats shape; if we hand that to the dashboard it
  // will call `.toLocaleString()` on undefined fields and crash the SPA.
  // Return an all-zero Stats stub instead so the UI renders the "no data"
  // aesthetic rather than unmounting.
  const res = await fetch(`/api/stats?window=${window}`);
  if (!res.ok) {
    return ZERO_STATS;
  }
  const data = await res.json();
  return sanitizeStats(data);
}

// ProviderStat mirrors observability.ProviderStat shapes returned by
// GET /api/stats/providers. The "cost %" derived column is computed in the
// widget itself rather than carried over the wire so the server stays
// simple and the rendering tier can decide how to scale (log scale,
// top-N truncation, etc.).
export interface ProviderStat {
  provider: string;
  requests: number;
  cost_usd: number;
  input_tokens: number;
  output_tokens: number;
  avg_latency_ms: number;
  cache_hits: number;
}

// fetchProviderStats reads /api/stats/providers with the same error-shape
// hygiene as fetchStats: pre-auth Overview paints the spend widget with a
// 30-second cadence and a 401 body cannot be safely cast to ProviderStat —
// the spend bars would crash on `cost_usd.toFixed(2)` over undefined. The
// 30-second in-process cache on the server means repeated dashboard tabs
// that load on the same poll cadence are not double-charged.
export async function fetchProviderStats(window = "1h"): Promise<ProviderStat[]> {
  const res = await fetch(`/api/stats/providers?window=${window}`);
  if (!res.ok) {
    return [];
  }
  const data = await res.json();
  if (!Array.isArray(data)) return [];
  return data
    .filter((d): d is ProviderStat => d && typeof d === "object" && typeof d.provider === "string")
    .map((d) => ({
      provider: d.provider,
      requests: Number(d.requests ?? 0),
      cost_usd: Number(d.cost_usd ?? 0),
      input_tokens: Number(d.input_tokens ?? 0),
      output_tokens: Number(d.output_tokens ?? 0),
      avg_latency_ms: Number(d.avg_latency_ms ?? 0),
      cache_hits: Number(d.cache_hits ?? 0),
    }));
}

function sanitizeStats(data: Partial<Stats> | undefined | null): Stats {
  if (!data || typeof data !== "object") return ZERO_STATS;
  const safe = (v: unknown, fallback: number) =>
    typeof v === "number" && Number.isFinite(v) ? v : fallback;
  return {
    total_requests: safe(data.total_requests, 0),
    error_rate: safe(data.error_rate, 0),
    avg_latency_ms: safe(data.avg_latency_ms, 0),
    p95_latency_ms: safe(data.p95_latency_ms, 0),
    total_tokens: safe(data.total_tokens, 0),
    total_input_tokens: safe(data.total_input_tokens, 0),
    total_output_tokens: safe(data.total_output_tokens, 0),
    total_cost_usd: safe(data.total_cost_usd, 0),
    cache_hits: safe(data.cache_hits, 0),
    cache_hit_rate: safe(data.cache_hit_rate, 0),
    guardrail_events: safe(data.guardrail_events, 0),
  };
}

const ZERO_STATS: Stats = {
  total_requests: 0,
  error_rate: 0,
  avg_latency_ms: 0,
  p95_latency_ms: 0,
  total_tokens: 0,
  total_input_tokens: 0,
  total_output_tokens: 0,
  total_cost_usd: 0,
  cache_hits: 0,
  cache_hit_rate: 0,
  guardrail_events: 0,
};

export async function fetchTraces(query: TraceQuery = {}): Promise<TracePage> {
  // Build the URL manually so we can attach the cursor fields and the
  // filter fields without a third-party query-string dependency and so
  // the wire shape stays an exact mirror of /api/traces' contract.
  const params = new URLSearchParams();
  if (query.before) params.set("before", query.before);
  if (query.since) params.set("since", query.since);
  if (query.status) params.set("status", query.status);
  if (query.provider) params.set("provider", query.provider);
  if (query.q) params.set("q", query.q);
  if (query.turn) params.set("turn", query.turn);
  if (typeof query.limit === "number") params.set("limit", String(query.limit));
  const qs = params.toString();
  const url = qs ? `/api/traces?${qs}` : `/api/traces`;
  const res = await fetch(url);
  if (!res.ok) {
    return { items: [], next_cursor: { before: "", since: "" } };
  }
  const data = await res.json();
  // Server envelope shape OR bare array shape (defensive — old binary
  // versions of the gateway might still emit the bare array). The Traces
  // page already handles both during the rolling deploy.
  if (Array.isArray(data)) {
    return { items: data as TraceSummary[], next_cursor: { before: "", since: "" } };
  }
  if (data && Array.isArray(data.items)) {
    return data as TracePage;
  }
  return { items: [], next_cursor: { before: "", since: "" } };
}

// fetchTurns returns the grouped overview rows. Unlike /api/traces this is
// window-bounded rather than cursor-paged: a GROUP BY has no stable cursor,
// and the overview only ever shows the most recent handful.
export async function fetchTurns(
  query: { window?: string; limit?: number } = {},
): Promise<TurnSummary[]> {
  const params = new URLSearchParams();
  if (query.window) params.set("window", query.window);
  if (typeof query.limit === "number") params.set("limit", String(query.limit));
  const qs = params.toString();
  const res = await fetch(qs ? `/api/turns?${qs}` : `/api/turns`);
  if (!res.ok) return [];
  const data = await res.json();
  return Array.isArray(data) ? (data as TurnSummary[]) : [];
}

export async function fetchRouting(): Promise<RoutingModel[]> {
  const res = await fetch(`/api/routing`);
  const data = await res.json();
  return Array.isArray(data) ? data : [];
}

export interface EvalConfigSnapshot {
  eval_enabled: boolean;
  routing_enabled: boolean;
  score_store: string;
  trace_store: string;
  score_persisted: boolean;
  routing_stats_store: string;
  eval: {
    pii_enabled: boolean;
    completeness_enabled: boolean;
    sample_rate: number;
    workers: number;
    judge: {
      enabled: boolean;
      base_url: string;
      model: string;
      api_key_set: boolean;
    };
    remote: {
      enabled: boolean;
      url: string;
      metrics: string[];
      timeout: string;
    };
  };
  routing: {
    weights: { quality?: number; cost?: number; latency?: number };
    window: string;
    refresh: string;
    groups: Record<string, string[]>;
    groups_spec: string;
    load_balance: boolean;
    // v1beta: external benchmark blend. bench_enabled reflects an
    // effective state (Postgres stats and a positive weight); the
    // raw values come straight from env vars so the console can show
    // operators exactly what is wired without needing a separate
    // "advanced settings" page.
    bench_enabled: boolean;
    bench_weight: number;
    bench_decay: string;
  };
  // True when NEXUS_EVAL_PLUGIN_ONLY is set on the pod that issued
  // this snapshot. Console surfaces a banner above the eval table.
  plugin_only: boolean;
  purge_legacy_profiles_on_boot: boolean;
  restart_required: string[];
}

export async function fetchEvalConfig(): Promise<EvalConfigSnapshot> {
  // Same defense-in-depth as fetchStats: an unauthenticated 401 body cannot
  // be safely cast to EvalConfigSnapshot (deeply nested optional fields will
  // throw downstream). Return a minimal "router-only" snapshot so the dials
  // render in a disabled state rather than unmounting the React tree.
  const res = await fetch("/api/eval/config");
  if (!res.ok) return ZERO_EVAL;
  const data = await res.json().catch(() => ({}));
  return sanitizeEvalConfig(data);
}

export async function patchEvalConfig(patch: Record<string, unknown>): Promise<EvalConfigSnapshot> {
  const res = await fetch("/api/eval/config", {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  });
  if (!res.ok) {
    const data = await res.json().catch(() => ({}));
    throw new Error(data.error || `HTTP ${res.status}`);
  }
  return sanitizeEvalConfig(await res.json());
}

// ---------------------------------------------------------------------------
// Eval profiles (PR #137).
// ---------------------------------------------------------------------------

export type KeySource = "org" | "user" | "inline" | "builtin";
export type ProfileKind =
  | "heuristic_pii"
  | "heuristic_completeness"
  | "slm_judge"
  | "remote_eval";
export type EvalScope = "org" | "user";

export interface EvalProfileEndpoint {
  base_url?: string;
  model?: string;
  key_source?: KeySource;
  key_ref?: string;
}

export interface EvalProfile {
  id?: string;
  name?: string;
  kind?: ProfileKind;
  scope?: EvalScope;
  owner_user_id?: string;
  endpoint?: EvalProfileEndpoint;
  metrics?: string[];
  threshold?: number;
  sample_rate?: number;
  enabled?: boolean;
  created_at?: string;
  updated_at?: string;
}

export interface EvalProfilePatch {
  name?: string;
  kind?: ProfileKind;
  scope?: EvalScope;
  owner_user_id?: string;
  endpoint?: EvalProfileEndpoint;
  metrics?: string[];
  threshold?: number;
  sample_rate?: number;
  enabled?: boolean;
}

export interface EvalProfileListResponse {
  profiles: EvalProfile[];
}

async function jsonOrError<T>(res: Response): Promise<T> {
  if (!res.ok) {
    const data = await res.json().catch(() => ({}));
    throw new Error((data as { error?: string }).error || `HTTP ${res.status}`);
  }
  return (await res.json()) as T;
}

export async function fetchEvalProfiles(): Promise<EvalProfile[]> {
  const res = await fetch("/api/eval/profiles");
  if (!res.ok) return [];
  const data = await jsonOrError<EvalProfileListResponse>(res);
  return Array.isArray(data.profiles) ? data.profiles : [];
}

export async function createEvalProfile(p: EvalProfile): Promise<EvalProfile> {
  const res = await fetch("/api/eval/profiles", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(p),
  });
  return jsonOrError<EvalProfile>(res);
}

export async function patchEvalProfile(
  id: string,
  patch: EvalProfilePatch,
): Promise<EvalProfile> {
  const res = await fetch(`/api/eval/profiles/${id}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  });
  return jsonOrError<EvalProfile>(res);
}

export async function deleteEvalProfile(id: string): Promise<void> {
  const res = await fetch(`/api/eval/profiles/${id}`, { method: "DELETE" });
  if (!res.ok) {
    const data = await res.json().catch(() => ({}));
    throw new Error((data as { error?: string }).error || `HTTP ${res.status}`);
  }
}

// ---- Eval plugins (Phase B) ------------------------------------------

export interface EvalPluginRecord {
  id?: string;
  org_id?: string;
  name: string;
  spec_yaml: string;
  enabled: boolean;
  created_at?: string;
  updated_at?: string;
}

export interface EvalPluginListResponse {
  plugins: EvalPluginRecord[];
}

export async function fetchEvalPlugins(): Promise<EvalPluginRecord[]> {
  const res = await fetch("/api/eval/plugins");
  if (!res.ok) return [];
  const data = await jsonOrError<EvalPluginListResponse>(res);
  return Array.isArray(data.plugins) ? data.plugins : [];
}

export async function createEvalPlugin(
  body: Omit<EvalPluginRecord, "id">,
): Promise<EvalPluginRecord> {
  const res = await fetch("/api/eval/plugins", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  return jsonOrError<EvalPluginRecord>(res);
}

export async function patchEvalPlugin(
  id: string,
  patch: { spec_yaml?: string; enabled?: boolean },
): Promise<EvalPluginRecord> {
  const res = await fetch(`/api/eval/plugins/${id}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  });
  return jsonOrError<EvalPluginRecord>(res);
}

export async function deleteEvalPlugin(id: string): Promise<void> {
  const res = await fetch(`/api/eval/plugins/${id}`, { method: "DELETE" });
  if (!res.ok) {
    const data = await res.json().catch(() => ({}));
    throw new Error((data as { error?: string }).error || `HTTP ${res.status}`);
  }
}

export interface PluginTestResult {
  ok: boolean;
  message: string;
  /** Latency in milliseconds for the underlying PING/HEAD round-trip. Optional
   * because some adapters (e.g. a cold cache) may report a generic failure
   * without timing it. */
  latency_ms?: number;
}

export async function testEvalPlugin(ref: string): Promise<PluginTestResult> {
  // `ref` is whichever identity the operator is looking at — the
  // server route accepts both the canonical metadata.name (e.g.
  // "langfuse-judge") and the database row id. The form always
  // passes the name; the route's UUID-tolerant lookup keeps us safe
  // if a stale id leaks through from older code paths.
  const res = await fetch(`/api/eval/plugins/${encodeURIComponent(ref)}/test`, {
    method: "POST",
  });
  if (!res.ok) {
    // Read the body once. The body of a 5xx response can be:
    //   1. a typed JSON `{ ok, message, ... }` we authored server-side,
    //   2. a different JSON shape from a legacy / proxy layer,
    //   3. an HTML page from a CDN / ingress interception,
    //   4. just empty.
    // We must read the bytes exactly once per Response, so read text
    // first and optionally attempt a JSON parse on the result.
    let rawSnippet = "";
    try {
      rawSnippet = await res.text();
    } catch {
      rawSnippet = "";
    }
    let data: Record<string, unknown> = {};
    if (rawSnippet) {
      try {
        data = JSON.parse(rawSnippet) as Record<string, unknown>;
      } catch {
        data = {};
      }
    }
    const message =
      (data as { message?: string }).message ||
      (data as { error?: string }).error ||
      extractBodyHint(rawSnippet, res.status, res.headers.get("content-type")) ||
      `Backend HTTP ${res.status}`;
    return { ok: false, message };
  }
  return jsonOrError<PluginTestResult>(res);
}

/**
 * extractBodyHint is the last-resort fallback when the server
 * reply did not match the typed PluginTestResult envelope. We slice
 * the first 120 characters of whatever body came back so the row
 * shows something actionable ("Backend HTTP 502 returned an
 * unexpected body (nginx 502 Bad Gateway)…") instead of leaving the
 * operator hunting in browser devtools.
 */
function extractBodyHint(
  raw: string,
  status: number,
  contentType: string | null,
): string | null {
  if (!raw) {
    if (status >= 500) {
      return `Backend HTTP ${status} returned no body — likely an upstream proxy page.`;
    }
    return null;
  }
  const oneLine = raw.replace(/\s+/g, " ").trim();
  if (oneLine.length < 3) return null;
  const ct = (contentType ?? "").toLowerCase();
  const prefix = ct.includes("html")
    ? `Backend HTTP ${status} returned HTML — auth or ingress likely intercepted the request (`
    : `Backend HTTP ${status} returned non-JSON body (`;
  return `${prefix}${oneLine.slice(0, 120)})`;
}

/** Send a synthetic webhook payload to a plugin's inbox. Backed by the
 * `/api/eval/plugins/{name}/webhook` console endpoint. Useful to verify
 * that an external vendor's webhook can actually reach Nexus after the
 * operator pasted their secretRef in. */
export async function pingEvalPluginWebhook(
  name: string,
  body: unknown,
): Promise<{ ok: boolean; accepted: number; message?: string }> {
  const res = await fetch(`/api/eval/plugins/${name}/webhook`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  return jsonOrError(res);
}

function sanitizeEvalConfig(raw: unknown): EvalConfigSnapshot {
  const safe = (v: unknown, fallback: number) =>
    typeof v === "number" && Number.isFinite(v) ? v : fallback;
  const safeBool = (v: unknown, fallback: boolean) =>
    typeof v === "boolean" ? v : fallback;
  const safeStr = (v: unknown, fallback: string) =>
    typeof v === "string" ? v : fallback;
  const safeStrArr = (v: unknown): string[] =>
    Array.isArray(v) ? v.filter((s): s is string => typeof s === "string") : [];

  const evalBlock = raw && typeof raw === "object" && "eval" in (raw as Record<string, unknown>)
    ? (raw as Record<string, unknown>).eval as Record<string, unknown>
    : {};
  const judge = (evalBlock.judge as Record<string, unknown>) ?? {};
  const remote = (evalBlock.remote as Record<string, unknown>) ?? {};
  const weightsRaw =
    raw && typeof raw === "object" && "routing" in (raw as Record<string, unknown>)
      ? ((raw as Record<string, unknown>).routing as Record<string, unknown>)?.weights as
          | Record<string, unknown>
          | undefined
      : undefined;

  return {
    eval_enabled: safeBool((raw as any)?.eval_enabled, false),
    routing_enabled: safeBool((raw as any)?.routing_enabled, false),
    score_store: safeStr((raw as any)?.score_store, ""),
    trace_store: safeStr((raw as any)?.trace_store, ""),
    score_persisted: safeBool((raw as any)?.score_persisted, false),
    routing_stats_store: safeStr((raw as any)?.routing_stats_store, ""),
    eval: {
      pii_enabled: safeBool(evalBlock.pii_enabled, false),
      completeness_enabled: safeBool(evalBlock.completeness_enabled, false),
      sample_rate: safe(evalBlock.sample_rate, 0),
      workers: safe(evalBlock.workers, 0),
      judge: {
        enabled: safeBool(judge.enabled, false),
        base_url: safeStr(judge.base_url, ""),
        model: safeStr(judge.model, ""),
        api_key_set: safeBool(judge.api_key_set, false),
      },
      remote: {
        enabled: safeBool(remote.enabled, false),
        url: safeStr(remote.url, ""),
        metrics: safeStrArr(remote.metrics),
        timeout: safeStr(remote.timeout, "30s"),
      },
    },
    routing: {
      weights: {
        quality: safe(weightsRaw?.quality, 0.6),
        cost: safe(weightsRaw?.cost, 0.2),
        latency: safe(weightsRaw?.latency, 0.2),
      },
      window: safeStr((raw as any)?.routing?.window, "1h"),
      refresh: safeStr((raw as any)?.routing?.refresh, "60s"),
      groups: ((raw as any)?.routing?.groups && typeof (raw as any).routing.groups === "object")
        ? ((raw as any).routing.groups as Record<string, string[]>)
        : {},
      groups_spec: safeStr((raw as any)?.routing?.groups_spec, ""),
      load_balance: safeBool((raw as any)?.routing?.load_balance, false),
      bench_enabled: safeBool((raw as any)?.routing?.bench_enabled, false),
      bench_weight: safe((raw as any)?.routing?.bench_weight, 0),
      bench_decay: safeStr((raw as any)?.routing?.bench_decay, ""),
    },
    plugin_only: safeBool((raw as any)?.plugin_only, false),
    purge_legacy_profiles_on_boot: safeBool((raw as any)?.purge_legacy_profiles_on_boot, false),
    restart_required: safeStrArr((raw as any)?.restart_required),
  };
}

export const ZERO_EVAL: EvalConfigSnapshot = {
  eval_enabled: false,
  routing_enabled: false,
  score_store: "",
  trace_store: "",
  score_persisted: false,
  routing_stats_store: "",
  eval: {
    pii_enabled: false,
    completeness_enabled: false,
    sample_rate: 0,
    workers: 0,
    judge: { enabled: false, base_url: "", model: "", api_key_set: false },
    remote: { enabled: false, url: "", metrics: [], timeout: "30s" },
  },
  routing: {
    weights: { quality: 0, cost: 0, latency: 0 },
    window: "",
    refresh: "",
    groups: {},
    groups_spec: "",
    load_balance: false,
    bench_enabled: false,
    bench_weight: 0,
    bench_decay: "",
  },
  plugin_only: false,
  purge_legacy_profiles_on_boot: false,
  restart_required: [],
};

// --- Gateway /v1/models (open-source discovery) ---

export interface GatewayModel {
  id: string;
  owned_by: string;
}

export interface GatewayModelCatalog {
  chat: string[];
  embed: string[];
  user: {
    provider: string;
    models: string[];
    // Scope is the visibility class the Gateway registered this router
    // with (PR #132). Empty string is treated as "legacy / unknown" and
    // falls back to the historical "Public" badge. The console player
    // sees *only* routers that her caller is allowed to see — so the
    // badge here is mostly an honest label, not a privacy knob (server
    // already enforced it in PR #133).
    scope?: "public" | "org" | "user" | string;
    owner_id?: string;
  }[];
}

// fetchGatewayModels hits /v1/models and returns the union list, plus a
// breakdown of any user/<provider>/... entries so the Playground picker can
// offer them grouped. Tolerates a 401/404 when the gateway is not reachable
// from the console (e.g. users running the dashboard alone) by returning an
// empty catalog so the UI renders the small set of stock options instead of
// crashing.
export async function fetchGatewayModels(): Promise<GatewayModelCatalog> {
  // The console's Playground reads the same catalog the gateway exposes at
  // /v1/models, but /v1/models requires a virtual-key Authorization header
  // that the Nexus session cookie does not satisfy. The console therefore
  // fronts the request with /api/me/playground/catalog, which is gated on
  // the user's Nexus session instead. Falls back to /v1/models when that
  // endpoint is not available (e.g. older builds, single-host debug mode),
  // and finally returns an empty catalog so the page still renders.
  try {
    const session = await fetch(`/api/me/playground/catalog`);
    if (session.ok) {
      const data = await session.json();
      return normalizeCatalog(data);
    }
  } catch {
    /* fall through */
  }
  try {
    const res = await fetch(`/v1/models`, { credentials: "include" });
    if (!res.ok) return { chat: [], embed: [], user: [] };
    const data = await res.json();
    return normalizeCatalog(data);
  } catch {
    return { chat: [], embed: [], user: [] };
  }
}

function normalizeCatalog(data: any): GatewayModelCatalog {
  // First-party (gateway's /v1/models path) ships an OpenAI {data:[{id}]}
  // shape, while the console's /api/me/playground/catalog path returns the
  // pre-split {chat, embed, user, scope, owner_id} shape. Detect which one
  // arrived so the picker can group models by visibility scope.
  let chat: string[];
  if (Array.isArray(data?.chat)) {
    chat = data.chat.filter((s: unknown): s is string => typeof s === "string");
  } else if (Array.isArray(data?.data)) {
    chat = data.data
      .map((m: { id: string }) => m.id)
      .filter((id: unknown): id is string => typeof id === "string");
  } else {
    chat = [];
  }
  const embed = Array.isArray(data?.embeddings?.data)
    ? data.embeddings.data
        .map((m: { id: string }) => m.id)
        .filter((id: unknown): id is string => typeof id === "string")
    : Array.isArray(data?.embed)
      ? data.embed.filter((s: unknown): s is string => typeof s === "string")
      : [];
  const userRaw = Array.isArray(data?.user) ? data.user : [];
  const out: GatewayModelCatalog["user"] = [];
  for (const u of userRaw) {
    if (!u || typeof u.provider !== "string" || !Array.isArray(u.models)) continue;
    const models = u.models.filter((m: unknown): m is string => typeof m === "string");
    out.push({
      provider: u.provider,
      models,
      scope: typeof u.scope === "string" ? u.scope : undefined,
      owner_id: typeof u.owner_id === "string" ? u.owner_id : undefined,
    });
  }
  // /api/me/playground/catalog already splits providers; /v1/models does
  // not, so if we're on the older path (no user array, but entries live in
  // /data) re-derive the provider grouping so the picker renders scoped
  // badges for any /user/... entries that happen to be in the union.
  if (out.length === 0) {
    const legacy = collectUserModels(chat);
    for (const l of legacy) {
      out.push({
        provider: l.provider,
        models: l.models,
        scope: undefined,
        owner_id: undefined,
      });
    }
  }
  return { chat, embed, user: out };
}

// collectUserModels groups the catalog by the "user/<provider>/" schema
// introduced by user-definable OpenAI-compatible credentials. Anything not
// matching that prefix is left in chat to be shown as a flat list.
function collectUserModels(ids: string[]): { provider: string; models: string[] }[] {
  const map = new Map<string, string[]>();
  for (const id of ids) {
    if (!id.startsWith("user/")) continue;
    const rest = id.slice("user/".length);
    const slash = rest.indexOf("/");
    if (slash <= 0) continue;
    const provider = rest.slice(0, slash);
    const model = rest.slice(slash + 1);
    if (!model) continue;
    const list = map.get(provider) ?? [];
    list.push(model);
    map.set(provider, list);
  }
  return Array.from(map.entries()).map(([provider, models]) => ({ provider, models }));
}

// --- Auth / self-service (BYOK) ---

async function jsonOrThrow(res: Response) {
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `request failed (${res.status})`);
  return data;
}

export async function login(email: string, password: string): Promise<User> {
  const res = await fetch(`/api/auth/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ email, password }),
  });
  const data = await jsonOrThrow(res);
  return data.user as User;
}

export interface AuthConfig {
  signup_enabled: boolean;
  sso_enabled: boolean;
  sso_label: string;
  gateway_url?: string;
}

export async function fetchAuthConfig(): Promise<AuthConfig> {
  const res = await fetch(`/api/auth/config`);
  if (!res.ok) return { signup_enabled: false, sso_enabled: false, sso_label: "" };
  const data = await res.json();
  return {
    signup_enabled: !!data.signup_enabled,
    sso_enabled: !!data.sso_enabled,
    sso_label: data.sso_label || "",
    gateway_url: data.gateway_url || "",
  };
}

// startSSOLogin redirects the browser to /api/auth/sso/login, which kicks
// off the OIDC Authorization Code flow against the configured IdP
// (Keycloak, Authentik, ...). The server does the token exchange and
// creates a Nexus session, then bounces back to /.
export function startSSOLogin(): void {
  window.location.href = "/api/auth/sso/login";
}

export interface RegisterResult {
  user: User;
  virtual_key?: string;
  warnings?: string[];
}

export async function register(input: {
  email: string;
  password: string;
  provider?: string;
  provider_name?: string;
  provider_secret?: string;
  models?: CredentialModels;
  key_name?: string;
}): Promise<RegisterResult> {
  const res = await fetch(`/api/auth/register`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  return jsonOrThrow(res) as Promise<RegisterResult>;
}

export async function logout(): Promise<void> {
  await fetch(`/api/auth/logout`, { method: "POST" });
}

// fetchMe returns the current user, or null when not logged in.
export async function fetchMe(): Promise<User | null> {
  const res = await fetch(`/api/me`);
  if (res.status === 401) return null;
  return res.json();
}

export async function updateMe(enforce_limits: boolean): Promise<User> {
  const res = await fetch(`/api/me`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ enforce_limits }),
  });
  return jsonOrThrow(res) as Promise<User>;
}

export async function fetchMyCredentials(): Promise<Credential[]> {
  const res = await fetch(`/api/me/credentials`);
  const data = await res.json();
  return Array.isArray(data) ? data : [];
}

export async function createMyCredential(input: {
  provider: string;
  name: string;
  base_url?: string;
  secret: string;
  models?: CredentialModels;
}): Promise<Credential> {
  const res = await fetch(`/api/me/credentials`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  return jsonOrThrow(res) as Promise<Credential>;
}

export async function deleteMyCredential(id: string): Promise<void> {
  await jsonOrThrow(await fetch(`/api/me/credentials/${id}`, { method: "DELETE" }));
}

// Provider-aware pre-flight before commit.
//
// The Provider credentials screen calls this *before* the drawer is
// allowed to mutate state. The server's handler invokes a single
// free, read-only auth round-trip to the upstream; we surface the
// result in the UI so a stale or typo'd key never lands in the
// encrypted store. `provider_label` is the human-friendly name the
// drawer renders in its connection-status pill; `detected_provider`
// is non-empty only when the pasted secret's shape suggests a
// different provider than the dropdown the operator picked.
export interface PreflightResult {
  ok: boolean;
  provider: string;
  provider_label: string;
  status?: number;
  latency_ms?: number;
  message?: string;
  detected_provider?: string;
}

export async function preflightCredential(input: {
  provider: string;
  secret: string;
  base_url?: string;
}): Promise<PreflightResult> {
  const res = await fetch(`/api/me/credentials/preflight`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  return jsonOrError<PreflightResult>(res);
}

export async function fetchMyKeys(): Promise<VirtualKey[]> {
  const res = await fetch(`/api/me/keys`);
  const data = await res.json();
  return Array.isArray(data) ? data : [];
}

export async function createMyKey(name: string): Promise<{ key: VirtualKey; secret: string }> {
  const res = await fetch(`/api/me/keys`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name }),
  });
  return jsonOrThrow(res) as Promise<{ key: VirtualKey; secret: string }>;
}

// --- Admin: user management ---

export async function fetchUsers(): Promise<User[]> {
  const res = await fetch(`/api/users`);
  if (!res.ok) return [];
  const data = await res.json();
  return Array.isArray(data) ? data : [];
}

export async function createUser(input: {
  email: string;
  password: string;
  role: string;
}): Promise<User> {
  const res = await fetch(`/api/users`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  return jsonOrThrow(res) as Promise<User>;
}

export async function deleteUser(id: string): Promise<void> {
  await jsonOrThrow(await fetch(`/api/users/${id}`, { method: "DELETE" }));
}

// --- Admin: invite flow ---
//
// Admin issues an invite; the server returns the raw token + a
// fully-formed URL that the admin hands off to the invitee out of
// band. The URL root comes from `NEXUS_PUBLIC_BASE_URL`; if that is
// not set we fall back to `window.location.origin` so the admin gets
// a usable link even on bare local dev clusters.
export interface InviteIssued {
  id: string;
  org_id: string;
  email: string;
  role: string;
  created_by: string;
  created_at: string;
  expires_at: string;
  accepted_at?: string | null;
  revoked_at?: string | null;
  url?: string;
  token: string;
}

export interface InviteRow {
  id: string;
  org_id: string;
  email: string;
  role: string;
  created_by: string;
  created_at: string;
  expires_at: string;
  accepted_at?: string | null;
  revoked_at?: string | null;
  accepted_by?: string | null;
}

export async function createInvite(input: {
  email: string;
  role: string;
}): Promise<InviteIssued> {
  const res = await fetch(`/api/invites`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  const base = jsonOrThrow(res) as Promise<InviteIssued>;
  // Fallback compose: if the server left URL empty (PublicBaseURL
  // not set on the cluster), plug in this page's origin so the
  // admin still has a usable shareable link.
  return base.then((inv) => {
    if (!inv.url || !inv.url.startsWith("http")) {
      const origin =
        typeof window !== "undefined" ? window.location.origin : "";
      inv.url = origin ? `${origin}/invite/${inv.token}` : `/invite/${inv.token}`;
    }
    return inv;
  });
}

export async function listInvites(): Promise<InviteRow[]> {
  const res = await fetch(`/api/invites`);
  if (!res.ok) return [];
  const data = await res.json();
  return Array.isArray(data) ? (data as InviteRow[]) : [];
}

export async function revokeInvite(id: string): Promise<void> {
  await jsonOrThrow(await fetch(`/api/invites/${id}`, { method: "DELETE" }));
}

// --- Spend (per-day LLM cost) -----------------------------------------
//
// /api/me/spend/daily and /api/me/spend/daily/{day}/breakdown are
// user-scoped (the caller's own gateway_traces). Admins can also target
// any member via /api/users/{id}/spend/daily, passing "me" for the admin's
// own spend.

export interface DailySpendRow {
  day: string; // YYYY-MM-DD (server UTC)
  cost_usd: number;
  tokens: number;
  requests: number;
  cache_hits: number;
}

export interface DailySpendBreakdownRow {
  model: string; // request_model
  provider: string;
  // response_model is omitted on cache-hit and legacy rows where the
  // server-side response shape was not persisted; the UI treats "" as
  // "cache served" rather than a literal model name.
  response_model?: string;
  cost_usd: number;
  tokens: number;
  requests: number;
  cache_hits: number;
}

function sanitizeDailySpendRow(d: Partial<DailySpendRow> | undefined | null): DailySpendRow {
  const safe = (v: unknown, fb: number) =>
    typeof v === "number" && Number.isFinite(v) ? v : fb;
  return {
    day: typeof d?.day === "string" ? d!.day : "",
    cost_usd: safe(d?.cost_usd, 0),
    tokens: safe(d?.tokens, 0),
    requests: safe(d?.requests, 0),
    cache_hits: safe(d?.cache_hits, 0),
  };
}

function sanitizeDailySpendBreakdownRow(
  d: Partial<DailySpendBreakdownRow> | undefined | null,
): DailySpendBreakdownRow {
  const safe = (v: unknown, fb: number) =>
    typeof v === "number" && Number.isFinite(v) ? v : fb;
  return {
    model: typeof d?.model === "string" ? d!.model : "",
    provider: typeof d?.provider === "string" ? d!.provider : "",
    response_model:
      typeof d?.response_model === "string" && d!.response_model !== ""
        ? d!.response_model
        : undefined,
    cost_usd: safe(d?.cost_usd, 0),
    tokens: safe(d?.tokens, 0),
    requests: safe(d?.requests, 0),
    cache_hits: safe(d?.cache_hits, 0),
  };
}

export async function fetchMySpendDaily(days: number): Promise<DailySpendRow[]> {
  const res = await fetch(`/api/me/spend/daily?days=${encodeURIComponent(String(days))}`);
  const data = await res.json();
  if (!Array.isArray(data)) return [];
  return data.map((d) => sanitizeDailySpendRow(d as Partial<DailySpendRow>));
}

export async function fetchMySpendBreakdown(day: string): Promise<DailySpendBreakdownRow[]> {
  const res = await fetch(
    `/api/me/spend/daily/${encodeURIComponent(day)}/breakdown`,
  );
  const data = await res.json();
  if (!Array.isArray(data)) return [];
  return data.map((d) => sanitizeDailySpendBreakdownRow(d as Partial<DailySpendBreakdownRow>));
}

// "me" is a server-side alias for the admin caller's own ID, so the
// admin can opt into the same shape as a per-member lookup without
// resolving their own user ID in the page.
export async function fetchUserSpendDaily(
  userID: string,
  days: number,
): Promise<DailySpendRow[]> {
  const res = await fetch(
    `/api/users/${encodeURIComponent(userID)}/spend/daily?days=${encodeURIComponent(String(days))}`,
  );
  const data = await res.json();
  if (!Array.isArray(data)) return [];
  return data.map((d) => sanitizeDailySpendRow(d as Partial<DailySpendRow>));
}

export async function fetchUserSpendBreakdown(
  userID: string,
  day: string,
): Promise<DailySpendBreakdownRow[]> {
  const res = await fetch(
    `/api/users/${encodeURIComponent(userID)}/spend/daily/${encodeURIComponent(day)}/breakdown`,
  );
  const data = await res.json();
  if (!Array.isArray(data)) return [];
  return data.map((d) => sanitizeDailySpendBreakdownRow(d as Partial<DailySpendBreakdownRow>));
}

// DailySpendSummary is the hero-card rollup the Spend page renders in
// the page head. It bundles current-window totals (`current_*`) with
// the same-shape totals for the equal-length window immediately
// preceding the current one (`previous_*`), plus the savings-pct
// computed server-side from those two cost columns. has_previous is
// false for first-window users where the comparison would otherwise
// read as "infinite savings" against a zero baseline.
export interface DailySpendSummary {
  days: number;
  current_cost_usd: number;
  previous_cost_usd: number;
  delta_cost_usd: number;
  savings_pct: number;
  has_previous: boolean;
  current_tokens: number;
  previous_tokens: number;
  current_requests: number;
  previous_requests: number;
  current_cache_hits: number;
  previous_cache_hits: number;
}

function sanitizeDailySpendSummary(
  d: Partial<DailySpendSummary> | undefined | null,
): DailySpendSummary {
  const safe = (v: unknown, fb: number) =>
    typeof v === "number" && Number.isFinite(v) ? v : fb;
  return {
    days: typeof d?.days === "number" && d.days > 0 ? d.days : 30,
    current_cost_usd: safe(d?.current_cost_usd, 0),
    previous_cost_usd: safe(d?.previous_cost_usd, 0),
    delta_cost_usd: safe(d?.delta_cost_usd, 0),
    savings_pct: safe(d?.savings_pct, 0),
    has_previous: Boolean(d?.has_previous),
    current_tokens: safe(d?.current_tokens, 0),
    previous_tokens: safe(d?.previous_tokens, 0),
    current_requests: safe(d?.current_requests, 0),
    previous_requests: safe(d?.previous_requests, 0),
    current_cache_hits: safe(d?.current_cache_hits, 0),
    previous_cache_hits: safe(d?.previous_cache_hits, 0),
  };
}

function emptySummary(days: number): DailySpendSummary {
  return {
    days,
    current_cost_usd: 0,
    previous_cost_usd: 0,
    delta_cost_usd: 0,
    savings_pct: 0,
    has_previous: false,
    current_tokens: 0,
    previous_tokens: 0,
    current_requests: 0,
    previous_requests: 0,
    current_cache_hits: 0,
    previous_cache_hits: 0,
  };
}

export async function fetchMySpendSummary(days: number): Promise<DailySpendSummary> {
  const res = await fetch(
    `/api/me/spend/summary?days=${encodeURIComponent(String(days))}`,
  );
  if (!res.ok) return emptySummary(days);
  const data = await res.json();
  if (!data || typeof data !== "object") return emptySummary(days);
  return sanitizeDailySpendSummary(data as Partial<DailySpendSummary>);
}

export async function fetchUserSpendSummary(
  userID: string,
  days: number,
): Promise<DailySpendSummary> {
  const res = await fetch(
    `/api/users/${encodeURIComponent(userID)}/spend/summary?days=${encodeURIComponent(String(days))}`,
  );
  if (!res.ok) return emptySummary(days);
  const data = await res.json();
  if (!data || typeof data !== "object") return emptySummary(days);
  return sanitizeDailySpendSummary(data as Partial<DailySpendSummary>);
}

// --- Admin: audit log (v1.1) ---

export interface AuditEntry {
  id: number;
  org_id: string;
  actor: string; // user_id of the caller; "system" for non-user actions
  action: string; // e.g. "vkey.create", "credential.rotate", "auth.login"
  target_id: string;
  detail: string;
  created_at: string;
}

export interface AuditQuery {
  limit?: number;
  action?: string;
  user_id?: string;
  since?: string; // RFC3339 or a duration like "24h"
}

export async function fetchAudit(q: AuditQuery = {}): Promise<AuditEntry[]> {
  const params = new URLSearchParams();
  if (q.limit != null) params.set("limit", String(q.limit));
  if (q.action) params.set("action", q.action);
  if (q.user_id) params.set("user_id", q.user_id);
  if (q.since) params.set("since", q.since);
  const qs = params.toString();
  const res = await fetch(`/api/audit${qs ? "?" + qs : ""}`);
  if (!res.ok) return [];
  const data = await res.json();
  return Array.isArray(data) ? data : [];
}

// PluginKeysState is the GET response shape for
// /api/eval/plugins/{name}/keys: which keys exist (by manifest),
// which of them have a value, and whether anything is configured at
// all. Values themselves are NEVER returned — the UI only sees
// names + booleans, so DevTools and log lines stay safe to share.
export interface PluginKeysState {
  plugin: string;
  configured: boolean;
  required_key_names?: string[];
  keys: PluginKeysEntry[];
}

export interface PluginKeysEntry {
  name: string;
  set: boolean;
}

// PluginKeysWriteResult mirrors the response of PUT /keys.
export type PluginKeysWriteResult = PluginKeysState;

// PluginKeysError is the typed failure shape returned by the server
// with HTTP 4xx/5xx (e.g. { ok: false, message: "...not wired..." }).
export interface PluginKeysErrorEnvelope {
  ok?: false;
  message?: string;
  error?: string;
}

export async function fetchPluginKeys(name: string): Promise<PluginKeysState> {
  const res = await fetch(`/api/eval/plugins/${encodeURIComponent(name)}/keys`, {
    credentials: "same-origin",
  });
  // 503 / 404 / other: shape may be the typed { ok, message } envelope
  // or go-chi default { error }. We map both into the same Error text.
  let parsed: unknown = null;
  try {
    parsed = await res.json();
  } catch {
    parsed = null;
  }
  if (!res.ok) {
    const env = parsed as PluginKeysErrorEnvelope | null;
    throw new Error(
      env?.message || env?.error || `HTTP ${res.status} on GET /keys`,
    );
  }
  return parsed as PluginKeysState;
}

export async function putPluginKeys(
  name: string,
  keys: Record<string, string>,
): Promise<PluginKeysWriteResult> {
  const res = await fetch(`/api/eval/plugins/${encodeURIComponent(name)}/keys`, {
    method: "PUT",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ keys }),
  });
  if (!res.ok) {
    let env: PluginKeysErrorEnvelope | null = null;
    try {
      env = (await res.json()) as PluginKeysErrorEnvelope;
    } catch {
      env = null;
    }
    throw new Error(
      env?.message || env?.error || `HTTP ${res.status} on PUT /keys`,
    );
  }
  return (await res.json()) as PluginKeysWriteResult;
}

export async function deletePluginKeys(name: string): Promise<PluginKeysWriteResult> {
  const res = await fetch(`/api/eval/plugins/${encodeURIComponent(name)}/keys`, {
    method: "DELETE",
    credentials: "same-origin",
  });
  if (!res.ok) {
    let env: PluginKeysErrorEnvelope | null = null;
    try {
      env = (await res.json()) as PluginKeysErrorEnvelope;
    } catch {
      env = null;
    }
    throw new Error(
      env?.message || env?.error || `HTTP ${res.status} on DELETE /keys`,
    );
  }
  return (await res.json()) as PluginKeysWriteResult;
}

// connectLive opens the live trace WebSocket. The backend pushes a full Trace
// object per gateway request; we map it to the summary shape used by the table.
export function connectLive(onTrace: (t: TraceSummary) => void): WebSocket {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const ws = new WebSocket(`${proto}://${location.host}/api/live`);
  ws.onmessage = (ev) => {
    try {
      const raw = JSON.parse(ev.data);
      onTrace({
        trace_id: raw.trace_id,
        timestamp: raw.timestamp,
        provider_name: raw["gen_ai.provider.name"] ?? raw.provider_name ?? "",
        request_model: raw["gen_ai.request.model"] ?? raw.request_model ?? "",
        input_tokens: raw["gen_ai.usage.input_tokens"] ?? 0,
        output_tokens: raw["gen_ai.usage.output_tokens"] ?? 0,
        latency_ms: raw.latency_ms ?? 0,
        ttft_ms: raw.ttft_ms ?? 0,
        cost_usd: raw.cost_usd ?? 0,
        status_code: raw.status_code ?? 0,
        streamed: raw.streamed ? 1 : 0,
        finish_reason: raw["gen_ai.response.finish_reasons"] ?? "",
        cache_hit: raw.cache_hit ? 1 : 0,
        guardrail_action: raw.guardrail_action ?? "",
        credential_source: raw.credential_source ?? "",
        user_id: raw.user_id ?? "",
      });
    } catch {
      /* ignore malformed frames */
    }
  };
  return ws;
}

// --- Model benchmarks (PrimeIntellect hosted evaluations) -------------
//
// A benchmark measures a model against a dataset on an external
// platform, so unlike an eval plugin there is no trace involved and a
// run takes minutes to hours. The console launches one and then watches
// the row settle.

export interface BenchmarkRun {
  id: string;
  org_id: string;
  provider: string;
  external_id: string;
  name: string;
  environments: string[];
  model: string;
  num_examples: number;
  rollouts: number;
  via_gateway: boolean;
  status: "pending" | "running" | "completed" | "failed" | "cancelled";
  external_status?: string;
  avg_score: number | null;
  min_score: number | null;
  max_score: number | null;
  total_samples: number | null;
  metrics?: unknown;
  viewer_url?: string;
  error?: string;
  created_by?: string;
  created_at: string;
  updated_at: string;
  started_at?: string;
  completed_at?: string;
}

export interface BenchmarkListResponse {
  runs: BenchmarkRun[];
  /** False when NEXUS_PUBLIC_GATEWAY_URL is unset, so the form must
   *  not offer to route the provider's inference back through us. */
  gateway_routing_available: boolean;
  /** Server-side cap on num_examples × rollouts. */
  max_total_samples: number;
}

export interface BenchmarkModel {
  id: string;
  name: string;
  provider: string;
  pricing: { prompt: number; completion: number };
}

export interface LaunchBenchmarkBody {
  name?: string;
  environments: string[];
  model: string;
  num_examples?: number;
  rollouts?: number;
  timeout_minutes?: number;
  via_gateway?: boolean;
}

export async function fetchBenchmarks(): Promise<BenchmarkListResponse> {
  const res = await fetch("/api/eval/benchmarks");
  if (!res.ok) {
    return { runs: [], gateway_routing_available: false, max_total_samples: 0 };
  }
  const data = await jsonOrError<BenchmarkListResponse>(res);
  return {
    runs: Array.isArray(data.runs) ? data.runs : [],
    gateway_routing_available: Boolean(data.gateway_routing_available),
    max_total_samples: data.max_total_samples ?? 0,
  };
}

export interface DryRunBenchmarkBody {
  environments: string[];
  model: string;
  timeout_minutes?: number;
}

// dryRunBenchmark probes the vendor credential + environment slugs
// before the operator commits to a real launch.
//
// We deliberately do not raise on non-2xx: the console wants the
// vendor's reason in a typed envelope no matter what HTTP status
// the backend chose, so the form can render the same toast
// conventions on both the happy and the broken path. A 401 here is
// "paste your key" just like a 500 is "the slug does not exist" —
// both belong on screen with the same styling.
export async function dryRunBenchmark(
  body: DryRunBenchmarkBody,
): Promise<{ ok: true; warning?: string } | { ok: false; error: string }> {
  const res = await fetch("/api/eval/benchmarks/validate", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  // Try to read the typed envelope regardless of the HTTP status.
  // vendor errors carry the same { ok, error } shape as the happy
  // path, so a single decoder covers both. A network failure or
  // non-JSON body falls back to a generic message tagged with the
  // status so the operator at least knows the request didn't make
  // it back to a known endpoint.
  let data: { ok?: boolean; error?: string } = {};
  try {
    data = (await res.json()) as { ok?: boolean; error?: string };
  } catch {
    /* body was not JSON; treat as opaque */
  }
  if (data?.ok === true) {
    return { ok: true };
  }
  const err =
    data?.error?.length !== undefined && data.error.length > 0
      ? data.error
      : `dry-run failed (HTTP ${res.status})`;
  return { ok: false, error: err };
}

export async function launchBenchmark(
  body: LaunchBenchmarkBody,
): Promise<BenchmarkRun> {
  const res = await fetch("/api/eval/benchmarks", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  return jsonOrError<BenchmarkRun>(res);
}

export async function cancelBenchmark(id: string): Promise<void> {
  const res = await fetch(`/api/eval/benchmarks/${id}/cancel`, {
    method: "POST",
  });
  await jsonOrError<{ ok: boolean }>(res);
}

export async function deleteBenchmark(id: string): Promise<void> {
  const res = await fetch(`/api/eval/benchmarks/${id}`, { method: "DELETE" });
  await jsonOrError<{ ok: boolean }>(res);
}

export async function fetchBenchmarkLogs(id: string): Promise<string> {
  const res = await fetch(`/api/eval/benchmarks/${id}/logs`);
  const data = await jsonOrError<{ logs: string }>(res);
  return data.logs ?? "";
}

/** Forces a poll pass. The server also polls on a timer; this exists so
 *  an operator watching a long run does not have to wait for the tick. */
export async function refreshBenchmarks(): Promise<number> {
  const res = await fetch("/api/eval/benchmarks/refresh", { method: "POST" });
  const data = await jsonOrError<{ updated: number }>(res);
  return data.updated ?? 0;
}

export async function fetchBenchmarkModels(): Promise<BenchmarkModel[]> {
  const res = await fetch("/api/eval/benchmarks/models");
  const data = await jsonOrError<{ models: BenchmarkModel[] }>(res);
  return Array.isArray(data.models) ? data.models : [];
}

// One merged dropdown entry on the New run panel. The grouping
// distinguishes "Prime base model" (runs directly via the hosted-
// evaluations provider) from "Router alias" (resolved by the Nexus
// gateway to a base model before being scored). Both are launchable
// today; the alias exists so an operator can benchmark the same
// model as Nexus serves it under `code-prime` or `thegrid/...`,
// not the underlying OpenAI/Anthropic id.
export interface BenchmarkModelOption {
  id: string;
  group: "prime" | "router";
  scope?: string;
  // Pricing is only known for Prime entries; router aliases inherit
  // pricing from the underlying base model at launch time.
  pricing?: BenchmarkModel["pricing"];
}

export interface BenchmarkModelCatalog {
  prime: BenchmarkModel[];
  router: { id: string; scope?: string; provider?: string }[];
}

// fetchBenchmarkModelCatalog pulls both halves of the catalog so the
// launch form's picker can group entries. Each side is fetched
// independently and a partial failure on one half does not poison
// the other — an unconfigured cluster (no gateway) is still useful
// because Prime entries can be benchmarked directly. The function
// returns an empty (but well-formed) catalog on total failure so
// the UI renders the free-text fallback model input, not a
// half-populated dropdown that hides options the operator expects
// to see.
export async function fetchBenchmarkModelCatalog(): Promise<BenchmarkModelCatalog> {
  const out: BenchmarkModelCatalog = { prime: [], router: [] };
  try {
    out.prime = await fetchBenchmarkModels();
  } catch {
    out.prime = [];
  }
  try {
    const gw = await fetchGatewayModels();
    for (const u of gw.user) {
      for (const m of u.models) {
        // /api/me/playground/catalog ships router entries as
        // "user/<provider>/<id>"; we restore the schema so the
        // launch payload matches what the existing `via gateway`
        // flow already sends (operators copy-paste these ids into
        // other tools).
        const id = u.provider ? `user/${u.provider}/${m}` : m;
        out.router.push({ id, scope: u.scope, provider: u.provider });
      }
    }
  } catch {
    out.router = [];
  }
  return out;
}

/** One operator-reported `prime env push` outcome.
 *
 *  Reported, not verified: the operator's CLI tells us it ran, and we
 *  take their word for it. Whether the vendor can actually see the slug
 *  is still only answered by dryRunBenchmark. */
export interface EnvPushReport {
  slug: string;
  ok: boolean;
  completed_at: string;
  received_at: string;
}

export async function fetchEnvPushReports(): Promise<EnvPushReport[]> {
  const res = await fetch("/api/eval/benchmarks/push-report");
  // An older server without this route 404s. That is not worth an error
  // toast — the panel simply has nothing to show.
  if (!res.ok) return [];
  const data = await jsonOrError<{ reports: EnvPushReport[] }>(res);
  return Array.isArray(data.reports) ? data.reports : [];
}

export interface BenchmarkCredentialState {
  provider: string;
  configured: boolean;
  team_id?: string;
}

export async function fetchBenchmarkCredential(): Promise<BenchmarkCredentialState> {
  const res = await fetch("/api/eval/benchmarks/credential");
  if (!res.ok) return { provider: "primeintellect", configured: false };
  return jsonOrError<BenchmarkCredentialState>(res);
}

export async function saveBenchmarkCredential(body: {
  api_key?: string;
  team_id?: string;
}): Promise<void> {
  const res = await fetch("/api/eval/benchmarks/credential", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  await jsonOrError<{ ok: boolean }>(res);
}

export async function clearBenchmarkCredential(): Promise<void> {
  const res = await fetch("/api/eval/benchmarks/credential", {
    method: "DELETE",
  });
  await jsonOrError<{ ok: boolean }>(res);
}

// --- Scheduled benchmarks ----------------------------------------------
//
// `benchmark_schedules` is the operator-side intent table: one row per
// recurring fire plan, with cadence and the run-shape fields lifted
// from the same LaunchSpec the manual scheduler posts. The console
// reads these via /api/eval/benchmarks/schedules/ and toggles them
// through the explicit pause/resume POSTs — see schedule_handlers.go
// for why we deliberately do not implement PATCH here.
export interface BenchmarkSchedule {
  id: string;
  org_id: string;
  name: string;
  environments: string[];
  model: string;
  num_examples: number;
  rollouts: number;
  via_gateway: boolean;
  cadence_seconds: number;
  next_launch_at: string;
  enabled: boolean;
  last_run_id?: string;
  last_launched_at?: string;
  created_by?: string;
  created_at: string;
  updated_at: string;
}

export interface CreateBenchmarkScheduleBody {
  name?: string;
  environments: string[];
  model: string;
  num_examples: number;
  rollouts: number;
  via_gateway?: boolean;
  cadence_seconds: number;
  enabled?: boolean;
}

export async function fetchBenchmarkSchedules(): Promise<BenchmarkSchedule[]> {
  const res = await fetch("/api/eval/benchmarks/schedules");
  if (!res.ok) return [];
  const data = await jsonOrError<{ schedules: BenchmarkSchedule[] }>(res);
  return Array.isArray(data.schedules) ? data.schedules : [];
}

export async function createBenchmarkSchedule(
  body: CreateBenchmarkScheduleBody,
): Promise<BenchmarkSchedule> {
  const res = await fetch("/api/eval/benchmarks/schedules", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  return jsonOrError<BenchmarkSchedule>(res);
}

export async function deleteBenchmarkSchedule(id: string): Promise<void> {
  const res = await fetch(`/api/eval/benchmarks/schedules/${id}`, {
    method: "DELETE",
  });
  await jsonOrError<{ ok: boolean }>(res);
}

export async function pauseBenchmarkSchedule(id: string): Promise<BenchmarkSchedule> {
  return jsonOrError<BenchmarkSchedule>(
    await fetch(`/api/eval/benchmarks/schedules/${id}/pause`, { method: "POST" }),
  );
}

export async function resumeBenchmarkSchedule(id: string): Promise<BenchmarkSchedule> {
  return jsonOrError<BenchmarkSchedule>(
    await fetch(`/api/eval/benchmarks/schedules/${id}/resume`, { method: "POST" }),
  );
}

// --- Manual-trigger plugin run ----------------------------------------
//
// `Send.trigger: manual` means inline traces are silently dropped
// (Scheduler never accepts them). The only way to make a manual-
// trigger plugin actually run is to POST to this admin endpoint,
// which drains the buffer / fires the registered dispatcher with the
// given audit tag. The server requires Keycloak admin role.
export interface ManualFireResult {
  ok: boolean;
  count: number;
  message: string;
}

export async function fireEvalPluginManual(
  name: string,
  trigger?: string,
): Promise<ManualFireResult> {
  return fireEvalPluginWhich(name, "manual", trigger);
}

export async function fireEvalPluginScheduled(
  name: string,
  trigger?: string,
): Promise<ManualFireResult> {
  return fireEvalPluginWhich(name, "scheduled", trigger);
}

// fireEvalPluginWhich factors the duplicated fetch/decoding path so
// both modes share the same envelope-aware error helper. `which`
// arrives at the backend as `?which=<mode>`; the route picks
// FireManual or FireScheduled accordingly.
async function fireEvalPluginWhich(
  name: string,
  which: "manual" | "scheduled",
  trigger?: string,
): Promise<ManualFireResult> {
  const qs = `?which=${which}`;
  const res = await fetch(
    `/api/eval/plugins/${encodeURIComponent(name)}/fire${qs}`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      // Empty body is fine — server defaults to "admin@<rfc3339>"
      // when no trigger is provided. Pass an audit tag if the operator
      // wants to record an operational note ("weekly-batch-2025-10-31").
      body: JSON.stringify(trigger ? { trigger } : {}),
    },
  );
  if (!res.ok) {
    let env: PluginKeysErrorEnvelope | null = null;
    try {
      env = (await res.json()) as PluginKeysErrorEnvelope;
    } catch {
      env = null;
    }
    throw new Error(
      env?.message || env?.error || `Backend HTTP ${res.status} on POST /fire`,
    );
  }
  return jsonOrError<ManualFireResult>(res);
}

// CreateLangSmithAutomationRule posts the typed admin REST
// "/automation" envelope and returns the LangSmith-side rule id
// Nexus created plus the webhook URL Nexus advertised. The React
// integration in EvalPlugins.tsx renders both on the row so the
// operator can verify which LangSmith rule mirrors their plugin.
//
// AlreadyConfigured is surfaced as a typed boolean (not parsed
// from message text) so the help text can say "you already have
// this rule — open LangSmith and confirm the webhook" rather
// than a generic "vendor complained". Mirrors backend handler
// behavior documented in pluginCreateAutomationRule.
export interface LangSmithRuleResult {
  ok: boolean;
  rule_id?: string;
  webhook_url?: string;
  already_configured?: boolean;
  message?: string;
}

export async function createLangSmithAutomationRule(
  name: string,
  sessionID: string,
): Promise<LangSmithRuleResult> {
  const res = await fetch(
    `/api/eval/plugins/${encodeURIComponent(name)}/automation`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ session_id: sessionID }),
    },
  );
  // 503 with a typed envelope is a valid "not wired" outcome
  // (Nexus hasn't been configured with NEXUS_PUBLIC_BASE_URL).
  // 200 with ok:false is a vendor-side conflict (409), surfaced
  // for the React UI to swap to a "verify manually" branch.
  if (!res.ok && res.status !== 503) {
    let env: PluginKeysErrorEnvelope | null = null;
    try {
      env = (await res.json()) as PluginKeysErrorEnvelope;
    } catch {
      env = null;
    }
    throw new Error(
      env?.message || env?.error || `Backend HTTP ${res.status} on POST /automation`,
    );
  }
  return jsonOrError<LangSmithRuleResult>(res);
}

// UI observability helper — fetches the operator-supplied Grafana URL
// bundle surfaced on /api/ui/observability so the sidebar / Spend /
// Quality pages can render an "Open in Grafana" link. `grafana` may be
// undefined when the operator did not set NEXUS_PUBLIC_GRAFANA_URL; the
// caller should treat that as "no link".
export type UIObservabilityGrafana = {
  base: string;
  overview: string;
  spend: string;
  eval: string;
};

export type UIObservability = {
  grafana?: UIObservabilityGrafana;
};

export async function fetchUIObservability(): Promise<UIObservability> {
  const res = await fetch("/api/ui/observability");
  if (!res.ok) return {};
  return jsonOrError<UIObservability>(res);
}

// === docs =================================================================
//
// The /api/docs surface mirrors thegrid.ai/docs's "index page plus
// per-slug md" contract: a backend walker turns /docs into a
// sidebar-shaped catalogue (slug → title → summary → category →
// status) and renders each page as the body of the markdown file
// verbatim. The Docs page in the console re-reads the index and the
// page body through these two calls — no in-tree React renderer or
// shadow copy of the prose lives here.

export interface DocEntry {
  path: string;
  title: string;
  summary?: string;
  category: string;
  order: number;
  status?: string;
  updated_at?: string;
  source_path?: string;
  bytes: number;
}

export interface DocCategory {
  slug: string;
  title: string;
  entries: DocEntry[];
}

export interface DocsIndex {
  title: string;
  tagline?: string;
  categories: DocCategory[];
  quick_links: DocEntry[];
}

export interface DocPage {
  path: string;
  title: string;
  summary?: string;
  category: string;
  order: number;
  status?: string;
  source_path?: string;
  bytes: number;
  body: string;
}

class DocsApiError extends Error {
  constructor(public readonly status: number, message: string) {
    super(message);
    this.name = "DocsApiError";
  }
}

export async function fetchDocsIndex(): Promise<DocsIndex> {
  // Failures here used to silently return an empty index, which rendered
  // an empty hero that looked identical to a healthy "no docs" cluster.
  // Throwing on 401/403 lets the page render a sign-in gate; throwing on
  // 5xx lets the page show a "temporarily unavailable" panel. 5xx is
  // distinct from 401 because the response shape is the same shape the
  // index expects, just empty — flatten that into an error so the
  // operator's pod logs (rather than a blank console) are the next
  // stop on the diagnostic path.
  const res = await fetch("/api/docs");
  if (!res.ok) {
    let body = "";
    try {
      body = await res.text();
    } catch {
      // best-effort; nothing to do if the body is not readable
    }
    throw new DocsApiError(res.status, body || res.statusText || "docs index fetch failed");
  }
  return jsonOrError<DocsIndex>(res);
}

export async function fetchDocPage(slug: string): Promise<DocPage | null> {
  // The slug is a /-separated URL path the backend takes from
  // chi.URLParam. EncodeURI keeps dashes and dots intact while
  // turning backslashes (allowed in some legacy paths) into the
  // /api/docs/{slug} url-safe form.
  const safe = encodeURI(slug).replace(/^\/+/, "");
  const res = await fetch(`/api/docs/${safe}`);
  if (res.status === 404) return null;
  if (!res.ok) {
    // 5xx and 401 are both informative failures — the page renders a
    // sign-in gate for the latter so we mirror the index path. A
    // successful return of `null` is reserved for genuinely missing
    // slugs (handled via 404 above) so we never confuse "no such page"
    // with "you are not signed in".
    let body = "";
    try {
      body = await res.text();
    } catch {
      // best-effort
    }
    throw new DocsApiError(res.status, body || res.statusText || "docs page fetch failed");
  }
  return jsonOrError<DocPage>(res);
}
