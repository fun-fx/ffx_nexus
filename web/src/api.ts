// API client + types for the Nexus console.

export interface Stats {
  total_requests: number;
  error_rate: number;
  avg_latency_ms: number;
  p95_latency_ms: number;
  total_tokens: number;
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
  };
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
    },
    restart_required: safeStrArr((raw as any)?.restart_required),
  };
}

const ZERO_EVAL: EvalConfigSnapshot = {
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
  },
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
