# Configuration

Every setting is an environment variable. The Helm chart maps them to
`config.*` values (see [`customer-self-hosted-install.md`](customer-self-hosted-install.md));
`nexus serve --local` sets a handful of them for you.

## Environment variables

| Env var | Default | Purpose |
| --- | --- | --- |
| `NEXUS_GATEWAY_ADDR` | `:8080` | Gateway proxy listen address (override when `:8080` clashes; e.g. `install.sh` uses `:8090` via `NEXUS_GATEWAY_PORT`) |
| `NEXUS_CONSOLE_ADDR` | `:8081` | Console API / dashboard listen address (override similarly; e.g. `install.sh` uses `:8091` via `NEXUS_CONSOLE_PORT`) |
| `NEXUS_PUBLIC_GATEWAY_URL` | _(empty)_ | User-facing gateway base URL shown in the console onboarding curl snippet and the Playground SDK panel (e.g. `https://api.nexus.ffx.ai`). See [Public console vs API hostname](#public-console-vs-api-hostname). |
| `NEXUS_CLICKHOUSE_URL` | _(empty)_ | Native DSN; empty disables persistence |
| `NEXUS_POSTGRES_URL` | _(empty)_ | Control plane DSN; empty disables key auth & credential store |
| `NEXUS_REDIS_URL` | _(empty)_ | Shared rate limits + budgets across replicas; empty = in-memory |
| `NEXUS_MASTER_KEY` | _(empty)_ | 32-byte (base64/hex) KEK for provider-secret encryption |
| `NEXUS_KEY_MODE` | `shared` | Upstream key resolution: `shared` / `byok` / `strict_byok` |
| `NEXUS_ADMIN_EMAIL` / `NEXUS_ADMIN_PASSWORD` | — | Bootstrap the first console admin (only when no users exist) |
| `NEXUS_ALLOW_SIGNUP` | `false` | Enable public `POST /api/auth/register` (member role only; in local mode the first account is promoted to admin) |
| `NEXUS_ENTERPRISE_CTA_URL` | _(empty)_ | Target of the "Talk to us" link on the login page. Empty hides the link. Must be `http(s)`. |
| `NEXUS_SSO_ISSUER` / `NEXUS_SSO_CLIENT_ID` / `NEXUS_SSO_CLIENT_SECRET` / `NEXUS_SSO_REDIRECT_URL` | — | OIDC SSO; when all four are set, `/api/auth/sso/login` is enabled. See [SSO (OIDC)](control-plane.md#sso-oidc-optional). |
| `NEXUS_SSO_LABEL` | `SSO` | UI label for the SSO button (e.g. `Keycloak`) |
| `OPENAI_API_KEY` / `OPENAI_BASE_URL` | — | OpenAI provider |
| `ANTHROPIC_API_KEY` | — | Anthropic provider |
| `GEMINI_API_KEY` | — | Google Gemini provider |
| `GROQ_API_KEY` | — | Groq OpenAI-compatible endpoint (Llama 3.x, Mixtral, Gemma, Whisper, llama-guard; chat model ids auto-listed) |
| `MISTRAL_API_KEY` | — | Mistral OpenAI-compatible endpoint (mistral-large/small, codestral, mixtral, pixtral) |
| `GRID_API_KEY` | — | The Grid (thegrid.ai) OpenAI-compatible endpoint — instruments: text-{standard,prime,max}, code-{standard,prime,max}, agent-{standard,prime,max}. On 307 supplier redirect, `Authorization` is auto-stripped when the new host is not `api.thegrid.ai` (security). See [Provider catalog opt-in](providers.md#provider-catalog-opt-in). |
| `NEXUS_DYNAMIC_MODEL_SYNC` | `false` | Background refresh of `/v1/models` from each provider's upstream (so new OpenAI / Gemini / Anthropic releases appear without a Nexus redeploy). See [Dynamic model catalog sync](control-plane.md#dynamic-model-catalog-sync-nexus_dynamic_model_sync). |
| `NEXUS_DYNAMIC_MODEL_INTERVAL` | `30m` | Refresh cadence (Go duration string; e.g. `10m`, `1h`). |
| `NEXUS_DYNAMIC_MODEL_MAX_RETRY` | `3` | Retry budget per refresh on transient upstream errors (max 60s backoff with jitter). |
| `NEXUS_JUDGE_BASE_URL` / `NEXUS_JUDGE_MODEL` | — / `qwen2.5:7b` | Local SLM judge (Phase 3) |
| `NEXUS_EVAL_ENABLED` | `true` | Async eval worker (heuristics + optional judges) |
| `NEXUS_JUDGE_API_KEY` / `NEXUS_EVAL_SAMPLE_RATE` | — / `1.0` | Judge auth + judge sampling fraction |
| `NEXUS_EVAL_SERVICE_URL` / `_METRICS` | — / `answer_relevancy,toxicity,bias` | Python eval sidecar (DeepEval/RAGAS) |
| `NEXUS_EVAL_WORKERS` / `NEXUS_EVAL_SERVICE_TIMEOUT` | `4` / `30s` | Eval worker concurrency + sidecar timeout |
| `NEXUS_EVAL_PLUGIN_DIR` | `/etc/nexus/eval-plugins` | Directory of Helm-mounted EvalPlugin YAMLs (loaded at boot) |
| `NEXUS_ROUTE_GROUPS` | _(empty)_ | Routing aliases, `alias=m1,m2;...` (Phase 4) |
| `NEXUS_ROUTE_W_QUALITY` / `_W_COST` / `_W_LATENCY` | `0.6` / `0.2` / `0.2` | Routing weights |
| `NEXUS_ROUTE_WINDOW` / `NEXUS_ROUTE_REFRESH` | `1h` / `30s` | Routing stats window & refresh |
| `NEXUS_ROUTE_LOAD_BALANCE` | `false` | Rank-weighted round-robin of the primary model among quality-qualified candidates in a routing alias. See [Load balancing within routing tiers](evals-and-routing.md#load-balancing-within-routing-tiers). |
| `NEXUS_SELF_CORRECTION_ENABLED` / `_MAX_RETRIES` | `false` / `1` | Paid retry of the same model after a schema-guardrail rejection. See [Structured-output self-correction](evals-and-routing.md#structured-output-self-correction). |
| `NEXUS_UPSTREAM_TIMEOUT` | `120s` | Upstream provider timeout |
| `NEXUS_MAX_CONCURRENT_PER_KEY` | `0` (off) | Per-vkey in-flight cap on a single replica (V5). Excess returns `429 concurrency_exceeded` with `Retry-After: 1`. See [High-concurrency tuning (V5)](evals-and-routing.md#high-concurrency-tuning-v5). |
| `NEXUS_FAILOVER_WEBHOOK` / `_SLACK_WEBHOOK` | _(empty)_ | Optional alert sinks on router primary→fallover hops. See [Failover alert sinks (V4)](evals-and-routing.md#failover-alert-sinks-v4). |
| `NEXUS_FAILOVER_ALERT_COOLDOWN` | `0` (off) | Cooldown that coalesces back-to-back alerts onto the same sink. |
| `NEXUS_METABASE_URL` | _(empty)_ | Metabase URL; empty disables the BI adapter. |
| `NEXUS_METABASE_USER` / `_PASSWORD` | _(empty)_ | Admin login for the bootstrap session. |
| `NEXUS_METABASE_CLICKHOUSE_URL` / `_POSTGRES_URL` | _(empty)_ | Data sources to register; both are independently opt-in. |
| `NEXUS_METABASE_SEED_DIR` | _(empty)_ | Directory of `*.json` Metabase collection exports seeded on boot. |

## Local mode

`nexus serve --local` runs a private Postgres as a child process and points
the control plane at it, so the console works on a laptop with nothing else
installed. It is what `npx @ffxnexus/nexus`, `install.sh` and the container
image's default CMD all use.

The mode also forces `NEXUS_AUTO_MIGRATE=true` (an empty database is normal on
first run), `NEXUS_DEV_MODE=true` and non-Secure cookies (the console is
plain HTTP on localhost), and opens signup unless a bootstrap admin is
configured. Those are not individually overridable — a half-applied local mode
produces a console that accepts your password and bounces you back to the
login form.

It refuses to start alongside `NEXUS_POSTGRES_URL`: two control planes is not
a configuration anyone means.

| Env var | Default | Purpose |
| --- | --- | --- |
| `NEXUS_LOCAL_DB` | `false` | Same as passing `--local`. Off by default so a pod never starts a database it cannot persist. |
| `NEXUS_LOCAL_STATE_DIR` | `~/.nexus` | Postgres data directory, cached server binaries, generated `master.key`. The image sets `/app/data`. |
| `NEXUS_LOCAL_DB_BINARIES` | _(empty)_ | Directory whose `bin/` holds `initdb` and `pg_ctl`. Empty downloads and caches them. The image sets `/opt/postgres`. |
| `NEXUS_LOCAL_DB_PORT` | `15432` | Preferred loopback port; moves to the next free one if taken. |

What local mode does **not** do: ClickHouse and Redis stay unconfigured, so
traces are live-only and rate limits are per-process. Point
`NEXUS_CLICKHOUSE_URL` and `NEXUS_REDIS_URL` at real instances if you want
either, or use the [Helm chart](kubernetes.md).

`rm -rf ~/.nexus` is a complete reset, including the encryption key — which
means every provider credential stored through the console becomes
unreadable, so copy `master.key` out first if you care.

## Public console vs API hostname

When the gateway runs behind a public hostname split — e.g. a public
`nexus.<domain>` for the dashboard and a separate `api.<domain>` for
programmatic SDK traffic — point the console at the API hostname with:

```yaml
# deploy/helm/nexus/values.yaml
config:
  publicGatewayUrl: https://api.nexus.ffx.ai
```

```bash
# or directly via env
NEXUS_PUBLIC_GATEWAY_URL=https://api.nexus.ffx.ai go run ./cmd/nexus
```

When set, three things change:

1. **Onboarding curl snippet.** The React Account tab's "copy this curl"
   panel shows `https://api.<domain>` instead of the in-process listen
   address, so a freshly-minted virtual key is immediately usable from
   the public host.
2. **Playground SDK panel.** The Playground's "SDK URL" hint uses the
   same public base so the snippet it pastes into `/v1` calls works
   without rewriting.
3. **Console `/v1/*` reverse proxy.** Because the console and gateway
   share a pod, the console reverse-proxies its in-mux `/v1/*` to the
   co-located gateway on `127.0.0.1` so `/v1/models` discovery and the
   in-browser Playground stay same-origin on the public console host
   (no CORS dance). Cursor-style clients that only trust the API
   hostname connect to `NEXUS_PUBLIC_GATEWAY_URL` directly.

CSP is tightened on `api.<domain>` automatically so the frontend can
cross-origin call it; deploy a TLS cert / Ingress for both hostnames
(the chart does not provision certs itself — use your existing ingress
controller or cert-manager).
