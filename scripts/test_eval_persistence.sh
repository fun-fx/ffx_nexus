#!/usr/bin/env bash
# Detailed integration: live completion -> async worker -> fake eval -> ClickHouse.
# Requires datastore stack + a working upstream provider (GEMINI_API_KEY or DB credential).
set -euo pipefail

# Use alternate ports so this script can run alongside other E2E scripts.
export NEXUS_GATEWAY_ADDR=":8092"
export NEXUS_CONSOLE_ADDR=":8093"

source "$(dirname "$0")/lib/e2e_common.sh"
e2e_init

FAKE_PORT=8201
FAKE_URL="http://127.0.0.1:${FAKE_PORT}"
FAKE_PID=""

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

cleanup() {
  stop_nexus
  stop_fake
}
trap cleanup EXIT

echo "== Detailed: remote eval -> ClickHouse persistence =="

go build -o "$BIN" ./cmd/nexus
pass "build ok"

load_dotenv

MODEL="${TEST_MODEL:-gemini-2.5-flash}"
if [[ -z "${GEMINI_API_KEY:-}" && -z "${OPENAI_API_KEY:-}" ]]; then
  skip "live persistence (no provider API key; Go ClickHouse integration test covers this path)"
  summary_exit
  exit 0
fi

wait_services
export_e2e_env

start_fake
pass "fake eval-service started"

start_nexus env \
  NEXUS_EVAL_SERVICE_URL="$FAKE_URL" \
  NEXUS_EVAL_SERVICE_METRICS="answer_relevancy,toxicity,bias" \
  NEXUS_EVAL_SAMPLE_RATE=1.0 \
  NEXUS_EVAL_WORKERS=4
pass "nexus started with eval-service (sample_rate=1.0)"

KEY_JSON=$(curl -sf -X POST "$CON_URL/api/keys" \
  -H 'Content-Type: application/json' \
  -d '{"name":"eval-persist","allowed_models":["'"$MODEL"'","auto"]}')
SECRET=$(echo "$KEY_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['secret'])")

BEFORE=$(curl -sf "http://localhost:8123/?user=nexus&password=nexus" \
  --data-binary "SELECT count() FROM nexus.eval_scores WHERE evaluator = 'deepeval'" 2>/dev/null || echo 0)

CODE=$(curl -s -o /tmp/eval_persist.json -w "%{http_code}" -X POST "$GW_URL/v1/chat/completions" \
  -H "Authorization: Bearer $SECRET" \
  -H 'Content-Type: application/json' \
  -d '{"model":"'"$MODEL"'","messages":[{"role":"user","content":"Reply with exactly: ok"}],"max_tokens":64}')

if [[ "$CODE" != "200" ]]; then
  if grep -qE 'RESOURCE_EXHAUSTED|quota exceeded|429|502' /tmp/eval_persist.json 2>/dev/null; then
    skip "upstream completion (quota exhausted) — worker+CH integration test covers this path"
    exit 0
  fi
  if grep -qE 'model_not_found|no provider registered' /tmp/eval_persist.json 2>/dev/null; then
    skip "upstream completion (no provider registered) — worker+CH integration test covers this path"
    exit 0
  fi
  fail "completion expected 200, got $CODE: $(head -c 300 /tmp/eval_persist.json)"
  summary_exit
fi
pass "upstream completion -> 200"

# Gemini 2.5+ may return empty content when max_tokens is too small (thinking
# tokens consume the budget). RemoteEvaluator skips traces with empty output, so
# confirm we got text before waiting for deepeval scores.
OUT_LEN=$(python3 -c "import json; d=json.load(open('/tmp/eval_persist.json')); print(len(d.get('choices',[{}])[0].get('message',{}).get('content','')))" 2>/dev/null || echo 0)
if [[ "${OUT_LEN:-0}" -lt 1 ]]; then
  fail "completion returned empty output (max_tokens too low for $MODEL?); remote eval will skip"
  summary_exit
fi
pass "completion returned non-empty output (len=$OUT_LEN)"

FOUND=0
echo "  waiting for deepeval scores in ClickHouse..."
for i in $(seq 1 30); do
  COUNT=$(curl -sf "http://localhost:8123/?user=nexus&password=nexus" \
    --data-binary "SELECT count() FROM nexus.eval_scores WHERE evaluator = 'deepeval'" 2>/dev/null || echo 0)
  NEW=$((COUNT - BEFORE))
  if [[ "${NEW:-0}" -ge 1 ]]; then
    FOUND=1
    break
  fi
  sleep 1
done

if [[ "$FOUND" == "1" ]]; then
  ROWS=$(curl -sf "http://localhost:8123/?user=nexus&password=nexus" \
    --data-binary "SELECT metric, score, passed FROM nexus.eval_scores WHERE evaluator = 'deepeval' ORDER BY timestamp DESC LIMIT 5 FORMAT TabSeparated" 2>/dev/null || true)
  pass "remote eval scores persisted to ClickHouse (new=$NEW)"
  echo "  latest deepeval rows:"
  echo "$ROWS" | sed 's/^/    /'
else
  fail "no deepeval scores in ClickHouse after 30s (before=$BEFORE count=${COUNT:-0})"
fi

# Heuristics should also run on the same trace.
HEUR=$(curl -sf "http://localhost:8123/?user=nexus&password=nexus" \
  --data-binary "SELECT count() FROM nexus.eval_scores WHERE evaluator LIKE 'heuristic_%' AND timestamp > now() - INTERVAL 2 MINUTE" 2>/dev/null || echo 0)
if [[ "${HEUR:-0}" -ge 1 ]]; then
  pass "heuristic scores also written for the same window (count=$HEUR)"
else
  fail "expected heuristic scores alongside remote eval"
fi

summary_exit
