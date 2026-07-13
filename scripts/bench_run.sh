#!/usr/bin/env bash
# Real benchmark: how many concurrent LLM-proxy requests can one
# Nexus instance sustain before latency or error rate becomes
# user-visible? All numbers here are measured, not estimated.
#
# Pipeline per round:
#   1. Start mock-upstream on :18080 (configurable latency)
#   2. Start Nexus in zero-dep mode on gateway :8090, console :8091
#   3. Enable NEXUS_METRICS_ADDR=:9101 so Prometheus can scrape
#   4. Fire `loadgen -c $C` for $DURATION seconds against /v1/chat/completions
#   5. Sample Nexus /metrics every 1s; capture peak in-flight + goroutines
#   6. Print the summary table
#
# We *do not* try to spin up a sidecar Postgres / Redis / ClickHouse:
# this benchmark is "what does the proxy itself handle when the
# upstream is fast?". Production realism comes from elsewhere.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

PORT_MOCK="${PORT_MOCK:-18080}"
PORT_GW="${PORT_GW:-8090}"
PORT_METRICS="${PORT_METRICS:-9101}"
LATENCY_MS="${LATENCY_MS:-50}"
STREAM="${STREAM:-0}"
NEXUS_BIN="$ROOT/bin/bench-nexus"
MOCK_BIN="$ROOT/bin/bench-mock"
LOADGEN_BIN="$ROOT/bin/bench-loadgen"

# compile all helpers once
echo "[bench] building: nexus / mock-upstream / loadgen"
mkdir -p "$ROOT/bin"
(cd "$ROOT" && go build -o "$NEXUS_BIN"   ./cmd/nexus)
(cd "$ROOT" && go build -o "$MOCK_BIN"    ./cmd/mock-upstream)
(cd "$ROOT" && go build -o "$LOADGEN_BIN" ./cmd/loadgen)

cleanup() {
  set +e
  if [[ -n "${NEXUS_PID:-}" ]] && kill -0 "$NEXUS_PID" 2>/dev/null; then
    kill "$NEXUS_PID" 2>/dev/null || true
  fi
  if [[ -n "${MOCK_PID:-}" ]] && kill -0 "$MOCK_PID" 2>/dev/null; then
    kill "$MOCK_PID" 2>/dev/null || true
  fi
  rm -f "${METRICS_LOG:-}" 2>/dev/null || true
}
trap cleanup EXIT

# 1) mock upstream
echo "[bench] starting mock-upstream on :$PORT_MOCK (latency=${LATENCY_MS}ms stream=$STREAM)"
"$MOCK_BIN" -addr=":$PORT_MOCK" -latency="$LATENCY_MS" -stream="$STREAM" >/tmp/mock.log 2>&1 &
MOCK_PID=$!
sleep 0.5

# 2) nexus
echo "[bench] starting nexus zero-dep on :$PORT_GW (metrics :$PORT_METRICS)"
# Force deterministic zero-dep path: no Postgres / Redis / ClickHouse
# env vars set; OPENAI_BASE_URL points at mock so chat completions
# go through our goroutine chain.
NEXUS_GATEWAY_ADDR=":$PORT_GW" \
NEXUS_METRICS_ADDR=":$PORT_METRICS" \
OPENAI_API_KEY="sk-bench-mock" \
OPENAI_BASE_URL="http://127.0.0.1:$PORT_MOCK" \
ALLOW_SIGNUP="0" \
"$NEXUS_BIN" >/tmp/nexus.log 2>&1 &
NEXUS_PID=$!

# wait for /healthz
for _ in $(seq 1 50); do
  if curl -sf "http://127.0.0.1:$PORT_GW/healthz" >/dev/null; then
    break
  fi
  sleep 0.1
done

echo "[bench] nexus up; mock up"

# 3) scrape metrics in background
METRICS_LOG="$(mktemp -t bench-metrics.XXXXXX)"
( while true; do
    ts=$(date +%s)
    inflight=$(curl -sf "http://127.0.0.1:$PORT_METRICS/metrics" 2>/dev/null | grep -E '^nexus_gateway_requests_in_flight' | awk '{print $2}' | head -1)
    goroutines=$(curl -sf "http://127.0.0.1:$PORT_METRICS/metrics" 2>/dev/null | grep -E '^nexus_go_goroutines' | awk '{print $2}' | head -1)
    pause=$(curl -sf "http://127.0.0.1:$PORT_METRICS/metrics" 2>/dev/null | grep -E '^nexus_go_gc_pause_ns_total' | awk '{print $2}' | head -1)
    echo "$ts inflight=${inflight:-0} goroutines=${goroutines:-0} gc_pause_ns=${pause:-0}" >> "$METRICS_LOG"
    sleep 1
  done ) &
SCRAPER_PID=$!

run_round() {
  local label="$1"
  local concurrency="$2"
  local duration="$3"
  echo ""
  echo "=========================================================="
  echo "[bench] $label: c=$concurrency d=$duration stream=$STREAM"
  echo "=========================================================="
  truncate -s0 "$METRICS_LOG"
  "$LOADGEN_BIN" -url="http://127.0.0.1:$PORT_GW/v1/chat/completions" -c "$concurrency" -d "$duration" -stream="$STREAM" 2>&1 | tee -a /tmp/round.log

  echo "[bench] $label: peak in-flight + goroutines during run"
  awk '
    /inflight/ {
      m = ($2 ~ /inflight=/ ? substr($2, index($2, "=")+1) : 0)
      if (m+0 > max_inflight) max_inflight = m+0
    }
    /goroutines/ {
      g = ($3 ~ /goroutines=/ ? substr($3, index($3, "=")+1) : 0)
      if (g+0 > max_g) max_g = g+0
    }
    END {
      printf "  peak in-flight=%d peak goroutines=%d\n", max_inflight, max_g
    }
  ' "$METRICS_LOG"
}

run_round "round-1-baseline" 100  20s
run_round "round-2-mid-load" 1000 20s
run_round "round-3-burst"    3000 15s

if [[ "$STREAM" == "1" ]]; then
  run_round "round-4-streams" 500 20s
fi

kill "$SCRAPER_PID" 2>/dev/null || true

echo ""
echo "[bench] done"
echo "[bench] nexus log tail:" 
tail -10 /tmp/nexus.log || true
