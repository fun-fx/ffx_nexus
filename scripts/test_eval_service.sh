#!/usr/bin/env bash
# E2E tests for the external Python eval-service integration.
#
# Verifies, without DeepEval/RAGAS or an LLM (a lightweight fake service stands
# in for the contract):
#   1. The Go <-> eval-service HTTP contract the RemoteEvaluator expects.
#   2. Nexus wires NEXUS_EVAL_SERVICE_URL into the eval worker at startup.
#   3. Failure isolation: a dead eval-service never blocks startup or serving.
set -euo pipefail

source "$(dirname "$0")/lib/e2e_common.sh"
e2e_init

FAKE_PORT=8200
FAKE_URL="http://127.0.0.1:${FAKE_PORT}"
FAKE_PID=""
NEXUS_LOG="/tmp/nexus_eval_service.log"

start_fake() {
  python3 "$ROOT/scripts/lib/fake_eval_service.py" "$FAKE_PORT" &
  FAKE_PID=$!
  for i in $(seq 1 20); do
    curl -sf "$FAKE_URL/healthz" >/dev/null 2>&1 && return 0
    sleep 0.25
  done
  echo "fake eval-service failed to start"
  return 1
}

stop_fake() {
  if [[ -n "$FAKE_PID" ]]; then
    kill "$FAKE_PID" 2>/dev/null || true
    wait "$FAKE_PID" 2>/dev/null || true
  fi
  FAKE_PID=""
}

# Start nexus with full datastore env plus extra KEY=VAL overrides, capturing
# logs so we can assert the eval-service wiring.
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

cleanup() {
  stop_nexus
  stop_fake
}
trap cleanup EXIT

echo "== External eval-service integration =="

go build -o "$BIN" ./cmd/nexus
pass "build ok"

load_dotenv
wait_services
export_e2e_env

# 1. Contract: fake service speaks the schema the RemoteEvaluator decodes.
start_fake
pass "fake eval-service started on $FAKE_URL"

RESP=$(curl -sf -X POST "$FAKE_URL/evaluate" -H 'Content-Type: application/json' \
  -d '{"trace_id":"t-1","input":"2+2?","output":"4","metrics":["answer_relevancy","toxicity"]}')
N=$(python3 -c "import json,sys; d=json.loads(sys.argv[1]); print(len(d.get('scores',[])))" "$RESP" 2>/dev/null || echo 0)
M0=$(python3 -c "import json,sys; d=json.loads(sys.argv[1]); print(d['scores'][0]['metric'])" "$RESP" 2>/dev/null || echo "")
if [[ "$N" == "2" && "$M0" == "answer_relevancy" ]]; then
  pass "contract: /evaluate returns scores[] with metric/score fields"
else
  fail "contract: unexpected /evaluate response: $RESP"
fi

# 2. Wiring: Nexus enables the external evaluator when the URL is set.
start_nexus_logged NEXUS_EVAL_SERVICE_URL="$FAKE_URL" NEXUS_EVAL_SERVICE_METRICS="answer_relevancy,toxicity"
if grep -q "external eval service enabled" "$NEXUS_LOG"; then
  pass "nexus wired the external eval service at startup"
else
  fail "expected 'external eval service enabled' in nexus logs"
fi
if [[ "$(http_code "$GW_URL/healthz")" == "200" ]]; then
  pass "nexus healthy with eval-service configured"
else
  fail "nexus not healthy with eval-service configured"
fi

# 3. Isolation: a dead eval-service must not block startup or serving.
stop_fake
start_nexus_logged NEXUS_EVAL_SERVICE_URL="http://127.0.0.1:59999"
if [[ "$(http_code "$GW_URL/healthz")" == "200" ]]; then
  pass "nexus healthy even when eval-service is unreachable"
else
  fail "dead eval-service blocked nexus startup/serving"
fi
if grep -q "external eval service enabled" "$NEXUS_LOG"; then
  pass "eval worker constructed despite unreachable eval-service (graceful)"
else
  fail "eval worker not constructed with eval-service URL set"
fi

summary_exit
