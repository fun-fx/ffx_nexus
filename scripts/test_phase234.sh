#!/usr/bin/env bash
# E2E tests for Phase 2b (rate limits + budgets), Phase 3 (async evals),
# and Phase 4 (quality-aware routing).
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
  # Also kill anything still bound to our test ports.
  for port in "${GW#:}" "${CON#:}"; do
    if pid=$(lsof -ti ":$port" 2>/dev/null); then
      kill $pid 2>/dev/null || true
    fi
  done
  sleep 0.5
}

start_nexus() {
  stop_nexus
  # Optional env overrides passed as KEY=VAL arguments (e.g. NEXUS_KEY_MODE=byok).
  if [[ $# -gt 0 ]]; then
    env "$@" "$BIN" &
  else
    "$BIN" &
  fi
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

http_code() {
  curl -s -o /dev/null -w "%{http_code}" "$@"
}

http_body() {
  curl -s "$@"
}

# --- Preflight ---

echo "== Preflight =="
command -v docker >/dev/null || { echo "docker required"; exit 1; }
command -v curl >/dev/null || { echo "curl required"; exit 1; }

docker compose -f deploy/docker-compose.yml up -d postgres clickhouse redis >/dev/null 2>&1
echo "  waiting for postgres..."
for i in $(seq 1 30); do
  docker compose -f deploy/docker-compose.yml exec -T postgres pg_isready -U nexus >/dev/null 2>&1 && break
  sleep 1
done
echo "  waiting for clickhouse..."
for i in $(seq 1 30); do
  curl -sf "http://localhost:8123/ping" >/dev/null 2>&1 && break
  sleep 1
done
echo "  waiting for redis..."
for i in $(seq 1 15); do
  docker compose -f deploy/docker-compose.yml exec -T redis redis-cli ping 2>/dev/null | grep -q PONG && break
  sleep 0.5
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
export NEXUS_ROUTE_REFRESH="${NEXUS_ROUTE_REFRESH:-5s}"

# Load provider keys from .env if present.
if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  source .env
  set +a
fi

if [[ -z "${GEMINI_API_KEY:-}" && -z "${OPENAI_API_KEY:-}" ]]; then
  echo "  WARN: no GEMINI_API_KEY or OPENAI_API_KEY — upstream completion tests will be skipped"
  HAS_PROVIDER=0
else
  HAS_PROVIDER=1
fi

MODEL="${TEST_MODEL:-gemini-2.5-flash}"

trap stop_nexus EXIT
# Boot with a bootstrap admin so /api/keys and /api/credentials (admin-only
# since v1.1) accept the test calls below.
ADMIN_EMAIL="${ADMIN_EMAIL:-admin-e2e@nexus.local}"
ADMIN_PASS="${ADMIN_PASS:-admin-e2e-pass}"
ADMIN_JAR="/tmp/nexus_phase234_admin.txt"
start_nexus env \
  NEXUS_ALLOW_SIGNUP=true \
  NEXUS_ADMIN_EMAIL="$ADMIN_EMAIL" \
  NEXUS_ADMIN_PASSWORD="$ADMIN_PASS"
pass "nexus started ($GW_URL)"

# Reset the admin password (the postgres volume is persistent across scripts).
docker compose -f deploy/docker-compose.yml exec -T postgres \
  psql -U nexus -d nexus -c \
  "UPDATE users SET password_hash = crypt('$ADMIN_PASS', gen_salt('bf')), role='admin' WHERE email='$ADMIN_EMAIL'" \
  >/dev/null 2>&1 || true

# Bootstrap admin was created above via NEXUS_ADMIN_EMAIL/PASSWORD.
LOGIN=$(curl -s -o /dev/null -w '%{http_code}' -c "$ADMIN_JAR" -X POST "$CON_URL/api/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}")
if [[ "$LOGIN" != "200" ]]; then
  fail "admin login failed ($LOGIN)"
  exit 1
fi
pass "admin login -> 200 + session cookie"

# --- Phase 2b: auth sanity ---

echo ""
echo "== Phase 2b: rate limits & budgets =="

code=$(http_code "$GW_URL/v1/models")
if [[ "$code" == "401" ]]; then pass "no auth -> 401"; else fail "no auth -> expected 401, got $code"; fi

# --- RPM limit (429) ---

RPM_KEY_JSON=$(curl -s -b "$ADMIN_JAR" -X POST "$CON_URL/api/keys" \
  -H 'Content-Type: application/json' \
  -d '{"name":"e2e-rpm","allowed_models":["'"$MODEL"'"],"rpm_limit":2}')
RPM_SECRET=$(echo "$RPM_KEY_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['secret'])")
RPM_ID=$(echo "$RPM_KEY_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['key']['id'])")

c1=$(http_code -H "Authorization: Bearer $RPM_SECRET" "$GW_URL/v1/models")
c2=$(http_code -H "Authorization: Bearer $RPM_SECRET" "$GW_URL/v1/models")
c3=$(http_code -H "Authorization: Bearer $RPM_SECRET" "$GW_URL/v1/models")

if [[ "$c1" == "200" && "$c2" == "200" ]]; then
  pass "rpm: first two requests -> 200"
else
  fail "rpm: first two requests -> expected 200, got $c1 $c2"
fi
if [[ "$c3" == "429" ]]; then
  pass "rpm: third request -> 429"
else
  fail "rpm: third request -> expected 429, got $c3"
fi

# --- Monthly budget (402) ---

BUD_KEY_JSON=$(curl -s -b "$ADMIN_JAR" -X POST "$CON_URL/api/keys" \
  -H 'Content-Type: application/json' \
  -d '{"name":"e2e-budget","allowed_models":["'"$MODEL"'"],"monthly_budget_usd":0.01}')
BUD_SECRET=$(echo "$BUD_KEY_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['secret'])")
BUD_ID=$(echo "$BUD_KEY_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['key']['id'])")

# Pre-seed spend above budget in Redis (month bucket = UTC YYYYMM).
MONTH=$(date -u +%Y%m)
SPEND_KEY="nexus:spend:${BUD_ID}:${MONTH}"
docker compose -f deploy/docker-compose.yml exec -T redis redis-cli SET "$SPEND_KEY" "0.02" >/dev/null

c402=$(http_code -H "Authorization: Bearer $BUD_SECRET" "$GW_URL/v1/models")
if [[ "$c402" == "402" ]]; then
  pass "budget: pre-seeded spend -> 402"
else
  fail "budget: pre-seeded spend -> expected 402, got $c402"
fi

# --- Phase 3: async evals (heuristics) ---

echo ""
echo "== Phase 3: async evals =="

if [[ "$HAS_PROVIDER" == "1" ]]; then
  EVAL_KEY_JSON=$(curl -s -b "$ADMIN_JAR" -X POST "$CON_URL/api/keys" \
    -H 'Content-Type: application/json' \
    -d '{"name":"e2e-eval","allowed_models":["'"$MODEL"'"]}')
  EVAL_SECRET=$(echo "$EVAL_KEY_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['secret'])")

  CHAT=$(curl -s -o /tmp/p234_chat.json -w "%{http_code}" -X POST "$GW_URL/v1/chat/completions" \
    -H "Authorization: Bearer $EVAL_SECRET" \
    -H 'Content-Type: application/json' \
    -d '{"model":"'"$MODEL"'","messages":[{"role":"user","content":"Reply with exactly: ok"}],"max_tokens":64}')
  CONTENT=$(python3 -c "import json; d=json.load(open('/tmp/p234_chat.json')); print(d.get('choices',[{}])[0].get('message',{}).get('content',''))" 2>/dev/null || true)
  FINISH=$(python3 -c "import json; d=json.load(open('/tmp/p234_chat.json')); print(d.get('choices',[{}])[0].get('finish_reason',''))" 2>/dev/null || true)

  QUOTA_HIT=0
  if [[ "$CHAT" == "200" && -n "$CONTENT" ]]; then
    pass "upstream completion ok (content present)"
  elif [[ "$CHAT" != "200" ]] && upstream_quota_exhausted /tmp/p234_chat.json; then
    skip "upstream completion (quota exhausted)"
    QUOTA_HIT=1
  elif [[ "$CHAT" == "200" && -z "$CONTENT" && "$FINISH" == "length" ]]; then
    skip "upstream completion (empty output; $MODEL hit max_tokens/thinking budget)"
    QUOTA_HIT=1
  else
    fail "upstream completion returned empty content: HTTP $CHAT $(cat /tmp/p234_chat.json)"
  fi

  if [[ "$QUOTA_HIT" -eq 0 ]]; then
    STREAM_CODE=$(curl -s -o /tmp/p234_stream.txt -w "%{http_code}" -X POST "$GW_URL/v1/chat/completions" \
      -H "Authorization: Bearer $EVAL_SECRET" \
      -H 'Content-Type: application/json' \
      -d '{"model":"'"$MODEL"'","stream":true,"messages":[{"role":"user","content":"Reply with exactly: ok"}],"max_tokens":64}')
    if [[ "$STREAM_CODE" == "200" ]] && grep -q '\[DONE\]' /tmp/p234_stream.txt 2>/dev/null; then
      pass "streaming completion -> 200 with [DONE]"
    elif upstream_quota_exhausted /tmp/p234_stream.txt; then
      skip "streaming completion (quota exhausted)"
    else
      fail "streaming completion failed: HTTP $STREAM_CODE $(head -c 200 /tmp/p234_stream.txt)"
    fi
  fi

  # Wait for async eval worker to flush to ClickHouse.
  echo "  waiting for eval_scores..."
  EVAL_FOUND=0
  for i in $(seq 1 20); do
    COUNT=$(curl -s "http://localhost:8123/?user=nexus&password=nexus" \
      --data-binary "SELECT count() FROM nexus.eval_scores WHERE evaluator IN ('heuristic_pii','heuristic_completeness')" 2>/dev/null || echo 0)
    if [[ "${COUNT:-0}" -gt 0 ]]; then
      EVAL_FOUND=1
      break
    fi
    sleep 1
  done
  if [[ "$EVAL_FOUND" == "1" ]]; then
    pass "heuristic eval scores in ClickHouse (count=$COUNT)"
  else
    fail "no heuristic eval_scores found in ClickHouse after 20s"
  fi

  # PII heuristic: inject a trace-like eval by calling with a prompt that might
  # cause PII in output is unreliable; instead verify schema via a direct row.
  PII_ROW=$(curl -s "http://localhost:8123/?user=nexus&password=nexus" \
    --data-binary "SELECT metric, score FROM nexus.eval_scores WHERE metric IN ('pii_leak','completeness') LIMIT 5 FORMAT TabSeparated" 2>/dev/null || true)
  if echo "$PII_ROW" | grep -qE 'pii_leak|completeness'; then
    pass "eval metrics include pii_leak/completeness"
  else
    fail "expected pii_leak/completeness metrics, got: $PII_ROW"
  fi
else
  echo "  SKIP upstream/eval tests (no provider key)"
fi

# --- Phase 4: quality-aware routing ---

echo ""
echo "== Phase 4: quality-aware routing =="

ROUTE_KEY_JSON=$(curl -s -b "$ADMIN_JAR" -X POST "$CON_URL/api/keys" \
  -H 'Content-Type: application/json' \
  -d '{"name":"e2e-route","allowed_models":["'"$MODEL"'","auto","fast"]}')
ROUTE_SECRET=$(echo "$ROUTE_KEY_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['secret'])")

ROUTING=$(http_body "$CON_URL/api/routing")
if echo "$ROUTING" | python3 -c "import sys,json; json.load(sys.stdin)" 2>/dev/null; then
  pass "GET /api/routing returns JSON array"
else
  fail "GET /api/routing invalid JSON: $ROUTING"
fi

if [[ "$HAS_PROVIDER" == "1" ]]; then
  AUTO_CODE=$(curl -s -o /tmp/p234_auto.json -w "%{http_code}" -X POST "$GW_URL/v1/chat/completions" \
    -H "Authorization: Bearer $ROUTE_SECRET" \
    -H 'Content-Type: application/json' \
    -d '{"model":"auto","messages":[{"role":"user","content":"Say ok"}],"max_tokens":16}')
  if [[ "$AUTO_CODE" == "200" ]]; then
    pass "model=auto routes to a provider successfully"
  elif upstream_quota_exhausted /tmp/p234_auto.json; then
    skip "model=auto routing (quota exhausted)"
  else
    fail "model=auto failed: HTTP $AUTO_CODE $(cat /tmp/p234_auto.json)"
  fi

  FAST=$(curl -s -o /tmp/p234_fast.json -w "%{http_code}" -X POST "$GW_URL/v1/chat/completions" \
    -H "Authorization: Bearer $ROUTE_SECRET" \
    -H 'Content-Type: application/json' \
    -d '{"model":"fast","messages":[{"role":"user","content":"Say ok"}],"max_tokens":16}')
  if [[ "$FAST" == "200" ]]; then
    pass "model=fast group alias -> 200"
  elif upstream_quota_exhausted /tmp/p234_fast.json; then
    skip "model=fast routing (quota exhausted)"
  else
    fail "model=fast -> expected 200, got $FAST"
  fi
else
  echo "  SKIP auto/fast routing completion tests (no provider key)"
fi

# --- Summary ---

echo ""
echo "== Summary =="
echo "  passed: $PASS"
[[ "${SKIP:-0}" -gt 0 ]] && echo "  skipped: $SKIP"
echo "  failed: $FAIL"
if [[ "$FAIL" -gt 0 ]]; then
  exit 1
fi
echo "  all tests passed"
