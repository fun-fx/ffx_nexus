#!/usr/bin/env bash
# scripts/takeover_metabase.sh
#
# Operator helper for Pattern B (existing customer Metabase). Stamps the
# "Nexus-managed" ownership marker on pre-existing Metabase objects so the
# adapter recognises them as our own and proceeds with refresh / re-seed on
# the next Nexus deploy. Without this script the adapter leaves foreign
# objects alone (this is the safe-by-default behaviour — see
# `metabase_test.go` for the unit-test guards).
#
# Usage:
#   scripts/takeover_metabase.sh datasource clickhouse   # take over nexus-clickhouse
#   scripts/takeover_metabase.sh collection "01 - Overview"   # take over Nexus - 01 - Overview
#   scripts/takeover_metabase.sh list                  # show all Nexus- reserved resources
#   scripts/takeover_metabase.sh all                   # take over every Nexus- reserved resource
#
# Required env (or pass via flags):
#   METABASE_URL        http://metabase:3000 / http://localhost:3001
#   METABASE_USER       admin@example.com
#   METABASE_PASSWORD   …the same creds the Nexus adapter uses
#
# The script is idempotent: re-running it just re-stamps the marker. Nexus
# itself remains untouched on this machine — you must restart the gateway
# (or re-deploy) for the marker to drive a refresh.

set -euo pipefail

METABASE_URL="${METABASE_URL:-http://localhost:3001}"
ADMIN_USER="${METABASE_USER:-}"
ADMIN_PASSWORD="${METABASE_PASSWORD:-}"
MARKER_DB='nexus_managed_by'
MARKER_DB_VALUE='metabase-bootstrapper/v1'
MARKER_COLL_PREFIX='[Nexus-managed] '

step() { printf "\n\033[1;34m▸ %s\033[0m\n" "$*"; }
ok()   { printf "  \033[1;32m✓ %s\033[0m\n" "$*"; }
fail() { printf "  \033[1;31m✗ %s\033[0m\n" "$*" >&2; exit 1; }

[ -n "$ADMIN_USER" ]      || fail "set METABASE_USER env var"
[ -n "$ADMIN_PASSWORD" ]  || fail "set METABASE_PASSWORD env var"

# Health check: metabase takes 30–90s for first boot.
if ! curl -fsS -o /dev/null "$METABASE_URL/api/health"; then
    fail "metabase is not reachable at $METABASE_URL/api/health"
fi

# Session token (X-Metabase-Session). Metabase accepts username/password on
# /api/session instead of API key auth so the same script works against a
# freshly initialized container (admin secret not yet provisioned).
step "Obtaining Metabase session"
SESSION=$(curl -fsS -X POST -H 'Content-Type: application/json' \
    --data "{\"username\":\"$ADMIN_USER\",\"password\":\"$ADMIN_PASSWORD\"}" \
    "$METABASE_URL/api/session" \
    | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
[ -n "$SESSION" ] || fail "could not log in (check METABASE_USER / METABASE_PASSWORD)"
ok "session acquired"

# JSON helpers using the session (so we don't have to depend on jq in CI).
api_get()    { curl -fsS -H "X-Metabase-Session: $SESSION" "$METABASE_URL$1"; }
api_post()   { curl -fsS -X POST -H 'Content-Type: application/json' -H "X-Metabase-Session: $SESSION" \
              --data "$2" "$METABASE_URL$1"; }
api_put_id() {
    local id="$1" body="$2"
    curl -fsS -X PUT -H 'Content-Type: application/json' -H "X-Metabase-Session: $SESSION" \
        --data "$body" "$METABASE_URL/api/database/$id" > /dev/null
}

list_datasources() {
    api_get /api/database | grep -oE '"id":[0-9]+,"name":"nexus-[^"]+","engine":"[^"]+"' \
        | sed -E 's/"id":([0-9]+).*"name":"(nexus-[^"]+)".*"engine":"([^"]+)"/\1\t\2\t\3/'
}
list_collections() {
    api_get /api/collection | grep -oE '"id":[0-9]+,"name":"Nexus - [^"]+"' \
        | sed -E 's/"id":([0-9]+).*"name":"(Nexus - [^"]+)"/\1\t\2/'
}

cmd_list() {
    step "Datasources with the reserved nexus- name"
    if rows=$(list_datasources); [ -n "$rows" ]; then
        printf "  %-6s  %-32s  %s\n" "ID" "NAME" "ENGINE"
        printf "  %-6s  %-32s  %s\n" "----" "----" "------"
        while IFS=$'\t' read -r id name engine; do
            printf "  %-6s  %-32s  %s\n" "$id" "$name" "$engine"
        done <<< "$rows"
    else
        echo "  (none)"
    fi
    step "Collections with the reserved 'Nexus -' prefix"
    if rows=$(list_collections); [ -n "$rows" ]; then
        printf "  %-6s  %s\n" "ID" "NAME"
        printf "  %-6s  %s\n" "----" "----"
        while IFS=$'\t' read -r id name; do
            printf "  %-6s  %s\n" "$id" "$name"
        done <<< "$rows"
    else
        echo "  (none)"
    fi
}

stamp_database() {
    local id="$1" name="$2" details
    details=$(api_get "/api/database/$id")
    # Merge the marker into the existing details object. We use python3 here
    # because jq is not always present in CI images. The output has the new
    # details JSON written back via PUT /api/database/:id.
    new_details=$(MARKER="$MARKER_DB" VALUE="$MARKER_DB_VALUE" python3 - <<PY
import json, os, sys
data = json.loads('''$details''')
det = data.get("details") or {}
det[os.environ["MARKER"]] = os.environ["VALUE"]
data["details"] = det
print(json.dumps(data))
PY
)
    # Metabase wants just {engine, details} on PUT; the rest is server-managed.
    api_get "/api/database/$id" >/dev/null  # warm up
    payload=$(python3 - <<PY
import json, sys
data = json.loads('''$new_details''')
out = {"engine": data["engine"], "details": data["details"]}
print(json.dumps(out))
PY
)
    api_put_id "$id" "$payload"
    ok "datasource id=$id name=$name stamped with $MARKER_DB=$MARKER_DB_VALUE"
}

stamp_collection() {
    local id="$1" name="$2"
    # /api/collection/:id is GET/PATCH. Patch with the new description that
    # carries the marker prefix.
    cur=$(api_get "/api/collection/$id")
    payload=$(python3 - <<PY
import json, sys
data = json.loads('''$cur''')
desc = data.get("description") or ""
prefix = "$MARKER_COLL_PREFIX"
if not desc.startswith(prefix):
    desc = prefix + desc
data["description"] = desc
print(json.dumps(data))
PY
)
    curl -fsS -X PUT -H 'Content-Type: application/json' -H "X-Metabase-Session: $SESSION" \
        --data "$payload" "$METABASE_URL/api/collection/$id" > /dev/null
    ok "collection id=$id name=$name stamped with description prefix"
}

# ---------------------------------------------------------------------------

subcmd="${1:-list}"
shift || true

case "$subcmd" in
    list)
        cmd_list
        ;;
    datasource)
        engine="${1:-}"
        [ -n "$engine" ] || fail "usage: $0 datasource <engine>"
        step "Datasource takeover (engine=$engine)"
        list_datasources | awk -F'\t' -v e="$engine" '$3 == e { print $1, $2 }' | while read -r id name; do
            stamp_database "$id" "$name"
        done
        ;;
    collection)
        short="${1:-}"
        [ -n "$short" ] || fail "usage: $0 collection <short-name> (e.g. '01 - Overview')"
        needle="Nexus - $short"
        step "Collection takeover (name=$needle)"
        list_collections | awk -F'\t' -v n="$needle" '$2 == n { print $1 }' | while read -r id; do
            stamp_collection "$id" "$needle"
        done
        ;;
    all)
        step "Taking over every Nexus- reserved resource"
        list_datasources | while IFS=$'\t' read -r id name engine; do
            stamp_database "$id" "$name"
        done
        list_collections | while IFS=$'\t' read -r id name; do
            stamp_collection "$id" "$name"
        done
        printf "\n\033[1;33mNext step:\033[0m restart Nexus so the bootstrap re-registers:\n"
        printf "  docker compose -f deploy/docker-compose.yml --profile bi restart nexus\n"
        printf "  docker logs -f deploy-nexus-1 | grep -i metabase\n"
        ;;
    *)
        cat <<EOF
Usage:
  $0 list
  $0 datasource <clickhouse|postgres>
  $0 collection <short-name>
  $0 all
EOF
        exit 2
        ;;
esac
