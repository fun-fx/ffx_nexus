#!/usr/bin/env bash
# E2E tests for BYOK + multi-tenancy: bootstrap admin, login/session, self-service
# virtual keys & provider credentials, the per-user budget toggle, admin user
# management, and RBAC scoping.
set -euo pipefail

source "$(dirname "$0")/lib/e2e_common.sh"
e2e_init

NEXUS_LOG="/tmp/nexus_byok.log"
JAR="/tmp/nexus_byok_cookies.txt"
ADMIN_EMAIL="admin@nexus.test"
ADMIN_PASS="admin-secret-123"

start_nexus_logged() {
  stop_nexus
  : >"$NEXUS_LOG"
  env "$@" "$BIN" >"$NEXUS_LOG" 2>&1 &
  NEXUS_PID=$!
  for i in $(seq 1 40); do
    curl -sf "$CON_URL/healthz" >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  echo "nexus failed to start; logs:"
  cat "$NEXUS_LOG"
  return 1
}

cleanup() {
  stop_nexus
  rm -f "$JAR"
}
trap cleanup EXIT

echo "== BYOK + multi-tenancy =="

go build -o "$BIN" ./cmd/nexus
pass "build ok"

if ! docker info >/dev/null 2>&1; then
  skip "BYOK e2e (docker not available; needs Postgres)"
  summary_exit
  exit 0
fi

load_dotenv
export_e2e_env
wait_services

# Boot with a bootstrap admin and BYOK key mode.
start_nexus_logged \
  NEXUS_KEY_MODE=byok \
  NEXUS_ADMIN_EMAIL="$ADMIN_EMAIL" \
  NEXUS_ADMIN_PASSWORD="$ADMIN_PASS"

if grep -q "bootstrap admin user created" "$NEXUS_LOG" || grep -q "per-request credential resolution enabled" "$NEXUS_LOG"; then
  pass "started in BYOK mode with bootstrap admin"
else
  # Admin may already exist from a prior run; that's fine.
  skip "bootstrap admin log not seen (admin may already exist)"
fi

api() {
  # api METHOD PATH [json-body]  -> body on stdout, sets HTTP_CODE
  local method="$1" path="$2" body="${3:-}"
  local -a args=(-s -o /tmp/byok_resp.json -w "%{http_code}" -b "$JAR" -c "$JAR"
    -X "$method" "$CON_URL$path" -H 'Content-Type: application/json')
  [[ -n "$body" ]] && args+=(-d "$body")
  HTTP_CODE=$(curl "${args[@]}")
  cat /tmp/byok_resp.json
}

# --- 1. Unauthenticated access is rejected ---
api GET /api/me >/dev/null
if [[ "$HTTP_CODE" == "401" ]]; then
  pass "GET /api/me without session -> 401"
else
  fail "expected 401 for unauthenticated /api/me, got $HTTP_CODE"
fi

# --- 2. Login as admin ---
api POST /api/auth/login "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}" >/dev/null
if [[ "$HTTP_CODE" == "200" ]]; then
  pass "admin login -> 200 + session cookie"
else
  fail "admin login failed ($HTTP_CODE); logs:"
  cat "$NEXUS_LOG"
  summary_exit
fi

# --- 3. /api/me returns the admin identity ---
ME=$(api GET /api/me)
if echo "$ME" | grep -q "\"email\":\"$ADMIN_EMAIL\"" && echo "$ME" | grep -q '"role":"admin"'; then
  pass "/api/me returns admin identity"
else
  fail "/api/me unexpected: $ME"
fi

# --- 4. Budget toggle (enforce_limits off then on) ---
OFF=$(api PATCH /api/me '{"enforce_limits":false}')
ON=$(api PATCH /api/me '{"enforce_limits":true}')
if echo "$OFF" | grep -q '"enforce_limits":false' && echo "$ON" | grep -q '"enforce_limits":true'; then
  pass "per-user budget toggle (off -> on)"
else
  fail "budget toggle unexpected: off=$OFF on=$ON"
fi

# --- 5. Self-service virtual key ---
KEY_RESP=$(api POST /api/me/keys '{"name":"byok-e2e-key"}')
SECRET=$(echo "$KEY_RESP" | sed -n 's/.*"secret":"\([^"]*\)".*/\1/p')
if [[ "$HTTP_CODE" == "201" && -n "$SECRET" ]]; then
  pass "created self-service virtual key (nxs secret returned once)"
else
  fail "create my key failed ($HTTP_CODE): $KEY_RESP"
fi

LIST_KEYS=$(api GET /api/me/keys)
if echo "$LIST_KEYS" | grep -q '"name":"byok-e2e-key"'; then
  pass "self-service key appears in /api/me/keys"
else
  fail "my keys list missing created key: $LIST_KEYS"
fi

# --- 6. Self-service BYOK provider credential ---
CRED_RESP=$(api POST /api/me/credentials '{"provider":"openai","name":"my-openai","secret":"sk-byok-e2e-fake"}')
if [[ "$HTTP_CODE" == "201" ]] && echo "$CRED_RESP" | grep -q '"provider":"openai"'; then
  pass "created BYOK provider credential"
  if echo "$CRED_RESP" | grep -q 'sk-byok-e2e-fake'; then
    fail "plaintext secret leaked in credential response"
  else
    pass "plaintext secret not returned after creation"
  fi
elif [[ "$HTTP_CODE" == "503" ]]; then
  skip "BYOK credential create (NEXUS_MASTER_KEY not set for encryption)"
else
  fail "create my credential failed ($HTTP_CODE): $CRED_RESP"
fi

# --- 7. Admin user management + RBAC ---
MEMBER_EMAIL="member@nexus.test"
NEW_USER=$(api POST /api/users "{\"email\":\"$MEMBER_EMAIL\",\"password\":\"member-pass-123\",\"role\":\"member\"}")
if [[ "$HTTP_CODE" == "201" || "$HTTP_CODE" == "409" ]]; then
  pass "admin can create users (or already exists)"
else
  fail "create user failed ($HTTP_CODE): $NEW_USER"
fi

# --- 8. Member cannot access admin endpoints (RBAC) ---
MEMBER_JAR="/tmp/nexus_byok_member.txt"
curl -s -o /dev/null -c "$MEMBER_JAR" -X POST "$CON_URL/api/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$MEMBER_EMAIL\",\"password\":\"member-pass-123\"}"
MCODE=$(curl -s -o /dev/null -w "%{http_code}" -b "$MEMBER_JAR" "$CON_URL/api/users")
rm -f "$MEMBER_JAR"
if [[ "$MCODE" == "403" ]]; then
  pass "member blocked from admin /api/users (403)"
else
  fail "expected 403 for member on /api/users, got $MCODE"
fi

# --- 9. Logout invalidates the session ---
api POST /api/auth/logout >/dev/null
api GET /api/me >/dev/null
if [[ "$HTTP_CODE" == "401" ]]; then
  pass "logout invalidates session (/api/me -> 401)"
else
  fail "expected 401 after logout, got $HTTP_CODE"
fi

summary_exit
