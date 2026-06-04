# Shared helpers for Nexus E2E test scripts. Source from scripts/: 
#   source "$(dirname "$0")/lib/e2e_common.sh"

e2e_init() {
  ROOT="$(cd "$(dirname "${BASH_SOURCE[1]}")/.." && pwd)"
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
}

pass() { PASS=$((PASS + 1)); echo "  ✓ $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  ✗ $1"; }

stop_nexus() {
  if [[ -n "${NEXUS_PID:-}" ]] && kill -0 "$NEXUS_PID" 2>/dev/null; then
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
  # Optional env overrides passed as arguments: KEY=VAL KEY2=VAL2 command
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
  return 1
}

wait_services() {
  docker compose -f deploy/docker-compose.yml up -d postgres clickhouse redis >/dev/null 2>&1
  echo "  waiting for postgres..."
  for i in $(seq 1 30); do
    docker compose -f deploy/docker-compose.yml exec -T postgres pg_isready -U nexus >/dev/null 2>&1 && break
    sleep 1
  done
  echo "  waiting for clickhouse..."
  for i in $(seq 1 30); do
    curl -sf http://localhost:8123/ping >/dev/null 2>&1 && break
    sleep 1
  done
  echo "  waiting for redis..."
  for i in $(seq 1 15); do
    docker compose -f deploy/docker-compose.yml exec -T redis redis-cli ping 2>/dev/null | grep -q PONG && break
    sleep 0.5
  done
}

load_dotenv() {
  if [[ -f .env ]]; then
    set -a
    # shellcheck disable=SC1091
    source .env
    set +a
  fi
}

export_e2e_env() {
  export NEXUS_GATEWAY_ADDR="$GW"
  export NEXUS_CONSOLE_ADDR="$CON"
  export NEXUS_POSTGRES_URL="${NEXUS_POSTGRES_URL:-postgres://nexus:nexus@localhost:5433/nexus?sslmode=disable}"
  export NEXUS_CLICKHOUSE_URL="${NEXUS_CLICKHOUSE_URL:-clickhouse://nexus:nexus@localhost:9000/nexus}"
  export NEXUS_REDIS_URL="${NEXUS_REDIS_URL:-redis://localhost:6379/0}"
  export NEXUS_MASTER_KEY="${NEXUS_MASTER_KEY:-$(openssl rand -hex 32)}"
  export NEXUS_ROUTE_GROUPS="${NEXUS_ROUTE_GROUPS:-fast=gemini-2.5-flash}"
  export NEXUS_ROUTE_REFRESH="${NEXUS_ROUTE_REFRESH:-5s}"
}

http_code() {
  curl -s -o /dev/null -w "%{http_code}" "$@"
}

# POST chat/completions; retry once after quota backoff. Returns body on stdout, sets CHAT_HTTP_CODE.
# Exit 2 when upstream quota is exhausted after retry (callers may skip).
curl_chat_completion() {
  local url="$1" auth_header="${2:-}" body="$3" outfile="${4:-/tmp/e2e_chat.json}"
  local -a hdr=(-H 'Content-Type: application/json')
  [[ -n "$auth_header" ]] && hdr+=(-H "Authorization: Bearer $auth_header")

  local attempt code
  for attempt in 1 2; do
    code=$(curl -s -o "$outfile" -w "%{http_code}" -X POST "$url" "${hdr[@]}" -d "$body")
    CHAT_HTTP_CODE="$code"
    if [[ "$code" == "200" ]]; then
      cat "$outfile"
      return 0
    fi
    if [[ "$attempt" == 1 ]] && grep -qE 'RESOURCE_EXHAUSTED|quota exceeded|429' "$outfile" 2>/dev/null; then
      echo "  (upstream quota hit, waiting 65s before retry...)" >&2
      sleep 65
      continue
    fi
    cat "$outfile"
    if grep -qE 'RESOURCE_EXHAUSTED|quota exceeded' "$outfile" 2>/dev/null; then
      return 2
    fi
    return 1
  done
}

skip() { SKIP=$((SKIP + 1)); echo "  ⊘ SKIP: $1"; }

summary_exit() {
  echo ""
  echo "== Summary =="
  echo "  passed: $PASS"
  [[ "${SKIP:-0}" -gt 0 ]] && echo "  skipped: $SKIP"
  echo "  failed: $FAIL"
  if [[ "$FAIL" -gt 0 ]]; then
    exit 1
  fi
  echo "  all tests passed"
}
