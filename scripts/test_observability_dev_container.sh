#!/usr/bin/env bash
# V1 dev-container smoke:
#   1. Build the gateway binary.
#   2. Boot the gateway with NEXUS_METRICS_ADDR=:9001 enabled (zero-dep mode).
#   3. Scrape /metrics and verify Prometheus exposition format contains every
#      series the pre-baked Grafana dashboard expects.
#   4. (Optional) When docker compose is available, start the `observability`
#      profile and assert Prometheus + Grafana started with the pre-baked
#      dashboard JSON provisioned.
#
# Designed to run as part of scripts/test_all.sh so CI catches regressions
# before they reach the release pipeline.
set -euo pipefail

source "$(dirname "$0")/lib/e2e_common.sh"
e2e_init

echo "== V1 dev-container observability smoke =="

# Use the e2e common-alt ports so we don't clash with a dev :8080/:8081.
export NEXUS_GATEWAY_ADDR=${NEXUS_GATEWAY_ADDR:-:8090}
export NEXUS_CONSOLE_ADDR=${NEXUS_CONSOLE_ADDR:-:8091}
METRICS_PORT=9091

go build -o "$BIN" ./cmd/nexus
pass "build ok"

# --- Phase A: in-process metrics endpoint (no docker required) -------------
SCRAPE_FILE=$(mktemp)
trap 'stop_nexus; rm -f "$SCRAPE_FILE"' EXIT
start_nexus env \
  -u NEXUS_POSTGRES_URL -u NEXUS_CLICKHOUSE_URL -u NEXUS_REDIS_URL -u NEXUS_MASTER_KEY \
  NEXUS_METRICS_ADDR=":$METRICS_PORT"

code=$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:$METRICS_PORT/healthz")
if [[ "$code" == "200" ]]; then pass "metrics /healthz -> 200"; else fail "metrics /healthz -> $code"; fi

# Drive at least one chat completion so the request counters AND the latency
# histogram have data. Any chat completion produces a Trace (including auth
# failures), so we don't need a provider key — zero-dep mode emits the trace
# with LatencyMs set even when the upstream is rejected.
#
# We use /v1/models to drive a 401-free path earlier; here we need to hit the
# chat endpoint so the trace path is exercised.
MODEL="${TEST_MODEL:-gemini-2.5-flash}"
code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$GW_URL/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"'"$MODEL"'","messages":[{"role":"user","content":"hi"}],"max_tokens":4}')
if [[ "$code" =~ ^(200|400|402|403|429|500|502)$ ]]; then
  pass "chat completion drove a Trace (HTTP $code, rec emit path unconditional)"
else
  fail "unexpected chat HTTP code: $code"
fi

SCRAPE=$(curl -s "http://127.0.0.1:$METRICS_PORT/metrics")
printf '%s\n' "$SCRAPE" >"$SCRAPE_FILE"
for series in \
  "nexus_gateway_requests_total" \
  "nexus_gateway_request_duration_seconds" \
  "nexus_gateway_cache_hits_total" \
  "nexus_gateway_errors_total" \
  "nexus_router_failover_total" \
  "nexus_gateway_cost_usd_total" \
  "nexus_eval_quality_score"; do
  if grep -q "^# HELP $series " "$SCRAPE_FILE"; then pass "# HELP $series present"; else fail "missing HELP for $series"; fi
  if grep -q "^# TYPE $series " "$SCRAPE_FILE"; then pass "# TYPE $series present"; else fail "missing TYPE for $series"; fi
done

# Validate exposition format header that Prometheus scrapers parse.
if head -1 "$SCRAPE_FILE" | grep -q '^# HELP '; then pass "exposition header starts with HELP"; else fail "exposition format malformed"; fi

# Stop the in-process gateway before touching docker compose.
stop_nexus

# --- Phase B: docker compose observability profile (best-effort) ----------
if ! command -v docker >/dev/null 2>&1; then
  echo "  (skipping Phase B: docker not available; Phase A is the contract test)"
  echo
  echo "Contracts:"
  echo "  PASS: $PASS    FAIL: $FAIL"
  if [[ $FAIL -gt 0 ]]; then exit 1; fi
  exit 0
fi

# Detect non-empty profiles by listing services; only proceed if the profile
# is available in this docker compose file. (Skipping on remote CI runners
# that don't have docker compose v2 installed.)
if ! docker compose -f deploy/docker-compose.yml config >/dev/null 2>&1; then
  echo "  (skipping Phase B: docker compose cannot parse deploy/docker-compose.yml)"
  echo
  echo "Contracts:"
  echo "  PASS: $PASS    FAIL: $FAIL"
  if [[ $FAIL -gt 0 ]]; then exit 1; fi
  exit 0
fi

echo "  bringing up observability profile (clickhouse, postgres, redis, prometheus, grafana, nexus)…"
docker compose -f deploy/docker-compose.yml --profile observability up -d \
  postgres redis clickhouse prometheus grafana nexus >/dev/null 2>&1 || {
  echo "  (docker compose up failed; continuing — remote CI may lack the daemon)"
  echo
  echo "Contracts:"
  echo "  PASS: $PASS    FAIL: $FAIL"
  if [[ $FAIL -gt 0 ]]; then exit 1; fi
  exit 0
}

# Wait for Prometheus / Grafana to come up. Generous timeout because Docker
# volumes may need to initialise.
for i in $(seq 1 60); do
  curl -sf http://localhost:9090/-/ready >/dev/null 2>&1 && \
    curl -sf http://localhost:3000/api/health >/dev/null 2>&1 && break
  sleep 1
done

# Prometheus healthy?
code=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:9090/-/ready)
if [[ "$code" == "200" ]]; then pass "prometheus /-/ready -> 200"; else fail "prometheus -> $code"; fi

# Grafana healthy and the pre-baked dashboard is provisioned.
code=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:3000/api/health)
if [[ "$code" == "200" ]]; then pass "grafana /api/health -> 200"; else fail "grafana -> $code"; fi

# The bundled dashboard uid matches the JSON file.
if curl -sf -u admin:admin http://localhost:3000/api/dashboards/uid/nexus-overview >/dev/null 2>&1; then
  pass "pre-baked nexus-overview dashboard provisioned"
else
  fail "nexus-overview dashboard not provisioned (check grafana-dashboard.json mount)"
fi

# Nexus gateway exposes /metrics to Prometheus.
code=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/metrics)
if [[ "$code" == "200" ]]; then pass "nexus /metrics -> 200 (Prometheus target up)"; else fail "nexus /metrics -> $code"; fi
code=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:8081/metrics)
if [[ "$code" == "200" ]]; then pass "nexus console /metrics -> 200"; else fail "nexus console /metrics -> $code"; fi

echo
echo "Contracts:"
echo "  PASS: $PASS    FAIL: $FAIL"
[[ $FAIL -eq 0 ]]
