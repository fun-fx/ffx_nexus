#!/usr/bin/env bash
# E2E tests for structured-output self-correction (hot path, non-streaming).
#
# The correction loop depends on an upstream that returns bad-then-good JSON,
# which is not deterministic with real providers, so this script verifies the
# startup wiring (the loop's block/allow/retry behavior is covered by the Go
# handler tests in internal/gateway/self_correction_test.go).
set -euo pipefail

source "$(dirname "$0")/lib/e2e_common.sh"
e2e_init

NEXUS_LOG="/tmp/nexus_self_correction.log"

start_nexus_logged() {
  stop_nexus
  : >"$NEXUS_LOG"
  env "$@" "$BIN" >"$NEXUS_LOG" 2>&1 &
  NEXUS_PID=$!
  for i in $(seq 1 40); do
    curl -sf "$GW_URL/healthz" >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  echo "nexus failed to start; logs:"
  cat "$NEXUS_LOG"
  return 1
}

trap stop_nexus EXIT

echo "== Structured-output self-correction =="

go build -o "$BIN" ./cmd/nexus
pass "build ok"

load_dotenv
export NEXUS_GATEWAY_ADDR="$GW"
export NEXUS_CONSOLE_ADDR="$CON"

# Self-correction requires the schema guardrail to supply the rejection signal.
start_nexus_logged \
  -u NEXUS_POSTGRES_URL -u NEXUS_CLICKHOUSE_URL -u NEXUS_REDIS_URL -u NEXUS_MASTER_KEY \
  NEXUS_GUARDRAILS_ENABLED=true \
  NEXUS_GUARDRAILS_VALIDATE_JSON_OUTPUT=true \
  NEXUS_SELF_CORRECTION_ENABLED=true \
  NEXUS_SELF_CORRECTION_MAX_RETRIES=2

if grep -q "self-correction enabled" "$NEXUS_LOG" && grep -q "max_retries=2" "$NEXUS_LOG"; then
  pass "self-correction wired at startup (max_retries=2)"
else
  fail "expected self-correction startup log with max_retries=2"
fi

# Disabled by default: no startup log line.
start_nexus_logged \
  -u NEXUS_POSTGRES_URL -u NEXUS_CLICKHOUSE_URL -u NEXUS_REDIS_URL -u NEXUS_MASTER_KEY
if grep -q "self-correction enabled" "$NEXUS_LOG"; then
  fail "self-correction should be disabled by default"
else
  pass "self-correction disabled by default"
fi

summary_exit
