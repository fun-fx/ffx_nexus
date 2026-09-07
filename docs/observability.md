# Observability

## Vendor-free adapter boundary

Edge runtime observability is **pluggable by design**. Because the trace
records carry OpenTelemetry GenAI semantic conventions (`gen_ai.*`),
Nexus forwards them to whichever backend the operator chooses — without
any code change on the gateway side.

| Sink | Adapter | Opt-in |
| --- | --- | --- |
| ClickHouse `gateway_traces` | `observability.CHRecorder` | enabled when `NEXUS_CLICKHOUSE_URL` is set (control-plane + persistence) |
| Live WebSocket hub (dashboard) | `console.Hub` | always on (UI is opt-out via message rate) |
| Prometheus `/metrics` (text exposition) | `observability.MetricsRecorder` | `NEXUS_METRICS_ADDR=:NNNN` (zero-dep fast path preserved when empty) |
| OTLP/HTTP JSON envelopes | `observability.OTLPRecorder` | `NEXUS_OTLP_ENABLED=true` + full OTLP/HTTP URL in `NEXUS_OTLP_ENDPOINT` |
| Metabase BI dashboards | `observability.MetabaseBootstrapper` | `NEXUS_METABASE_URL=http://metabase:3000` (one-shot boot, no hot-path traffic) |

All adapters compose through `observability.MultiRecorder` so each runs
independently — a ClickHouse outage doesn't disable the OTLP exporter,
a Prometheus scrape misconfig doesn't disable CH trace persistence, a
flaky OTLP collector doesn't gate the gateway hot path. Drop-in a
fifth sink by implementing `Record(Trace)` / `Close(ctx)` and appending
to the recorder list.

### One-line dev container

Bring up Grafana + Prometheus alongside the gateway with a single command.
The bundled dashboards (`deploy/observability/grafana-dashboard.json`) are
auto-loaded so the moment Prometheus starts scraping, panels have data to
chart:

| Panel | Source metric |
| --- | --- |
| Request latency p50 / p95 / p99 by model | `nexus_gateway_request_duration_seconds_bucket` |
| Requests / sec by model | `nexus_gateway_requests_total` |
| Cache hit rate (1h) | `nexus_gateway_cache_hits_total` |
| Cost / hour by model (USD) | `nexus_gateway_cost_usd_total` |
| Failover events / hour | `nexus_router_failover_total` |
| BYOK adoption (per credential source) | `nexus_gateway_requests_total{credential_source=…}` |
| Quality judge score (rolling 1h mean) | `nexus_eval_quality_score` |

```bash
docker compose -f deploy/docker-compose.yml --profile observability up -d
# Prometheus → http://localhost:9090
# Grafana    → http://localhost:3000 (admin/admin; anonymous viewer enabled)

# Or spin the BI tool too — auto-registers ClickHouse + Postgres as datasources
# and pre-seeds the spend / quality / overview collections under
# deploy/observability/metabase/seed. Open http://localhost:3001 once it's up.
docker compose -f deploy/docker-compose.yml --profile bi up -d
# Metabase    → http://localhost:3001  (3000 reserved for the Grafana UI)
```

Want the OTLP collector so other backends (Loki / Tempo / Honeycomb / …)
can plug in without modifying Nexus? Add the `full` profile:

```bash
docker compose -f deploy/docker-compose.yml --profile full up -d
```

### Prometheus-only scrape (Helm)

If you already run Prometheus, just point it at the gateway:

```yaml
# values.yaml
config:
  metricsAddr: ":9095"   # optional — empty disables the /metrics endpoint
  otlpEnabled: false     # leave off if scrape is enough
```

| Env / Helm key | Default | Effect |
| --- | --- | --- |
| `NEXUS_METRICS_ADDR` | _empty_ (`config.metricsAddr: ""`) | bind for the Prometheus `/metrics` scrape server. Empty disables it (zero-dep fast path unchanged). |
| `NEXUS_OTLP_ENABLED` | `false` (`config.otlpEnabled: false`) | export traces via OTLP to `NEXUS_OTLP_ENDPOINT`. |
| `NEXUS_OTLP_ENDPOINT` | _empty_ | OTLP/HTTP or gRPC collector target. |
| `NEXUS_METABASE_URL` | _empty_ | Metabase base URL; empty disables the BI adapter (no DNS, no goroutines). |
| `NEXUS_METABASE_USER` / `_PASSWORD` | _empty_ | Metabase admin login for the bootstrap session. |
| `NEXUS_METABASE_CLICKHOUSE_URL` / `_POSTGRES_URL` | _empty_ | Data sources to register; both are independently opt-in. |
| `NEXUS_METABASE_SEED_DIR` | _empty_ | Directory of `*.json` Metabase collection exports seeded on boot. |

## Metabase BI adapter — quickstart

The V1 dev container ships an optional `bi` profile that brings up Metabase
plus the rest of Nexus's data plumbing. Because the Metabase adapter follows
the same opt-in contract as V3 OTLP, **leaving `NEXUS_METABASE_URL` empty
disables the adapter entirely** — no goroutines, no DNS, no boot network calls.
The gateway never penalises operators who don't need BI.

### Run locally

```bash
# bring up the full stack including Metabase
docker compose -f deploy/docker-compose.yml --profile bi up -d

# open the BI UI in your browser
open http://localhost:3001
```

Once the wizard inside Metabase is finished (operator action — set the admin
password), restart Nexus so the bootstrap re-runs with the right creds:

```bash
docker compose -f deploy/docker-compose.yml --profile bi restart nexus
docker logs -f deploy-nexus-1 | grep -i metabase
# expected: "metabase bootstrap ok"
```

Open Metabase again — `nexus-clickhouse` and `nexus-postgres` should be in
the data source list, and three `Nexus -` collections (overview / spend /
eval) should be visible in the sidebar with pre-built dashboards.

### Run on a cluster (Helm)

```yaml
# deploy/helm/nexus/values.yaml
config:
  metabase:
    url:            http://metabase.observability.svc.cluster.local:3000
    user:           admin@example.com
    password:       <from-secret>
    clickhouseUrl:  http://clickhouse-cloud:8123?database=nexus
    postgresUrl:    postgres://nexus:<from-secret>@pg:5432/nexus
```

The Helm chart maps those values to `NEXUS_METABASE_*` env vars on the
gateway pod. Listing the password in `secrets.managed.metabase.password`
keeps the secret out of git.

### Wire it into the existing observability profile

Combine Grafana (ops) with Metabase (BI) without duplicating data:

```bash
docker compose -f deploy/docker-compose.yml --profile full up -d
# Grafana     → http://localhost:3000
# Metabase    → http://localhost:3001
# Prometheus  → http://localhost:9090
```

Both tools read the same ClickHouse + Postgres. Grafana stays the operational
dashboard; Metabase gets the cohort / spend / SQL-driven reports.

### Replace with another BI tool (Redash / Superset / …)

The adapter is one Go file — `internal/observability/metabase.go`. Any
other tool that exposes a login + datasource REST API can drop in:

1. Add the tool as a new `Bootstrapper` next to `MetabaseBootstrapper`.
2. Pass it to `MultiBootstrapper` in `cmd/nexus/main.go` instead of (or
   alongside) the existing one.
3. New `internal/observability/<tool>_test.go` mirroring the existing
   `metabase_test.go` patterns.

The `Bootstrapper` contract is `Name() string` + `Bootstrap(ctx) error`,
idempotent, log-only on failure.

### Deploying into a customer's existing Metabase (Pattern B)

Most customers don't run their own Metabase, so the adapter behaves as
"self-host" (Pattern A) — datasource registration + dashboard seed happen
on first boot. For the customers that already have a Metabase at work (data
team, BI analysts, governance groups with their own dashboards), the
adapter runs in *coordinate* mode automatically. The two safety nets:

| Resource | Reservation | Ownership marker |
| --- | --- | --- |
| Datasource | `nexus-clickhouse`, `nexus-postgres` | `details.nexus_managed_by = "metabase-bootstrapper/v1"` |
| Collection | `Nexus - <name>` | `description = "[Nexus-managed] <original>"` |

When the adapter finds an existing record using one of the reserved names
*but without* the matching marker, it logs a Warn, refuses to overwrite,
returns the existing id (so the gateway keeps functioning), and lets the
operator decide:

```
$ docker logs deploy-nexus-1 | grep -i 'refraining'
level=WARN msg="metabase datasource with reserved name already exists; refraining from update"
  engine=clickhouse id=13 hint="the existing datasource is not owned by Nexus. To take it over, add `nexus_managed_by: \"metabase-bootstrapper\"` to its details."
```

If the customer moved the data team's datasource onto the reserved name
deliberately and wants Nexus to take it over, the fix is one metadata edit
in their Metabase UI (or a small DB-conn script for a larger deploy):

```
-- Postgres SQL against Metabase's app DB
UPDATE metabase_database
  SET details = jsonb_set(details, '{nexus_managed_by}', '"metabase-bootstrapper/v1"')
  WHERE id = 13;

UPDATE collection
  SET description = '[Nexus-managed] ' || description
  WHERE id = 21;
```

The next Nexus deploy then handles the resource as if it had created it
itself — credentials refreshed, collections re-seeded, dashboards updated.
No code change needed in either direction.

### Verifying the safety net locally

The unit tests in `internal/observability/metabase_test.go` cover both
legs of the safeguard: a foreign datasource does not get PUT-overwritten,
and a foreign collection does not receive new cards. They run in 2s
without any external service, so harness them on every PR:

```
go test ./internal/observability/... -run 'TestMetabase.*(Foreign|Owned)'
```
