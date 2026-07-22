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
  const res = await fetch(`/api/stats?window=${window}`);
  return res.json();
}

export async function fetchTraces(limit = 100): Promise<TraceSummary[]> {
  const res = await fetch(`/api/traces?limit=${limit}`);
  const data = await res.json();
  return Array.isArray(data) ? data : [];
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
  const res = await fetch("/api/eval/config");
  if (!res.ok) {
    const data = await res.json().catch(() => ({}));
    throw new Error(data.error || `HTTP ${res.status}`);
  }
  return res.json();
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
  return res.json();
}

// --- Gateway /v1/models (open-source discovery) ---

export interface GatewayModel {
  id: string;
  owned_by: string;
}

export interface GatewayModelCatalog {
  chat: string[];
  embed: string[];
  user: { provider: string; models: string[] }[];
}

// fetchGatewayModels hits /v1/models and returns the union list, plus a
// breakdown of any user/<provider>/... entries so the Playground picker can
// offer them grouped. Tolerates a 401/404 when the gateway is not reachable
// from the console (e.g. users running the dashboard alone) by returning an
// empty catalog so the UI renders the small set of stock options instead of
// crashing.
export async function fetchGatewayModels(): Promise<GatewayModelCatalog> {
  try {
    const res = await fetch(`/v1/models`);
    if (!res.ok) return { chat: [], embed: [], user: [] };
    const data = await res.json();
    const chat = Array.isArray(data?.data) ? data.data.map((m: { id: string }) => m.id) : [];
    const embed = Array.isArray(data?.embeddings?.data) ? data.embeddings.data.map((m: { id: string }) => m.id) : [];
    const user = collectUserModels(chat);
    return { chat, embed, user };
  } catch {
    return { chat: [], embed: [], user: [] };
  }
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
