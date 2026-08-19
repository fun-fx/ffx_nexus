#!/usr/bin/env bash
# =============================================================================
# Invite-user end-to-end check against a freshly migrated database
# =============================================================================
# This is the acceptance test for the migration rework: `POST /api/invites`
# used to return 500 on every fresh install, because
# migrations/postgres/014_invite_tokens.sql existed in the repository but was
# never added to the hardcoded migration list in main(), so invite_tokens was
# never created. Nothing caught it, because the migration error was logged at
# boot and then ignored while the pod reported healthy.
#
# The flow exercised here is the one a customer performs on day one:
#
#   migrate -> bootstrap admin -> admin logs in -> admin invites a colleague ->
#   invite link resolves -> colleague accepts -> colleague can log in as member
#
# Usage:
#   NEXUS_TEST_POSTGRES_ADMIN_URL='postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable' \
#     scripts/e2e_invite_flow.sh
#
# Creates and drops its own database; touches no external service.
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

ADMIN_URL="${NEXUS_TEST_POSTGRES_ADMIN_URL:-}"
if [[ -z "${ADMIN_URL}" ]]; then
  echo "set NEXUS_TEST_POSTGRES_ADMIN_URL to a Postgres the script may create a database in" >&2
  exit 127
fi

PSQL="${PSQL:-psql}"
DB="nexus_invite_e2e_$$"
GW_PORT="${GW_PORT:-18190}"
CONSOLE_PORT="${CONSOLE_PORT:-18191}"
ADMIN_EMAIL="admin@customer.example"
ADMIN_PASS="admin-pw-for-test-only"
INVITEE="newhire@customer.example"
INVITEE_PASS="invitee-pw-for-test-only"

BIN="$(mktemp -d)/nexus"
COOKIES="$(mktemp)"
SRV_PID=""

pass=0; fail=0
ok()  { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass+1)); }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail+1)); }

cleanup() {
  [[ -n "${SRV_PID}" ]] && kill "${SRV_PID}" 2>/dev/null || true
  [[ -n "${SRV_PID}" ]] && wait "${SRV_PID}" 2>/dev/null || true
  ${PSQL} "${ADMIN_URL}" -q -c "DROP DATABASE IF EXISTS ${DB}" >/dev/null 2>&1 || true
  rm -f "${COOKIES}"
}
trap cleanup EXIT

api() { curl -sS -b "${COOKIES}" -c "${COOKIES}" "http://127.0.0.1:${CONSOLE_PORT}$@"; }
code() { curl -sS -o /dev/null -w '%{http_code}' -b "${COOKIES}" -c "${COOKIES}" "http://127.0.0.1:${CONSOLE_PORT}$@"; }

jsonfield() { python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get(sys.argv[1],""))' "$1"; }

printf '\n\033[1m0. Build + fresh database\033[0m\n'
go build -o "${BIN}" ./cmd/nexus
${PSQL} "${ADMIN_URL}" -q -c "CREATE DATABASE ${DB}" >/dev/null
export NEXUS_POSTGRES_URL="$(python3 - "$ADMIN_URL" "$DB" <<'PY'
import sys
from urllib.parse import urlsplit, urlunsplit
u = urlsplit(sys.argv[1])
print(urlunsplit((u.scheme, u.netloc, "/" + sys.argv[2], u.query, u.fragment)))
PY
)"
ok "created database ${DB}"

printf '\n\033[1m1. nexus migrate on an empty database\033[0m\n'
if "${BIN}" migrate >/tmp/e2e_migrate.log 2>&1; then
  ok "migrate exited 0"
else
  bad "migrate failed"; cat /tmp/e2e_migrate.log; exit 1
fi
if ${PSQL} "${NEXUS_POSTGRES_URL}" -tAc \
    "SELECT to_regclass('invite_tokens') IS NOT NULL" | grep -qx t; then
  ok "invite_tokens exists (the table whose absence made invites 500)"
else
  bad "invite_tokens was not created"; exit 1
fi

printf '\n\033[1m2. Start the server\033[0m\n'
NEXUS_GATEWAY_ADDR="127.0.0.1:${GW_PORT}" \
NEXUS_CONSOLE_ADDR="127.0.0.1:${CONSOLE_PORT}" \
NEXUS_ADMIN_EMAIL="${ADMIN_EMAIL}" \
NEXUS_ADMIN_PASSWORD="${ADMIN_PASS}" \
NEXUS_PUBLIC_BASE_URL="http://127.0.0.1:${CONSOLE_PORT}" \
NEXUS_MASTER_KEY="$(printf '0%.0s' {1..64})" \
  "${BIN}" >/tmp/e2e_server.log 2>&1 &
SRV_PID=$!

for _ in $(seq 1 40); do
  [[ "$(code /healthz 2>/dev/null || echo 000)" == "200" ]] && break
  sleep 0.25
done
if [[ "$(code /healthz)" == "200" ]]; then ok "server is listening"; else
  bad "server never became healthy"; tail -30 /tmp/e2e_server.log; exit 1
fi

# Readiness must be green: the schema is current.
if [[ "$(code /readyz)" == "200" ]]; then
  ok "/readyz is 200 on a migrated database"
else
  bad "/readyz is $(code /readyz) after a successful migration"; api /readyz; echo
fi

printf '\n\033[1m3. Admin login\033[0m\n'
login_code="$(curl -sS -o /tmp/e2e_login.json -w '%{http_code}' \
  -c "${COOKIES}" -b "${COOKIES}" \
  -H 'Content-Type: application/json' \
  -H "Origin: http://127.0.0.1:${CONSOLE_PORT}" \
  -d "{\"email\":\"${ADMIN_EMAIL}\",\"password\":\"${ADMIN_PASS}\"}" \
  "http://127.0.0.1:${CONSOLE_PORT}/api/auth/login")"
if [[ "${login_code}" == "200" ]]; then ok "bootstrap admin can log in"; else
  bad "admin login returned ${login_code}"; cat /tmp/e2e_login.json; echo
fi

role="$(api /api/me | jsonfield role || true)"
if [[ "${role}" == "admin" ]]; then ok "session reports role=admin"; else
  bad "session role = '${role}', want admin"
fi

printf '\n\033[1m4. Create an invite (the call that used to 500)\033[0m\n'
inv_code="$(curl -sS -o /tmp/e2e_invite.json -w '%{http_code}' \
  -b "${COOKIES}" -c "${COOKIES}" \
  -H 'Content-Type: application/json' \
  -H "Origin: http://127.0.0.1:${CONSOLE_PORT}" \
  -d "{\"email\":\"${INVITEE}\",\"role\":\"member\"}" \
  "http://127.0.0.1:${CONSOLE_PORT}/api/invites")"
if [[ "${inv_code}" == "201" || "${inv_code}" == "200" ]]; then
  ok "POST /api/invites returned ${inv_code}"
else
  bad "POST /api/invites returned ${inv_code}"; cat /tmp/e2e_invite.json; echo
fi

INV_URL="$(jsonfield url </tmp/e2e_invite.json || true)"
TOKEN="${INV_URL##*/}"
if [[ -n "${TOKEN}" && "${TOKEN}" != "null" ]]; then
  ok "invite URL issued (token length ${#TOKEN})"
else
  bad "no invite token in the response"; cat /tmp/e2e_invite.json; echo; exit 1
fi

if api /api/invites | grep -q "${INVITEE}"; then
  ok "invite appears in GET /api/invites"
else
  bad "invite missing from the admin list"
fi

printf '\n\033[1m5. Invitee resolves and accepts the link\033[0m\n'
# The invitee has no session: lookup and accept must work anonymously.
INVITEE_JAR="$(mktemp)"
look="$(curl -sS -o /tmp/e2e_look.json -w '%{http_code}' -c "${INVITEE_JAR}" \
  "http://127.0.0.1:${CONSOLE_PORT}/api/invite/${TOKEN}")"
if [[ "${look}" == "200" ]]; then ok "anonymous GET /api/invite/{token} resolves"; else
  bad "invite lookup returned ${look}"; cat /tmp/e2e_look.json; echo
fi

acc="$(curl -sS -o /tmp/e2e_accept.json -w '%{http_code}' -c "${INVITEE_JAR}" -b "${INVITEE_JAR}" \
  -H 'Content-Type: application/json' \
  -H "Origin: http://127.0.0.1:${CONSOLE_PORT}" \
  -d "{\"password\":\"${INVITEE_PASS}\",\"name\":\"New Hire\"}" \
  "http://127.0.0.1:${CONSOLE_PORT}/api/invite/${TOKEN}/accept")"
if [[ "${acc}" == "200" || "${acc}" == "201" ]]; then ok "invite accepted (${acc})"; else
  bad "accept returned ${acc}"; cat /tmp/e2e_accept.json; echo
fi

printf '\n\033[1m6. Token is single-use\033[0m\n'
reuse="$(curl -sS -o /tmp/e2e_reuse.json -w '%{http_code}' \
  -H 'Content-Type: application/json' \
  -H "Origin: http://127.0.0.1:${CONSOLE_PORT}" \
  -d "{\"password\":\"another-pw\",\"name\":\"Impostor\"}" \
  "http://127.0.0.1:${CONSOLE_PORT}/api/invite/${TOKEN}/accept")"
if [[ "${reuse}" == "200" || "${reuse}" == "201" ]]; then
  bad "the invite token was accepted a SECOND time (${reuse}) — it must be single-use"
else
  ok "replayed token rejected with ${reuse}"
fi

printf '\n\033[1m7. Invitee can log in, and is a member (not an admin)\033[0m\n'
NEW_JAR="$(mktemp)"
li="$(curl -sS -o /tmp/e2e_newlogin.json -w '%{http_code}' -c "${NEW_JAR}" -b "${NEW_JAR}" \
  -H 'Content-Type: application/json' \
  -H "Origin: http://127.0.0.1:${CONSOLE_PORT}" \
  -d "{\"email\":\"${INVITEE}\",\"password\":\"${INVITEE_PASS}\"}" \
  "http://127.0.0.1:${CONSOLE_PORT}/api/auth/login")"
if [[ "${li}" == "200" ]]; then ok "invited user can log in"; else
  bad "invited user login returned ${li}"; cat /tmp/e2e_newlogin.json; echo
fi

nrole="$(curl -sS -b "${NEW_JAR}" "http://127.0.0.1:${CONSOLE_PORT}/api/me" | jsonfield role || true)"
if [[ "${nrole}" == "member" ]]; then ok "invited user role = member"; else
  bad "invited user role = '${nrole}', want member"
fi

# The role granted must come from the server-side invite record, so a member
# must not be able to reach an admin-only route.
admin_attempt="$(curl -sS -o /dev/null -w '%{http_code}' -b "${NEW_JAR}" \
  "http://127.0.0.1:${CONSOLE_PORT}/api/invites")"
if [[ "${admin_attempt}" == "403" || "${admin_attempt}" == "401" ]]; then
  ok "member is refused on the admin-only invite list (${admin_attempt})"
else
  bad "member reached an admin-only route with ${admin_attempt}"
fi

printf '\n\033[1m8. Audit trail\033[0m\n'
audit="$(${PSQL} "${NEXUS_POSTGRES_URL}" -tAc \
  "SELECT string_agg(DISTINCT action, ',') FROM audit_log" 2>/dev/null || echo "")"
echo "  actions recorded: ${audit:-<none>}"
for want in invite.issued invite.accepted; do
  if [[ "${audit}" == *"${want}"* ]]; then ok "audit contains ${want}"; else
    bad "audit missing ${want}"
  fi
done

printf '\n\033[1m=== invite E2E: %d passed, %d failed ===\033[0m\n' "${pass}" "${fail}"
[[ "${fail}" -eq 0 ]] || exit 1
