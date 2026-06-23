#!/usr/bin/env bash
# E2E tests for eval→routing loop and provider fallback (PR #2, #3).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

GW="${NEXUS_GATEWAY_ADDR:-:8090}"
CON="${NEXUS_CONSOLE_ADDR:-:8091}"
GW_URL="http://127.0.0.1:${GW#:}"
CON_URL="http://127.0.0.1:${CON#:}"
BIN="${BIN:-./bin/nexus}"
NEXUS_PID=""

PASS=0
FAIL=0
SKIP=0

pass() { PASS=$((PASS + 1)); echo "  ✓ $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  ✗ $1"; }
skip() { SKIP=$((SKIP + 1)); echo "  ⊘ SKIP: $1"; }

upstream_quota_exhausted() {
  grep -qE 'RESOURCE_EXHAUSTED|quota exceeded' "$1" 2>/dev/null
}

stop_nexus() {
  if [[ -n "$NEXUS_PID" ]] && kill -0 "$NEXUS_PID" 2>/dev/null; then
    kill "$NEXUS_PID" 2>/dev/null || true
    wait "$NEXUS_PID" 2>/dev/null || true
  fi
  NEXUS_PID=""
  for port in "${GW#:}" "${CON#:}"; do
    if pid=$(lsof -ti ":$port" 2>/dev/null); then
      kill $pid 2>/dev/null || true
    fi
  done
  sleep 0.5
}

start_nexus() {
  stop_nexus
  "$BIN" &
  NEXUS_PID=$!
  for i in $(seq 1 30); do
    if curl -sf "$GW_URL/healthz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  echo "nexus failed to start on $GW_URL"
  exit 1
}

echo "== Preflight =="
docker compose -f deploy/docker-compose.yml up -d postgres clickhouse redis >/dev/null 2>&1
for i in $(seq 1 30); do
  curl -sf http://localhost:8123/ping >/dev/null 2>&1 && break
  sleep 1
done
go build -o "$BIN" ./cmd/nexus
pass "build ok"

export NEXUS_GATEWAY_ADDR="$GW"
export NEXUS_CONSOLE_ADDR="$CON"
export NEXUS_POSTGRES_URL="${NEXUS_POSTGRES_URL:-postgres://nexus:nexus@localhost:5433/nexus?sslmode=disable}"
export NEXUS_CLICKHOUSE_URL="${NEXUS_CLICKHOUSE_URL:-clickhouse://nexus:nexus@localhost:9000/nexus}"
export NEXUS_REDIS_URL="${NEXUS_REDIS_URL:-redis://localhost:6379/0}"
export NEXUS_MASTER_KEY="${NEXUS_MASTER_KEY:-$(openssl rand -hex 32)}"
export NEXUS_ROUTE_GROUPS="${NEXUS_ROUTE_GROUPS:-fast=gemini-2.5-flash}"
export NEXUS_ROUTE_REFRESH="${NEXUS_ROUTE_REFRESH:-3s}"

if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  source .env
  set +a
fi

MODEL="${TEST_MODEL:-gemini-2.5-flash}"
if [[ -z "${GEMINI_API_KEY:-}" && -z "${OPENAI_API_KEY:-}" ]]; then
  echo "  WARN: no provider key — upstream/fallback completion tests will be skipped"
  HAS_PROVIDER=0
else
  HAS_PROVIDER=1
fi

trap stop_nexus EXIT
start_nexus
pass "nexus started"

# --- Admin login (org-level /api/keys, /api/credentials are admin-only since v1.1).
ADMIN_EMAIL="${ADMIN_EMAIL:-admin@nexus.local}"
ADMIN_PASS="${ADMIN_PASS:-admin-e2e-password}"
ADMIN_JAR="/tmp/nexus_eval_routing_admin.txt"
# Register and promote to admin.
REG=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$CON_URL/api/auth/register" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}")
if [[ "$REG" != "201" && "$REG" != "409" ]]; then
  fail "admin register unexpected: $REG"
  exit 1
fi
docker compose -f deploy/docker-compose.yml exec -T postgres \
  psql -U nexus -d nexus -c "UPDATE users SET role='admin' WHERE email='$ADMIN_EMAIL'" >/dev/null 2>&1 || true
LOGIN=$(curl -s -o /dev/null -w '%{http_code}' -c "$ADMIN_JAR" -X POST "$CON_URL/api/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}")
if [[ "$LOGIN" != "200" ]]; then
  fail "admin login failed ($LOGIN)"
  exit 1
fi
pass "admin login -> 200 + session cookie"

# Without env provider keys, seed a DB credential and restart so routing tests
# have registered models (min_quality gate needs candidates in the auto group).
if [[ "$HAS_PROVIDER" == "0" ]]; then
  curl -s -b "$ADMIN_JAR" -X POST "$CON_URL/api/credentials" \
    -H 'Content-Type: application/json' \
    -d '{"provider":"gemini","name":"e2e-seed","secret":"sk-e2e-fake-'"$(openssl rand -hex 8)"'"}' >/dev/null
  stop_nexus
  start_nexus env -u GEMINI_API_KEY -u OPENAI_API_KEY -u ANTHROPIC_API_KEY
  pass "provider seeded from DB credential (no env keys)"
fi

# --- min_quality_score enforcement (503) ---

echo ""
echo "== min_quality_score enforcement =="

# Exploration quality is 0.75; eff_quality caps at 1.0. Use 1.01 so no model
# can pass regardless of accumulated ClickHouse stats.
MQ_KEY=$(curl -s -b "$ADMIN_JAR" -X POST "$CON_URL/api/keys" \
  -H 'Content-Type: application/json' \
  -d '{"name":"e2e-minq","allowed_models":["auto","'"$MODEL"'"],"min_quality_score":1.01}')
MQ_SECRET=$(echo "$MQ_KEY" | python3 -c "import sys,json; print(json.load(sys.stdin)['secret'])")

code=$(curl -s -o /tmp/mq_resp.json -w "%{http_code}" -X POST "$GW_URL/v1/chat/completions" \
  -H "Authorization: Bearer $MQ_SECRET" \
  -H 'Content-Type: application/json' \
  -d '{"model":"auto","messages":[{"role":"user","content":"hi"}],"max_tokens":32}')
err_type=$(python3 -c "import json; d=json.load(open('/tmp/mq_resp.json')); print(d.get('error',{}).get('type',''))" 2>/dev/null || echo "")

if [[ "$code" == "503" && "$err_type" == "no_model_meets_quality" ]]; then
  pass "min_quality=1.01 blocks auto routing -> 503 no_model_meets_quality"
else
  fail "min_quality gate: expected 503/no_model_meets_quality, got code=$code type=$err_type body=$(cat /tmp/mq_resp.json)"
fi

# min_quality=0 should allow routing (exploration 0.75 >= 0).
OK_KEY=$(curl -s -b "$ADMIN_JAR" -X POST "$CON_URL/api/keys" \
  -H 'Content-Type: application/json' \
  -d '{"name":"e2e-minq0","allowed_models":["auto","'"$MODEL"'"],"min_quality_score":0}')
OK_SECRET=$(echo "$OK_KEY" | python3 -c "import sys,json; print(json.load(sys.stdin)['secret'])")

if [[ "$HAS_PROVIDER" == "1" ]]; then
  code=$(curl -s -o /tmp/mq0_resp.json -w "%{http_code}" -X POST "$GW_URL/v1/chat/completions" \
    -H "Authorization: Bearer $OK_SECRET" \
    -H 'Content-Type: application/json' \
    -d '{"model":"auto","messages":[{"role":"user","content":"Say ok"}],"max_tokens":16}')
  if [[ "$code" == "200" ]]; then
    pass "min_quality=0 allows auto routing -> 200"
  elif upstream_quota_exhausted /tmp/mq0_resp.json; then
    skip "min_quality=0 upstream test (quota exhausted)"
  else
    fail "min_quality=0: expected 200, got $code body=$(cat /tmp/mq0_resp.json)"
  fi
else
  echo "  SKIP min_quality=0 upstream test (no provider key)"
fi

# --- /api/routing exposes eff_quality ---

echo ""
echo "== routing stats (eff_quality) =="

ROUTING=$(curl -s "$CON_URL/api/routing")
if echo "$ROUTING" | python3 -c "
import sys, json
data = json.load(sys.stdin)
assert isinstance(data, list)
# After traffic, entries may include eff_quality field when stats exist.
print('ok')
" 2>/dev/null; then
  pass "GET /api/routing returns valid JSON array"
else
  fail "GET /api/routing invalid: $ROUTING"
fi

if [[ "$HAS_PROVIDER" == "1" ]]; then
  # Generate traffic then check routing stats include a model with samples.
  curl -s -X POST "$GW_URL/v1/chat/completions" \
    -H "Authorization: Bearer $OK_SECRET" \
    -H 'Content-Type: application/json' \
    -d '{"model":"'"$MODEL"'","messages":[{"role":"user","content":"hi"}],"max_tokens":64}' >/dev/null
  sleep 4  # wait for eval worker + router refresh

  HAS_EFF=$(curl -s "$CON_URL/api/routing" | python3 -c "
import sys, json
rows = json.load(sys.stdin)
for r in rows:
    if r.get('model') == '$MODEL' and r.get('samples', 0) > 0:
        eq = r.get('eff_quality', -1)
        if eq >= 0:
            print('yes')
            sys.exit(0)
print('no')
" 2>/dev/null || echo "no")

  if [[ "$HAS_EFF" == "yes" ]]; then
    pass "routing stats include eff_quality for $MODEL after traffic"
  else
    fail "expected eff_quality in /api/routing for $MODEL"
  fi

  # Check heuristic eval scores landed (feeds safety pass rate).
  EVAL_COUNT=$(curl -s "http://localhost:8123/?user=nexus&password=nexus" \
    --data-binary "SELECT count() FROM nexus.eval_scores WHERE evaluator LIKE 'heuristic_%'" 2>/dev/null || echo 0)
  if [[ "${EVAL_COUNT:-0}" -gt 0 ]]; then
    pass "heuristic eval scores present in ClickHouse (count=$EVAL_COUNT)"
  else
    fail "no heuristic eval_scores in ClickHouse"
  fi
fi

# --- provider fallback ---

echo ""
echo "== provider fallback =="

if [[ "$HAS_PROVIDER" == "1" ]]; then
  # Group: unregistered model first, then working model — resolve fails on first,
  # upstream succeeds on second (failover chain).
  FB_KEY=$(curl -s -b "$ADMIN_JAR" -X POST "$CON_URL/api/keys" \
    -H 'Content-Type: application/json' \
    -d '{"name":"e2e-fallback","allowed_models":["auto","fast","'"$MODEL"'","nonexistent-model-xyz"]}')
  FB_SECRET=$(echo "$FB_KEY" | python3 -c "import sys,json; print(json.load(sys.stdin)['secret'])")

  # Use explicit group with bad model first via env — restart nexus with updated groups.
  stop_nexus
  export NEXUS_ROUTE_GROUPS="failover=nonexistent-model-xyz,$MODEL"
  start_nexus

  FB_KEY2=$(curl -s -b "$ADMIN_JAR" -X POST "$CON_URL/api/keys" \
    -H 'Content-Type: application/json' \
    -d '{"name":"e2e-fb2","allowed_models":["failover","'"$MODEL"'"]}')
  FB_SECRET2=$(echo "$FB_KEY2" | python3 -c "import sys,json; print(json.load(sys.stdin)['secret'])")

  code=$(curl -s -o /tmp/fb_resp.json -w "%{http_code}" -X POST "$GW_URL/v1/chat/completions" \
    -H "Authorization: Bearer $FB_SECRET2" \
    -H 'Content-Type: application/json' \
    -d '{"model":"failover","messages":[{"role":"user","content":"Say ok"}],"max_tokens":64}')

  # Retry once on transient upstream errors (503 high demand or quota backoff).
  if [[ "$code" == "502" ]]; then
    if upstream_quota_exhausted /tmp/fb_resp.json; then
      echo "  (upstream quota hit, waiting 65s before retry...)" >&2
      sleep 65
    else
      sleep 2
    fi
    code=$(curl -s -o /tmp/fb_resp.json -w "%{http_code}" -X POST "$GW_URL/v1/chat/completions" \
      -H "Authorization: Bearer $FB_SECRET2" \
      -H 'Content-Type: application/json' \
      -d '{"model":"failover","messages":[{"role":"user","content":"Say ok"}],"max_tokens":16}')
  fi

  if [[ "$code" == "200" ]]; then
    pass "failover group: bad model skipped, $MODEL succeeds -> 200"
  elif upstream_quota_exhausted /tmp/fb_resp.json; then
    skip "failover group (quota exhausted)"
  else
    fail "failover group: expected 200, got $code body=$(cat /tmp/fb_resp.json)"
  fi

  # Concrete model with bad id should NOT fallback — 404/502, not success via another model.
  code=$(curl -s -o /tmp/concrete_resp.json -w "%{http_code}" -X POST "$GW_URL/v1/chat/completions" \
    -H "Authorization: Bearer $FB_SECRET2" \
    -H 'Content-Type: application/json' \
    -d '{"model":"nonexistent-model-xyz","messages":[{"role":"user","content":"hi"}],"max_tokens":32}')
  if [[ "$code" != "200" ]]; then
    pass "concrete unknown model does not fallback -> $code"
  else
    fail "concrete unknown model should not succeed via fallback"
  fi
else
  echo "  SKIP fallback completion tests (no provider key)"
fi

# --- summary ---

echo ""
echo "== Summary =="
echo "  passed: $PASS"
[[ "${SKIP:-0}" -gt 0 ]] && echo "  skipped: $SKIP"
echo "  failed: $FAIL"
if [[ "$FAIL" -gt 0 ]]; then
  exit 1
fi
echo "  all tests passed"
