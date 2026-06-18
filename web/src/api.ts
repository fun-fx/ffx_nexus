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
}

export interface User {
  id: string;
  org_id: string;
  email: string;
  role: string;
  enforce_limits: boolean;
  created_at: string;
}

export interface Credential {
  id: string;
  provider: string;
  name: string;
  base_url?: string;
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

export interface EvalMetric {
  evaluator: string;
  metric: string;
  avg_score: number;
  pass_rate: number;
  samples: number;
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

export async function fetchEvals(window = "24h"): Promise<EvalMetric[]> {
  const res = await fetch(`/api/evals?window=${window}`);
  const data = await res.json();
  return Array.isArray(data) ? data : [];
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
}

export async function fetchAuthConfig(): Promise<AuthConfig> {
  const res = await fetch(`/api/auth/config`);
  if (!res.ok) return { signup_enabled: false };
  return res.json();
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

export interface UserQuality {
  user_id: string;
  email: string;
  avg_quality: number;
  pass_rate: number;
  samples: number;
  cost_usd: number;
  requests: number;
}

export interface MyUsageStats {
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

export interface MyUsageQuality {
  user_id: string;
  avg_quality: number;
  pass_rate: number;
  samples: number;
  cost_usd: number;
  requests: number;
}

export async function fetchMyStats(window = "1h"): Promise<MyUsageStats> {
  const res = await fetch(`/api/me/stats?window=${window}`);
  return res.json();
}

export async function fetchMyTraces(limit = 20): Promise<TraceSummary[]> {
  const res = await fetch(`/api/me/traces?limit=${limit}`);
  const data = await res.json();
  return Array.isArray(data) ? data : [];
}

export async function fetchMyQuality(window = "24h"): Promise<MyUsageQuality[]> {
  const res = await fetch(`/api/me/quality?window=${window}`);
  const data = await res.json();
  return Array.isArray(data) ? data : [];
}

// fetchUserQuality returns per-user rolling quality + spend (admin only). This
// is the eval differentiator: quality per user, not just spend per key.
export async function fetchUserQuality(window = "24h"): Promise<UserQuality[]> {
  const res = await fetch(`/api/users/quality?window=${window}`);
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
      });
    } catch {
      /* ignore malformed frames */
    }
  };
  return ws;
}
