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
  NEXUS_ALLOW_SIGNUP=true \
  NEXUS_ADMIN_EMAIL="$ADMIN_EMAIL" \
  NEXUS_ADMIN_PASSWORD="$ADMIN_PASS"

if grep -q "bootstrap admin user created" "$NEXUS_LOG" || grep -q "per-request credential resolution enabled" "$NEXUS_LOG"; then
  pass "started in BYOK mode with bootstrap admin"
else
  # Admin may already exist from a prior run; that's fine.
  skip "bootstrap admin log not seen (admin may already exist)"
fi

api() {
  # api METHOD PATH [json-body]  -> body on stdout, status in HTTP_CODE.
  # The status is also written to a file so callers can read it after a
  # command substitution ($(api ...)) runs api() in a subshell, where a plain
  # variable assignment would not propagate back to the parent shell.
  local method="$1" path="$2" body="${3:-}"
  local -a args=(-s -o /tmp/byok_resp.json -w "%{http_code}" -b "$JAR" -c "$JAR"
    -X "$method" "$CON_URL$path" -H 'Content-Type: application/json')
  [[ -n "$body" ]] && args+=(-d "$body")
  HTTP_CODE=$(curl "${args[@]}")
  printf '%s' "$HTTP_CODE" >/tmp/byok_code.txt
  cat /tmp/byok_resp.json
}

# code returns the status of the most recent api() call, surviving subshells.
code() { cat /tmp/byok_code.txt 2>/dev/null; }

# --- 1. Unauthenticated access is rejected ---
api GET /api/me >/dev/null
if [[ "$HTTP_CODE" == "401" ]]; then
  pass "GET /api/me without session -> 401"
else
  fail "expected 401 for unauthenticated /api/me, got $HTTP_CODE"
fi

# --- 1b. Self-service signup ---
SIGNUP_EMAIL="signup@nexus.test"
SIGNUP_PASS="signup-pass-123"
CONFIG=$(curl -sk "$CON_URL/api/auth/config")
if echo "$CONFIG" | grep -q '"signup_enabled":true'; then
  pass "signup enabled in /api/auth/config"
else
  fail "expected signup_enabled true: $CONFIG"
fi

BAD_PW=$(api POST /api/auth/register "{\"email\":\"bad@nexus.test\",\"password\":\"short\"}")
if [[ "$(code)" == "400" ]]; then
  pass "register rejects short password -> 400"
else
  fail "expected 400 for short password, got $(code): $BAD_PW"
fi

SIGNUP_RESP=$(api POST /api/auth/register "{\"email\":\"$SIGNUP_EMAIL\",\"password\":\"$SIGNUP_PASS\",\"provider\":\"openai\",\"provider_secret\":\"sk-signup-e2e-fake\"}")
SIGNUP_SECRET=$(echo "$SIGNUP_RESP" | sed -n 's/.*"virtual_key":"\([^"]*\)".*/\1/p')
if [[ "$(code)" == "201" || "$(code)" == "409" ]]; then
  pass "self-service register (201 or 409 if already exists)"
  if [[ -n "$SIGNUP_SECRET" ]]; then
    pass "register returned virtual_key once"
  elif [[ "$(code)" == "409" ]]; then
    skip "virtual_key not returned (user already registered)"
  else
    fail "register missing virtual_key: $SIGNUP_RESP"
  fi
else
  fail "register failed ($(code)): $SIGNUP_RESP"
fi

# Fresh session for signup user (register sets cookie on 201).
SIGNUP_JAR="/tmp/nexus_byok_signup.txt"
curl -s -o /dev/null -c "$SIGNUP_JAR" -X POST "$CON_URL/api/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$SIGNUP_EMAIL\",\"password\":\"$SIGNUP_PASS\"}"
SIGNUP_ME=$(curl -sk -b "$SIGNUP_JAR" "$CON_URL/api/me")
if echo "$SIGNUP_ME" | grep -q '"role":"member"'; then
  pass "registered user has member role"
else
  fail "signup user role unexpected: $SIGNUP_ME"
fi
rm -f "$SIGNUP_JAR"

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
if [[ "$(code)" == "201" && -n "$SECRET" ]]; then
  pass "created self-service virtual key (nxs secret returned once)"
else
  fail "create my key failed ($(code)): $KEY_RESP"
fi

LIST_KEYS=$(api GET /api/me/keys)
if echo "$LIST_KEYS" | grep -q '"name":"byok-e2e-key"'; then
  pass "self-service key appears in /api/me/keys"
else
  fail "my keys list missing created key: $LIST_KEYS"
fi

# --- 6. Self-service BYOK provider credential ---
CRED_RESP=$(api POST /api/me/credentials '{"provider":"openai","name":"my-openai","secret":"sk-byok-e2e-fake"}')
if [[ "$(code)" == "201" ]] && echo "$CRED_RESP" | grep -q '"provider":"openai"'; then
  pass "created BYOK provider credential"
  if echo "$CRED_RESP" | grep -q 'sk-byok-e2e-fake'; then
    fail "plaintext secret leaked in credential response"
  else
    pass "plaintext secret not returned after creation"
  fi
elif [[ "$(code)" == "503" ]]; then
  skip "BYOK credential create (NEXUS_MASTER_KEY not set for encryption)"
else
  fail "create my credential failed ($(code)): $CRED_RESP"
fi

# --- 7. Admin user management + RBAC ---
MEMBER_EMAIL="member@nexus.test"
NEW_USER=$(api POST /api/users "{\"email\":\"$MEMBER_EMAIL\",\"password\":\"member-pass-123\",\"role\":\"member\"}")
if [[ "$(code)" == "201" || "$(code)" == "409" ]]; then
  pass "admin can create users (or already exists)"
else
  fail "create user failed ($(code)): $NEW_USER"
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

# --- 8b. Per-user quality endpoint (eval differentiator) ---
api GET /api/users/quality >/dev/null
if [[ "$HTTP_CODE" == "200" ]]; then
  pass "admin can read per-user quality (/api/users/quality)"
else
  fail "expected 200 for /api/users/quality, got $HTTP_CODE"
fi

# --- 8c. Self-service usage endpoints (member scoped) ---
MEMBER2_JAR="/tmp/nexus_byok_member2.txt"
rm -f "$MEMBER2_JAR"
curl -s -o /dev/null -c "$MEMBER2_JAR" -X POST "$CON_URL/api/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$MEMBER_EMAIL\",\"password\":\"member-pass-123\"}"
MSTATS=$(curl -sk -b "$MEMBER2_JAR" "$CON_URL/api/me/stats?window=1h")
MTRACES=$(curl -sk -b "$MEMBER2_JAR" -o /dev/null -w "%{http_code}" "$CON_URL/api/me/traces?limit=5")
MQUAL=$(curl -sk -b "$MEMBER2_JAR" -o /dev/null -w "%{http_code}" "$CON_URL/api/me/quality?window=24h")
if echo "$MSTATS" | python3 -c "import json,sys; json.load(sys.stdin)" 2>/dev/null; then
  pass "member /api/me/stats returns valid JSON"
else
  fail "member /api/me/stats invalid: $MSTATS"
fi
if [[ "$MTRACES" == "200" ]]; then
  pass "member /api/me/traces -> 200"
else
  fail "member /api/me/traces: expected 200, got $MTRACES"
fi
if [[ "$MQUAL" == "200" ]]; then
  pass "member /api/me/quality -> 200"
else
  fail "member /api/me/quality: expected 200, got $MQUAL"
fi
rm -f "$MEMBER2_JAR"

# --- 8d. Unauthenticated /api/me/* -> 401 ---
for path in /api/me/stats /api/me/traces /api/me/quality; do
  CODE=$(curl -sk -o /dev/null -w "%{http_code}" "$CON_URL$path")
  if [[ "$CODE" == "401" ]]; then
    pass "unauthenticated GET $path -> 401"
  else
    fail "unauthenticated GET $path: expected 401, got $CODE"
  fi
done

# --- 9. Logout invalidates the session ---
api POST /api/auth/logout >/dev/null
api GET /api/me >/dev/null
if [[ "$HTTP_CODE" == "401" ]]; then
  pass "logout invalidates session (/api/me -> 401)"
else
  fail "expected 401 after logout, got $HTTP_CODE"
fi

summary_exit
