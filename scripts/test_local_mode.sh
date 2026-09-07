#!/usr/bin/env bash
# =============================================================================
# Local mode end-to-end smoke
# =============================================================================
# `npx -y @ffxnexus/nexus` and `docker run` both land on `nexus serve --local`,
# and the promise is that the console works with nothing else installed. Unit
# tests cover the pieces; only a real boot covers the claim.
#
# What it proves, in the order a first-time user hits it:
#   1. A clean state directory boots and reports ready (schema applied)
#   2. The first account is created and is an admin
#   3. That admin can store an encrypted provider credential
#   4. That admin can mint a virtual key, and the gateway accepts it
#   5. A restart keeps all of it — the data directory and the master key are
#      not regenerated
#   6. A SIGKILLed run leaves no wreckage the next start cannot clear
#
# Usage:  scripts/test_local_mode.sh
# Requires: go, curl, python3. Downloads a Postgres runtime on first run.
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_DIR="$(mktemp -d)"
BIN="${STATE_DIR}/nexus"
GW_PORT="${NEXUS_TEST_GATEWAY_PORT:-18080}"
CON_PORT="${NEXUS_TEST_CONSOLE_PORT:-18081}"
GW="http://127.0.0.1:${GW_PORT}"
CON="http://127.0.0.1:${CON_PORT}"
JAR="${STATE_DIR}/cookies.txt"
LOG="${STATE_DIR}/run.log"

EMAIL="owner@local.test"
PASSWORD="localpass123"

pass=0
fail=0
ok()   { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail + 1)); }
head_() { printf '\n\033[1m%s\033[0m\n' "$1"; }

NEXUS_PID=""
cleanup() {
  if [[ -n "$NEXUS_PID" ]] && kill -0 "$NEXUS_PID" 2>/dev/null; then
    kill -TERM "$NEXUS_PID" 2>/dev/null || true
    wait "$NEXUS_PID" 2>/dev/null || true
  fi
  # The child Postgres is detached from the gateway, so a failed run can leave
  # it behind. Take it down with the data directory it holds.
  if [[ -f "${STATE_DIR}/pg/data/postmaster.pid" ]]; then
    local pg_pid
    pg_pid="$(head -1 "${STATE_DIR}/pg/data/postmaster.pid" 2>/dev/null || true)"
    [[ -n "$pg_pid" ]] && kill -TERM "$pg_pid" 2>/dev/null || true
    sleep 1
  fi
  rm -rf "$STATE_DIR"
}
trap cleanup EXIT

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing dependency: $1" >&2; exit 127; }; }
need go
need curl
need python3

jqish() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)" 2>/dev/null || true; }

start_nexus() {
  NEXUS_LOCAL_STATE_DIR="$STATE_DIR" \
  NEXUS_GATEWAY_ADDR=":${GW_PORT}" \
  NEXUS_CONSOLE_ADDR=":${CON_PORT}" \
    "$BIN" serve --local >>"$LOG" 2>&1 &
  NEXUS_PID=$!
}

stop_nexus() {
  kill -TERM "$NEXUS_PID" 2>/dev/null || true
  wait "$NEXUS_PID" 2>/dev/null || true
  NEXUS_PID=""
}

# The first boot also runs initdb, and on a cold cache downloads the server.
wait_ready() {
  local tries="${1:-180}"
  while (( tries > 0 )); do
    if curl -fsS --max-time 2 "${CON}/readyz" >/dev/null 2>&1; then return 0; fi
    if [[ -n "$NEXUS_PID" ]] && ! kill -0 "$NEXUS_PID" 2>/dev/null; then
      echo "nexus exited during startup:" >&2
      tail -30 "$LOG" >&2
      return 1
    fi
    sleep 1
    tries=$((tries - 1))
  done
  tail -30 "$LOG" >&2
  return 1
}

# ---------------------------------------------------------------------------
head_ "0. Build"
# ---------------------------------------------------------------------------
(cd "$REPO_ROOT" && go build -o "$BIN" ./cmd/nexus)
ok "built $(basename "$BIN")"

# ---------------------------------------------------------------------------
head_ "1. Cold boot on an empty state directory"
# ---------------------------------------------------------------------------
start_nexus
if wait_ready; then
  ok "gateway came up and /readyz answered"
else
  bad "gateway never became ready"
  exit 1
fi

readyz="$(curl -fsS "${CON}/readyz")"
# Migrations run at boot in local mode; a ready postgres_schema check is how
# that shows up. Without it the console would 500 on the first write.
if grep -q '"postgres_schema"' <<<"$readyz" && grep -q '"ready":true' <<<"$readyz"; then
  ok "schema check is present and ready (migrations ran at boot)"
else
  bad "readyz did not report a ready postgres schema: $readyz"
fi

if [[ -f "${STATE_DIR}/master.key" ]]; then
  ok "master key was generated in the state directory"
  MASTER_KEY_FIRST="$(cat "${STATE_DIR}/master.key")"
else
  bad "no master.key — provider credentials cannot be encrypted"
  MASTER_KEY_FIRST=""
fi

if [[ -f "${STATE_DIR}/pg/data/PG_VERSION" ]]; then
  ok "postgres data directory initialised at ${STATE_DIR}/pg/data"
else
  bad "no postgres data directory"
fi

# ---------------------------------------------------------------------------
head_ "2. First account is the admin"
# ---------------------------------------------------------------------------
reg="$(curl -fsS -X POST "${CON}/api/auth/register" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"${EMAIL}\",\"password\":\"${PASSWORD}\"}" || true)"
role="$(jqish "d['user']['role']" <<<"$reg")"
if [[ "$role" == "admin" ]]; then
  ok "first self-signup is an admin"
else
  bad "first account has role '${role}' — the only user on the machine cannot administer it"
fi

# Everyone after the first is an ordinary member; local mode does not turn the
# console into an open admin panel.
second="$(curl -fsS -X POST "${CON}/api/auth/register" \
  -H 'Content-Type: application/json' \
  -d '{"email":"second@local.test","password":"localpass123"}' || true)"
if [[ "$(jqish "d['user']['role']" <<<"$second")" == "member" ]]; then
  ok "the second account is a member"
else
  bad "the second account was promoted too: $second"
fi

login_code="$(curl -s -o /dev/null -w '%{http_code}' -c "$JAR" \
  -X POST "${CON}/api/auth/login" -H 'Content-Type: application/json' \
  -d "{\"email\":\"${EMAIL}\",\"password\":\"${PASSWORD}\"}")"
if [[ "$login_code" == "200" ]]; then
  ok "login over plain HTTP sets a usable session cookie"
else
  bad "login returned ${login_code} — check that local mode relaxed Secure cookies"
fi

users_code="$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" "${CON}/api/users")"
if [[ "$users_code" == "200" ]]; then
  ok "admin-only routes are reachable by the first account"
else
  bad "GET /api/users returned ${users_code} for the admin"
fi

# ---------------------------------------------------------------------------
head_ "3. Credentials and virtual keys"
# ---------------------------------------------------------------------------
cred="$(curl -fsS -b "$JAR" -X POST "${CON}/api/me/credentials" \
  -H 'Content-Type: application/json' \
  -d '{"provider":"openai","name":"smoke","secret":"sk-smoke-test-key"}' || true)"
CRED_ID="$(jqish "d.get('credential',d).get('id','')" <<<"$cred")"
if [[ -n "$CRED_ID" ]]; then
  ok "provider credential stored (encrypted with the generated master key)"
else
  bad "could not store a provider credential: $cred"
fi

# The secret must never come back out of the API.
creds_list="$(curl -fsS -b "$JAR" "${CON}/api/me/credentials" || true)"
if grep -q 'sk-smoke-test-key' <<<"$creds_list"; then
  bad "the provider secret is readable back through the API"
else
  ok "the provider secret is not returned by the API"
fi

key="$(curl -fsS -b "$JAR" -X POST "${CON}/api/me/keys" \
  -H 'Content-Type: application/json' -d '{"name":"smoke"}' || true)"
VKEY="$(jqish "d.get('secret') or d.get('key_secret') or d.get('token','')" <<<"$key")"
if [[ -n "$VKEY" ]]; then
  ok "virtual key minted"
else
  bad "no virtual key secret in the response: $key"
fi

if [[ -n "$VKEY" ]]; then
  # A wrong key must be rejected, or "the gateway accepted my key" proves
  # nothing about authentication.
  bad_code="$(curl -s -o /dev/null -w '%{http_code}' "${GW}/v1/models" \
    -H "Authorization: Bearer nxs_live_not_a_real_key")"
  if [[ "$bad_code" == "401" || "$bad_code" == "403" ]]; then
    ok "the gateway rejects an unknown virtual key (${bad_code})"
  else
    bad "an unknown virtual key got ${bad_code} from /v1/models"
  fi

  good_code="$(curl -s -o /dev/null -w '%{http_code}' "${GW}/v1/models" \
    -H "Authorization: Bearer ${VKEY}")"
  if [[ "$good_code" == "200" ]]; then
    ok "the gateway accepts the minted virtual key"
  else
    bad "the minted virtual key got ${good_code} from /v1/models"
  fi
fi

# ---------------------------------------------------------------------------
head_ "4. Restart keeps everything"
# ---------------------------------------------------------------------------
stop_nexus
if grep -q "local postgres stopped" "$LOG"; then
  ok "SIGTERM shut the child postgres down cleanly"
else
  bad "no clean postgres shutdown in the log — the next start has to recover"
fi

start_nexus
if wait_ready 120; then
  ok "restarted"
else
  bad "did not come back up after a restart"
  exit 1
fi

if [[ "$(cat "${STATE_DIR}/master.key")" == "$MASTER_KEY_FIRST" ]]; then
  ok "master key survived the restart (stored credentials stay decryptable)"
else
  bad "the master key was regenerated — every stored credential is now unreadable"
fi

relogin="$(curl -s -o /dev/null -w '%{http_code}' -c "$JAR" \
  -X POST "${CON}/api/auth/login" -H 'Content-Type: application/json' \
  -d "{\"email\":\"${EMAIL}\",\"password\":\"${PASSWORD}\"}")"
if [[ "$relogin" == "200" ]]; then
  ok "the account created before the restart still logs in"
else
  bad "login after restart returned ${relogin} — the database did not persist"
fi

if curl -fsS -b "$JAR" "${CON}/api/me/credentials" | grep -q '"openai"'; then
  ok "the stored provider credential survived the restart"
else
  bad "the provider credential is gone after a restart"
fi

# ---------------------------------------------------------------------------
head_ "5. Recovery from an unclean exit"
# ---------------------------------------------------------------------------
# Closing a terminal on `npx` sends SIGHUP and the detached postgres keeps
# running. If the next start cannot reclaim the data directory, the one-liner
# is broken for everyone who does not exit with Ctrl-C.
kill -KILL "$NEXUS_PID" 2>/dev/null || true
wait "$NEXUS_PID" 2>/dev/null || true
NEXUS_PID=""
sleep 1

start_nexus
if wait_ready 120; then
  ok "started again after a SIGKILL left postgres running"
else
  bad "could not recover from an unclean exit"
fi

if curl -fsS -b "$JAR" "${CON}/readyz" | grep -q '"ready":true'; then
  ok "ready after recovery, with the same data directory"
else
  bad "not ready after recovery"
fi

printf '\n\033[1m=== local mode smoke: %d passed, %d failed ===\033[0m\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]]
