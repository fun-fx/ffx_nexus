#!/usr/bin/env bash
# E2E tests for Phase 2 control plane: virtual keys, encrypted credentials,
# audit log, revoke, and DB credential reload on restart.
set -euo pipefail

source "$(dirname "$0")/lib/e2e_common.sh"
e2e_init

echo "== Phase 2: control plane =="
command -v docker >/dev/null || { echo "docker required"; exit 1; }
command -v curl >/dev/null || { echo "curl required"; exit 1; }

wait_services
go build -o "$BIN" ./cmd/nexus
pass "build ok"

export_e2e_env
load_dotenv

MODEL="${TEST_MODEL:-gemini-2.5-flash}"
GEMINI_SECRET="${GEMINI_API_KEY:-}"
if [[ -n "$GEMINI_SECRET" ]]; then
  HAS_PROVIDER=1
  CRED_PLAINTEXT="$GEMINI_SECRET"
else
  echo "  WARN: no GEMINI_API_KEY — encryption/reload tests use a fake credential; completion skipped"
  HAS_PROVIDER=0
  CRED_PLAINTEXT="sk-e2e-fake-$(openssl rand -hex 8)"
fi

# Fixed master key so restart can decrypt the same credentials.
export NEXUS_MASTER_KEY="${NEXUS_MASTER_KEY_FIXED:-$(openssl rand -hex 32)}"

trap stop_nexus EXIT

# Start without env provider keys — providers must come from DB after credential create.
# Shared E2E admin email/password (the postgres volume is persistent across
# test scripts, so each script that needs admin access resets the password
# via raw SQL to guarantee the bootstrap env matches what we log in with).
ADMIN_EMAIL="${ADMIN_EMAIL:-admin-e2e@nexus.local}"
ADMIN_PASS="${ADMIN_PASS:-admin-e2e-pass}"

start_nexus env \
  -u GEMINI_API_KEY -u OPENAI_API_KEY -u ANTHROPIC_API_KEY \
  NEXUS_ALLOW_SIGNUP=true \
  NEXUS_ADMIN_EMAIL="$ADMIN_EMAIL" \
  NEXUS_ADMIN_PASSWORD="$ADMIN_PASS"
pass "nexus started (no env provider keys)"

# Reset the admin password so it matches what we'll log in with, regardless
# of which previous test script created the row.
docker compose -f deploy/docker-compose.yml exec -T postgres \
  psql -U nexus -d nexus -c \
  "UPDATE users SET password_hash = crypt('$ADMIN_PASS', gen_salt('bf')), role='admin' WHERE email='$ADMIN_EMAIL'" \
  >/dev/null 2>&1 || true

# --- Admin login (org-level /api/keys, /api/credentials are admin-only since v1.1).
# The bootstrap admin was created above via NEXUS_ADMIN_EMAIL/PASSWORD.
LOGIN=$(curl -s -o /dev/null -w '%{http_code}' -c "$ADMIN_JAR" -X POST "$CON_URL/api/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}")
if [[ "$LOGIN" == "200" ]]; then
  pass "admin login -> 200 + session cookie"
else
  fail "admin login failed ($LOGIN)"
  summary_exit
fi

# --- Virtual key lifecycle ---

echo ""
echo "-- virtual keys --"

KEY_JSON=$(curl -s -b "$ADMIN_JAR" -X POST "$CON_URL/api/keys" \
  -H 'Content-Type: application/json' \
  -d '{"name":"e2e-phase2","allowed_models":["'"$MODEL"'"],"rpm_limit":0}')
SECRET=$(echo "$KEY_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['secret'])")
KEY_ID=$(echo "$KEY_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['key']['id'])")
PREFIX=$(echo "$KEY_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['key']['key_prefix'])")
LAST4=$(echo "$KEY_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['key']['key_last4'])")

if [[ "$SECRET" == nxs_live_* && -n "$KEY_ID" ]]; then
  pass "virtual key created (secret returned once)"
else
  fail "virtual key create malformed: $KEY_JSON"
fi

LIST=$(curl -s -b "$ADMIN_JAR" "$CON_URL/api/keys")
if echo "$LIST" | python3 -c "
import sys,json
keys=json.load(sys.stdin)
k=next(x for x in keys if x['id']=='$KEY_ID')
assert k['key_prefix']=='$PREFIX' and k['key_last4']=='$LAST4'
" 2>/dev/null; then
  pass "list keys shows prefix/last4 only (no secret)"
else
  fail "list keys missing expected metadata"
fi

code=$(http_code -H "Authorization: Bearer $SECRET" "$GW_URL/v1/models")
if [[ "$code" == "200" ]]; then pass "valid virtual key -> 200"; else fail "valid key -> expected 200, got $code"; fi

code=$(http_code "$GW_URL/v1/models")
if [[ "$code" == "401" ]]; then pass "no auth -> 401"; else fail "no auth -> expected 401, got $code"; fi

code=$(http_code -H "Authorization: Bearer nxs_live_invalid000000000000000000000000" "$GW_URL/v1/models")
if [[ "$code" == "401" ]]; then pass "bad virtual key -> 401"; else fail "bad key -> expected 401, got $code"; fi

# --- allowed_models enforcement ---

RESTRICT_JSON=$(curl -s -b "$ADMIN_JAR" -X POST "$CON_URL/api/keys" \
  -H 'Content-Type: application/json' \
  -d '{"name":"e2e-restricted","allowed_models":["gpt-4o-mini"]}')
RESTRICT_SECRET=$(echo "$RESTRICT_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['secret'])")

code=$(curl -s -o /tmp/p2_403.json -w "%{http_code}" -X POST "$GW_URL/v1/chat/completions" \
  -H "Authorization: Bearer $RESTRICT_SECRET" \
  -H 'Content-Type: application/json' \
  -d '{"model":"'"$MODEL"'","messages":[{"role":"user","content":"hi"}],"max_tokens":32}')
err=$(python3 -c "import json; print(json.load(open('/tmp/p2_403.json')).get('error',{}).get('type',''))" 2>/dev/null || echo "")
if [[ "$code" == "403" && "$err" == "model_not_allowed" ]]; then
  pass "disallowed model -> 403 model_not_allowed"
else
  fail "disallowed model -> expected 403, got $code type=$err"
fi

# --- Provider credentials ---

echo ""
echo "-- provider credentials --"

CRED_JSON=$(curl -s -b "$ADMIN_JAR" -X POST "$CON_URL/api/credentials" \
  -H 'Content-Type: application/json' \
  -d '{"provider":"gemini","name":"e2e-gemini","secret":"'"$CRED_PLAINTEXT"'"}')
CRED_ID=$(echo "$CRED_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
CRED_LAST4=$(echo "$CRED_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['secret_last4'])")

if [[ -n "$CRED_ID" && "$CRED_LAST4" == "${CRED_PLAINTEXT: -4}" ]]; then
  pass "credential created (last4 only in response)"
else
  fail "credential create failed: $CRED_JSON"
fi

if echo "$CRED_JSON" | grep -q "$CRED_PLAINTEXT"; then
  fail "credential response leaked plaintext secret"
else
  pass "credential response does not contain plaintext"
fi

# DB must not store plaintext (check hex of ciphertext).
PLAINTEXT_HEX=$(echo -n "$CRED_PLAINTEXT" | xxd -p | tr -d '\n')
DB_HEX=$(docker compose -f deploy/docker-compose.yml exec -T postgres \
  psql -U nexus -d nexus -t -A -c \
  "SELECT encode(secret_ciphertext,'hex') FROM provider_credentials WHERE id='$CRED_ID';" 2>/dev/null | tr -d '\n')

if [[ -n "$DB_HEX" && "$DB_HEX" != *"$PLAINTEXT_HEX"* ]]; then
  pass "credential encrypted at rest (no plaintext in DB)"
else
  fail "DB may contain plaintext credential (hex check failed)"
fi

# Audit log entries.
AUDIT_COUNT=$(docker compose -f deploy/docker-compose.yml exec -T postgres \
  psql -U nexus -d nexus -t -A -c \
  "SELECT count(*) FROM audit_log WHERE action IN ('vkey.create','credential.create');" 2>/dev/null | tr -d ' ')
if [[ "${AUDIT_COUNT:-0}" -ge 2 ]]; then
  pass "audit log records key/credential actions (count=$AUDIT_COUNT)"
else
  fail "audit log missing expected entries (count=${AUDIT_COUNT:-0})"
fi

# --- Restart: load credentials from DB (no env keys) ---

echo ""
echo "-- credential reload on restart --"

stop_nexus
start_nexus env -u GEMINI_API_KEY -u OPENAI_API_KEY -u ANTHROPIC_API_KEY
pass "nexus restarted (DB credentials only)"

MODELS=$(curl -s -H "Authorization: Bearer $SECRET" "$GW_URL/v1/models")
if echo "$MODELS" | python3 -c "import sys,json; ids=[m['id'] for m in json.load(sys.stdin)['data']]; sys.exit(0 if '$MODEL' in ids else 1)" 2>/dev/null; then
  pass "gemini model registered from DB credential after restart"
else
  fail "model $MODEL not found after restart: $MODELS"
fi

CHAT=$(curl -s -o /tmp/p2_chat.json -w "%{http_code}" -X POST "$GW_URL/v1/chat/completions" \
  -H "Authorization: Bearer $SECRET" \
  -H 'Content-Type: application/json' \
  -d '{"model":"'"$MODEL"'","messages":[{"role":"user","content":"Say hi"}],"max_tokens":16}')
if [[ "$HAS_PROVIDER" == "0" ]]; then
  skip "completion via DB credential (no real provider key)"
elif [[ "$CHAT" == "200" ]]; then
  pass "completion via DB-stored credential -> 200"
elif [[ "$CHAT" == "502" ]] && grep -qE 'RESOURCE_EXHAUSTED|quota exceeded|429' /tmp/p2_chat.json 2>/dev/null; then
  skip "completion via DB credential (upstream quota exhausted)"
else
  fail "completion via DB credential failed: HTTP $CHAT $(cat /tmp/p2_chat.json)"
fi

# --- Rotate credential ---

echo ""
echo "-- credential rotation --"

# Rotate to a new secret. With a real provider key, rotate to the same value so
# the hot-reloaded provider keeps working; otherwise use a fresh fake secret.
if [[ "$HAS_PROVIDER" == "1" ]]; then
  NEW_SECRET="$CRED_PLAINTEXT"
else
  NEW_SECRET="sk-e2e-rot-$(openssl rand -hex 8)"
fi

ROT_JSON=$(curl -s -b "$ADMIN_JAR" -X POST "$CON_URL/api/credentials/$CRED_ID/rotate" \
  -H 'Content-Type: application/json' \
  -d '{"secret":"'"$NEW_SECRET"'"}')

if echo "$ROT_JSON" | python3 -c "
import sys,json
c=json.load(sys.stdin)
assert c['id']=='$CRED_ID'
assert c['secret_last4']=='${NEW_SECRET: -4}'
assert c.get('rotated_at')
" 2>/dev/null; then
  pass "credential rotated (new last4 + rotated_at set)"
else
  fail "rotation response malformed: $ROT_JSON"
fi

if echo "$ROT_JSON" | grep -q "$NEW_SECRET"; then
  fail "rotation response leaked plaintext secret"
else
  pass "rotation response does not contain plaintext"
fi

# Ciphertext at rest must change after rotation (and still no plaintext).
NEW_DB_HEX=$(docker compose -f deploy/docker-compose.yml exec -T postgres \
  psql -U nexus -d nexus -t -A -c \
  "SELECT encode(secret_ciphertext,'hex') FROM provider_credentials WHERE id='$CRED_ID';" 2>/dev/null | tr -d '\n')
NEW_PLAINTEXT_HEX=$(echo -n "$NEW_SECRET" | xxd -p | tr -d '\n')

if [[ -n "$NEW_DB_HEX" && "$NEW_DB_HEX" != "$DB_HEX" && "$NEW_DB_HEX" != *"$NEW_PLAINTEXT_HEX"* ]]; then
  pass "rotation re-encrypted secret at rest (ciphertext changed, no plaintext)"
else
  fail "rotation ciphertext check failed (old=$DB_HEX new=$NEW_DB_HEX)"
fi

ROT_AUDIT=$(docker compose -f deploy/docker-compose.yml exec -T postgres \
  psql -U nexus -d nexus -t -A -c \
  "SELECT count(*) FROM audit_log WHERE action='credential.rotate';" 2>/dev/null | tr -d ' ')
if [[ "${ROT_AUDIT:-0}" -ge 1 ]]; then
  pass "audit log records credential.rotate"
else
  fail "audit log missing credential.rotate (count=${ROT_AUDIT:-0})"
fi

# Hot-reload: a completion should still work after rotation, without restart.
ROT_CHAT=$(curl -s -o /tmp/p2_rot_chat.json -w "%{http_code}" -X POST "$GW_URL/v1/chat/completions" \
  -H "Authorization: Bearer $SECRET" \
  -H 'Content-Type: application/json' \
  -d '{"model":"'"$MODEL"'","messages":[{"role":"user","content":"Say hi"}],"max_tokens":16}')
if [[ "$HAS_PROVIDER" == "0" ]]; then
  skip "completion after rotation (no real provider key)"
elif [[ "$ROT_CHAT" == "200" ]]; then
  pass "completion works after rotation (hot-reload, no restart)"
elif [[ "$ROT_CHAT" == "502" ]] && grep -qE 'RESOURCE_EXHAUSTED|quota exceeded|429' /tmp/p2_rot_chat.json 2>/dev/null; then
  skip "completion after rotation (upstream quota exhausted)"
else
  fail "completion after rotation failed: HTTP $ROT_CHAT $(cat /tmp/p2_rot_chat.json)"
fi

# Rotating a non-existent credential -> 404.
code=$(curl -s -o /dev/null -w "%{http_code}" -b "$ADMIN_JAR" -X POST "$CON_URL/api/credentials/does-not-exist/rotate" \
  -H 'Content-Type: application/json' -d '{"secret":"whatever"}')
if [[ "$code" == "404" ]]; then pass "rotate unknown credential -> 404"; else fail "rotate unknown -> expected 404, got $code"; fi

# --- Revoke virtual key (after rotation so hot-reload still has a valid vkey) ---

echo ""
echo "-- revoke --"

curl -s -b "$ADMIN_JAR" -X DELETE "$CON_URL/api/keys/$KEY_ID" >/dev/null
code=$(http_code -H "Authorization: Bearer $SECRET" "$GW_URL/v1/models")
if [[ "$code" == "401" ]]; then pass "revoked key -> 401"; else fail "revoked key -> expected 401, got $code"; fi

# --- Delete credential ---

curl -s -b "$ADMIN_JAR" -X DELETE "$CON_URL/api/credentials/$CRED_ID" >/dev/null
CREDS=$(curl -s -b "$ADMIN_JAR" "$CON_URL/api/credentials")
if echo "$CREDS" | python3 -c "import sys,json; ids=[c['id'] for c in json.load(sys.stdin)]; sys.exit(0 if '$CRED_ID' not in ids else 1)" 2>/dev/null; then
  pass "credential deleted"
else
  fail "credential still listed after delete"
fi

AUDIT_DEL=$(docker compose -f deploy/docker-compose.yml exec -T postgres \
  psql -U nexus -d nexus -t -A -c \
  "SELECT count(*) FROM audit_log WHERE action IN ('vkey.revoke','credential.delete');" 2>/dev/null | tr -d ' ')
if [[ "${AUDIT_DEL:-0}" -ge 2 ]]; then
  pass "audit log records revoke/delete"
else
  fail "audit log missing revoke/delete (count=${AUDIT_DEL:-0})"
fi

summary_exit
