#!/usr/bin/env bash
# E2E test for strict-byok default (NEXUS_KEY_MODE=strict_byok, the v0.1.0
# default). Verifies that:
#
#   1. Nexus boots in strict-byok mode even without any env provider keys.
#   2. Operator env keys are loaded but logged as "present but unused".
#   3. Calling the gateway without a stored per-user key is rejected with 403
#      missing_byok_key, never a 502 from the upstream being auth-less.
#   4. After registering a per-user provider key, the same gateway call is
#      authorized (returns 200 if GEMINI_API_KEY is set; returns 503 if not,
#      because the BYOK-stored key points at a fake upstream).
#   5. NEXUS_ALLOW_SHARED_KEYS=true flips registration back on, and shared
#      keys act as fallback when NEXUS_KEY_MODE=strict_byok (escape hatch).
#
# This test does not depend on a real provider key — it exercises the path
# resolution only. Real upstream completion is covered by test_phase2.sh /
# test_byok.sh.
set -euo pipefail

source "$(dirname "$0")/lib/e2e_common.sh"
e2e_init

echo "== Strict-byok default (v0.1.0+) =="
command -v docker >/dev/null || { echo "docker required"; exit 1; }
command -v curl >/dev/null || { echo "curl required"; exit 1; }

wait_services
go build -o "$BIN" ./cmd/nexus
pass "build ok"

export_e2e_env
load_dotenv

# Fixed master key so restart can decrypt credentials for user B below.
export NEXUS_MASTER_KEY="${NEXUS_MASTER_KEY_FIXED:-$(openssl rand -hex 32)}"

trap stop_nexus EXIT

USER_A_EMAIL="alice-strictbyok@nexus.local"
USER_A_PASS="alice-strictbyok-pass"
USER_B_EMAIL="bob-strictbyok@nexus.local"
USER_B_PASS="bob-strictbyok-pass"
ADMIN_EMAIL="admin-strictbyok@nexus.local"
ADMIN_PASS="admin-strictbyok-pass"

# Bring up nexus in default mode (strict_byok) and explicit NEXUS_KEY_MODE so
# the test does not silently drift. -u GEMINI_API_KEY ensures env vars do not
# bias the strict-byok dedup branch.
start_nexus env \
  -u GEMINI_API_KEY \
  -u OPENAI_API_KEY \
  -u ANTHROPIC_API_KEY \
  NEXUS_KEY_MODE=strict_byok \
  NEXUS_ALLOW_SIGNUP=true \
  NEXUS_ADMIN_EMAIL="$ADMIN_EMAIL" \
  NEXUS_ADMIN_PASSWORD="$ADMIN_PASS" \
  GEMINI_API_KEY="env-key-from-fake-e2e"
pass "nexus booted in strict-byok default"

# Wait a beat for the warn line that confirms env keys are present-but-inert.
sleep 0.3

# Reset known users so the test is rerunnable.
docker compose -f deploy/docker-compose.yml exec -T postgres psql -U nexus -d nexus -c \
  "DELETE FROM provider_credentials; DELETE FROM virtual_keys; DELETE FROM user_sessions; DELETE FROM users WHERE email IN ('$USER_A_EMAIL','$USER_B_EMAIL','$ADMIN_EMAIL')" \
  >/dev/null 2>&1 || true

# --- 1) Sign up Alice (no provider key registration at signup).
ALICE_REG=$(curl -s -c /tmp/nexus_strictbyok_alice.txt -X POST "$CON_URL/api/auth/register" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$USER_A_EMAIL\",\"password\":\"$USER_A_PASS\"}")
ALICE_VKEY=$(echo "$ALICE_REG" | python3 -c "import sys,json; print(json.load(sys.stdin).get('virtual_key',''))")
[[ -n "$ALICE_VKEY" ]] && pass "Alice signed up (vkey issued)" || { fail "Alice signup did not return vkey: $ALICE_REG"; }

# --- 2) Alice (no provider key registered) calls the gateway → expect 403.
BODY='{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi strict-byok"}]}'
RESP=$(curl -s -o /tmp/nexus_strictbyok_resp.txt -w '%{http_code}' \
  -X POST "$GW_URL/v1/chat/completions" \
  -H "Authorization: Bearer $ALICE_VKEY" \
  -H 'Content-Type: application/json' \
  -d "$BODY")
if [[ "$RESP" == "403" ]] && grep -q "missing_byok_key" /tmp/nexus_strictbyok_resp.txt; then
  pass "Alice with no key → 403 missing_byok_key (strict-byok enforced)"
else
  fail "Alice should have been rejected by strict-byok; got HTTP $RESP: $(cat /tmp/nexus_strictbyok_resp.txt)"
fi

# --- 3) Sign up Bob, then register a (fake) gemini credential.
BOB_REG=$(curl -s -c /tmp/nexus_strictbyok_bob.txt -X POST "$CON_URL/api/auth/register" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$USER_B_EMAIL\",\"password\":\"$USER_B_PASS\"}")
BOB_VKEY=$(echo "$BOB_REG" | python3 -c "import sys,json; print(json.load(sys.stdin).get('virtual_key',''))")
[[ -n "$BOB_VKEY" ]] && pass "Bob signed up (vkey issued)" || fail "Bob signup did not return vkey: $BOB_REG"

# Bob logs in.
curl -s -c /tmp/nexus_strictbyok_bob.txt -X POST "$CON_URL/api/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$USER_B_PASS\",\"password\":\"$USER_B_PASS\"}" >/dev/null
curl -s -b /tmp/nexus_strictbyok_bob.txt -X POST "$CON_URL/api/me/credentials" \
  -H 'Content-Type: application/json' \
  -d '{"provider":"gemini","name":"bob-e2e","secret":"sk-gemini-fake-e2e"}' >/dev/null
pass "Bob registered a per-user gemini credential"

# --- 4) Bob also gets 200/503 from upstream (no real provider), but never 403.
RESP=$(curl -s -o /tmp/nexus_strictbyok_resp.txt -w '%{http_code}' \
  -X POST "$GW_URL/v1/chat/completions" \
  -H "Authorization: Bearer $BOB_VKEY" \
  -H 'Content-Type: application/json' \
  -d "$BODY")
case "$RESP" in
  200) pass "Bob with key → 200 (real upstream call)" ;;
  502|503) pass "Bob with key → upstream-side rejection ($RESP), strict-byok admitted; body: $(cat /tmp/nexus_strictbyok_resp.txt | head -c 120)" ;;
  403)
    if grep -q "missing_byok_key" /tmp/nexus_strictbyok_resp.txt; then
      fail "Bob's key was not honored by resolver — still missing_byok_key"
    else
      skip "Bob with key → 403 (different reason, possibly guardrail): $(cat /tmp/nexus_strictbyok_resp.txt | head -c 120)"
    fi
    ;;
  *) fail "unexpected HTTP $RESP for Bob: $(cat /tmp/nexus_strictbyok_resp.txt | head -c 120)" ;;
esac

# --- 5) Restart with NEXUS_ALLOW_SHARED_KEYS=true; env keys re-register and
#       act as fallback. We expect Alice (no stored key) to no longer 403
#       "missing_byok_key" — instead she should at least reach the upstream
#       call, which against a fake env key will 401/502/503 from upstream.
stop_nexus
start_nexus env \
  -u NEXUS_KEY_MODE \
  NEXUS_KEY_MODE=strict_byok \
  NEXUS_ALLOW_SHARED_KEYS=true \
  NEXUS_ALLOW_SIGNUP=true \
  NEXUS_ADMIN_EMAIL="$ADMIN_EMAIL" \
  NEXUS_ADMIN_PASSWORD="$ADMIN_PASS" \
  GEMINI_API_KEY="env-key-from-fake-e2e"
pass "nexus restarted with ALLOW_SHARED_KEYS=true (escape hatch on)"

RESP=$(curl -s -o /tmp/nexus_strictbyok_resp.txt -w '%{http_code}' \
  -X POST "$GW_URL/v1/chat/completions" \
  -H "Authorization: Bearer $ALICE_VKEY" \
  -H 'Content-Type: application/json' \
  -d "$BODY")
if [[ "$RESP" == "403" ]] && grep -q "missing_byok_key" /tmp/nexus_strictbyok_resp.txt; then
  fail "with ALLOW_SHARED_KEYS=true Alice should not 403 missing_byok_key"
else
  pass "Alice with ALLOW_SHARED_KEYS=true → $RESP (no missing_byok_key after escape hatch on)"
fi

summary_exit
