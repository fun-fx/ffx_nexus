#!/usr/bin/env bash
# V5 single-pod ceiling measurement.
#
# Goal: quantify "one pod" capacity so HPA metric thresholds
# (CPU 70%, in-flight req X, ...) can be calibrated against actual
# delivered throughput, not vibes. Plan V5 calls this out as a
# prerequisite for HPA activation.
#
# Tooling:
#   - wrk 4.x against the dev profile's nexus gateway (:8080)
#   - mock-upstream with bounded worker pool (workers=8) as the LLM
#     backend so we measure *gateway* latency, not provider variability
#
# Outputs (printed, not saved):
#   - per-phase p50/p75/p90/p99 (ms) + req/s + transfer
#
# Up: docker compose -f deploy/docker-compose.yml --profile dev up -d
# Run: ./scripts/test_v5_ceiling.sh

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

GATEWAY="${GATEWAY:-http://localhost:8080}"
CONSOLE="${CONSOLE:-http://localhost:8081}"
DURATION="${DURATION:-15}"
WRK_WORKERS="${WRK_WORKERS:-4}"

for bin in wrk docker curl awk sed; do
  command -v "$bin" >/dev/null 2>&1 || { echo "missing dep: $bin" >&2; exit 2; }
done

echo "== V5 single-pod ceiling measurement =="
echo "  gateway       : $GATEWAY"
echo "  duration/phase: ${DURATION}s   (phase-4 is 4× longer)"
echo "  wrk workers   : $WRK_WORKERS"
echo

if ! curl -fsS "$GATEWAY/healthz" >/dev/null 2>&1; then
  cat <<EOF >&2
gateway is not reachable at $GATEWAY. Bring the dev stack up first:
  docker compose -f deploy/docker-compose.yml --profile dev up -d
EOF
  exit 2
fi

# Mint a virtual key the wrk client can present.
echo "[mint vkey via console signup]"
EMAIL="ceil_$(date +%s)@nexus.local"
curl -fsS -o /tmp/ceil_signup.json -X POST -H 'Content-Type: application/json' \
  --data "{\"email\":\"$EMAIL\",\"password\":\"ceil_test_pw_1\",\"provider\":\"openai\",\"provider_secret\":\"sk-ceil-fake\"}" \
  "$CONSOLE/api/auth/register"
VKEY=$(sed -n 's/.*"virtual_key":"\([^"]*\)".*/\1/p' /tmp/ceil_signup.json || true)
if [ -z "$VKEY" ]; then
  VKEY=$(sed -n 's/.*"vkey":"\([^"]*\)".*/\1/p' /tmp/ceil_signup.json || true)
fi
[ -n "$VKEY" ] || { echo "could not parse vkey:"; cat /tmp/ceil_signup.json; exit 3; }
export VKEY
echo "  vkey (truncated): ${VKEY:0:24}..."

# Warm-up.
echo
echo "[warmup 5s @ 50 concurrent]"
wrk -t"$WRK_WORKERS" -c50 -d5 --latency "$GATEWAY/v1/chat/completions" \
  -s "$ROOT/scripts/wrk_ceiling.lua" \
  | grep -E 'Latency|Req/sec' | sed 's/^/    /'

# 4 phases. Each phase writes its raw wrk output to /tmp/v5_p{n}.txt.
PHASES=( "200,${DURATION}" "500,${DURATION}" "1000,${DURATION}" "1000,$((DURATION * 4))" )
for i in 0 1 2 3; do
  IFS=',' read -r CONN DUR <<< "${PHASES[$i]}"
  echo
  echo "[phase-$((i+1))] $CONN concurrent, ${DUR}s"
  wrk -t"$WRK_WORKERS" -c"$CONN" -d"$DUR" --latency \
    "$GATEWAY/v1/chat/completions" \
    -s "$ROOT/scripts/wrk_ceiling.lua" >"/tmp/v5_p$((i+1)).txt" 2>&1 || true
  grep -E 'Latency|Req/sec|Transfer' "/tmp/v5_p$((i+1)).txt" | sed 's/^/    /'
done

# Summary: latency distribution + req/sec per phase.
echo
echo "== V5 ceiling summary =="
printf '%-18s %-8s %-8s %-8s %-8s %-10s\n' phase p50 p75 p90 p99 req/s
for n in 1 2 3 4; do
  f="/tmp/v5_p${n}.txt"
  [ -s "$f" ] || continue
  awk -v ph="phase-$n" '
    /50%/  {p50=$2}
    /75%/  {p75=$2}
    /90%/  {p90=$2}
    /99%/  {p99=$2}
    /Requests\/sec:/ {rps=$2}
    END {printf "%-18s %-8s %-8s %-8s %-8s %-10s\n", ph, p50, p75, p90, p99, rps}
  ' "$f"
done
echo
echo "raw output preserved at /tmp/v5_p{1..4}.txt"
echo "next: feed p99 + req/s into HPA metric threshold. Until then, HPA is hold."
