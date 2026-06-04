#!/usr/bin/env bash
# E2E test for zero-dependency mode: gateway boots without Postgres,
# ClickHouse, or Redis; uses env-configured provider keys only.
set -euo pipefail

source "$(dirname "$0")/lib/e2e_common.sh"
e2e_init

echo "== Zero-dependency mode =="

go build -o "$BIN" ./cmd/nexus
pass "build ok"

load_dotenv
export_e2e_env

if [[ -z "${GEMINI_API_KEY:-}" && -z "${OPENAI_API_KEY:-}" ]]; then
  echo "  SKIP: no provider key in .env"
  exit 0
fi

MODEL="${TEST_MODEL:-gemini-2.5-flash}"
trap stop_nexus EXIT

# No postgres, clickhouse, redis, master key — keep test ports isolated from dev :8080.
start_nexus env \
  -u NEXUS_POSTGRES_URL -u NEXUS_CLICKHOUSE_URL -u NEXUS_REDIS_URL -u NEXUS_MASTER_KEY
pass "nexus started without datastores"

code=$(http_code "$GW_URL/healthz")
if [[ "$code" == "200" ]]; then pass "healthz -> 200"; else fail "healthz -> $code"; fi

# No auth required when postgres disabled.
code=$(http_code "$GW_URL/v1/models")
if [[ "$code" == "200" ]]; then pass "models without auth -> 200 (zero-dep)"; else fail "models -> $code"; fi

CHAT=$(curl -s -o /tmp/zero_dep.json -w "%{http_code}" -X POST "$GW_URL/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"'"$MODEL"'","messages":[{"role":"user","content":"Say ok"}],"max_tokens":16}')
if [[ "$CHAT" == "200" ]]; then
  pass "completion without virtual key -> 200"
elif grep -qE 'RESOURCE_EXHAUSTED|quota exceeded' /tmp/zero_dep.json 2>/dev/null; then
  skip "completion (upstream quota exhausted)"
else
  fail "completion failed: HTTP $CHAT $(cat /tmp/zero_dep.json)"
fi

summary_exit
