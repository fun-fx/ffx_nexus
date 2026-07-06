#!/usr/bin/env bash
# Local smoke test for the v0.1.0 release image at
# ghcr.io/fun-fx/ffx_nexus:v0.1.0 (override with $IMAGE).
#
# Pulls the publicly published image, runs it with the strict-byok
# default and a minimal Postgres+ClickHouse+Redis stack from
# deploy/docker-compose.yml, and verifies:
#
#   1. The container comes up; /healthz returns 200.
#   2. /api/auth/register accepts a new user (NEXUS_ALLOW_SIGNUP=true).
#   3. The signed-up user's first vkey call to /v1/chat/completions for
#      gemini is rejected with 403 missing_byok_key — confirming the
#      v0.1.0 strict-byok default.
#   4. After registering a (fake) per-user provider key via
#      /api/me/credentials, the same gateway call goes through to the
#      upstream (we expect 503 / 502 because we never registered a real
#      key, but never 403 missing_byok_key).
#
# This script is hermetic — it does NOT need a real provider API key —
# so it can run on CI / on a developer laptop without leaking secrets.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE="${IMAGE:-ghcr.io/fun-fx/ffx_nexus:v0.1.0}"
EMAIL="smoke-$(date +%s)@nexus.local"
PASS="smoke-pass-pass"

cd "$ROOT"

echo "== v0.1.0 image smoke =="
echo "  image: $IMAGE"

command -v docker >/dev/null || { echo "docker required"; exit 1; }
command -v curl >/dev/null || { echo "curl required"; exit 1; }

echo "  starting postgres + clickhouse + redis (compose services only)..."
docker compose -f deploy/docker-compose.yml up -d postgres clickhouse redis >/dev/null

# Health polls (independent of nexus itself).
for i in {1..30}; do
  if docker compose -f deploy/docker-compose.yml exec -T postgres pg_isready -U nexus >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
for i in {1..30}; do
  if curl -sf http://localhost:8123/ping >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

echo "  pulling + starting nexus container..."
docker pull "$IMAGE" >/dev/null
docker rm -f nexus-smoke 2>/dev/null || true

# Convert compose port mappings to host ports the container can reach via
# host.docker.internal.
NEXUS_CONTAINER=$(docker run -d --name nexus-smoke \
  -p 8080:8080 -p 8081:8081 \
  -e NEXUS_POSTGRES_URL="postgres://nexus:nexus@host.docker.internal:5433/nexus?sslmode=disable" \
  -e NEXUS_CLICKHOUSE_URL="clickhouse://nexus:nexus@host.docker.internal:9000/nexus" \
  -e NEXUS_REDIS_URL="redis://host.docker.internal:6379/0" \
  -e NEXUS_MASTER_KEY="$(openssl rand -base64 32)" \
  -e NEXUS_ALLOW_SIGNUP=true \
  "$IMAGE")

cleanup() {
  docker rm -f "$NEXUS_CONTAINER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "  waiting for /healthz..."
for i in {1..40}; do
  if curl -sf http://localhost:8080/healthz >/dev/null; then
    echo "  ✓ nexus container is healthy"
    break
  fi
  sleep 0.5
  if [[ "$i" == "40" ]]; then
    echo "  ✗ nexus container did not become healthy in time"
    docker logs "$NEXUS_CONTAINER" | tail -40 || true
    exit 1
  fi
done

# 2) Register
echo "  signing up $EMAIL..."
REG=$(curl -s -X POST http://localhost:8081/api/auth/register \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS\"}")
VKEY=$(echo "$REG" | python3 -c "import sys,json; print(json.load(sys.stdin).get('virtual_key',''))")
if [[ -z "$VKEY" ]]; then
  echo "  ✗ register did not return vkey: $REG"
  exit 1
fi
echo "  ✓ virtual key issued"

# 3) Call without a stored credential — expect 403 missing_byok_key.
BODY='{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}'
RESP=$(curl -s -o /tmp/nexus_smoke.txt -w '%{http_code}' \
  -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $VKEY" \
  -H 'Content-Type: application/json' \
  -d "$BODY")
if [[ "$RESP" == "403" ]] && grep -q "missing_byok_key" /tmp/nexus_smoke.txt; then
  echo "  ✓ strict-byok default enforced (caller without stored key → 403 missing_byok_key)"
else
  echo "  ✗ expected 403 missing_byok_key, got HTTP $RESP"
  head -c 200 /tmp/nexus_smoke.txt
  echo
  exit 1
fi

# 4) Login, then register a fake credential, then call again.
JAR=/tmp/nexus_smoke_jar.txt
curl -s -c "$JAR" -X POST http://localhost:8081/api/auth/login \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS\"}" >/dev/null
curl -s -b "$JAR" -X POST http://localhost:8081/api/me/credentials \
  -H 'Content-Type: application/json' \
  -d '{"provider":"gemini","name":"smoke","secret":"sk-gemini-fake-smoke"}' >/dev/null
echo "  ✓ registered a fake gemini credential for $EMAIL"

RESP=$(curl -s -o /tmp/nexus_smoke.txt -w '%{http_code}' \
  -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $VKEY" \
  -H 'Content-Type: application/json' \
  -d "$BODY")
case "$RESP" in
  502|503)
    echo "  ✓ after registering a key, request left the gateway (HTTP $RESP — upstream-side, expected for fake credential)"
    ;;
  200)
    echo "  ✓ request succeeded (HTTP 200)"
    ;;
  *)
    if grep -q "missing_byok_key" /tmp/nexus_smoke.txt; then
      echo "  ✗ still missing_byok_key despite a registered credential"
      head -c 200 /tmp/nexus_smoke.txt
      echo
      exit 1
    fi
    echo "  ✓ request left the gateway (HTTP $RESP — body inspected below)"
    head -c 200 /tmp/nexus_smoke.txt
    echo
    ;;
esac

echo ""
echo "== v0.1.0 smoke complete =="
echo "  pass: image boots, strict-byok enforced, BYOK path resolves"
