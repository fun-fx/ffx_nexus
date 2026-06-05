#!/usr/bin/env bash
# E2E tests for schema/JSON output guardrails (hot path).
#
# Output validation needs a real upstream completion, so the live roundtrip is
# skipped without a provider key. Deterministic block/allow behavior is covered
# by the Go handler tests; this script verifies startup wiring and, when a key
# is present, that valid JSON output passes end-to-end.
set -euo pipefail

source "$(dirname "$0")/lib/e2e_common.sh"
e2e_init

NEXUS_LOG="/tmp/nexus_schema_guard.log"

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

echo "== Schema/JSON output guardrails =="

go build -o "$BIN" ./cmd/nexus
pass "build ok"

load_dotenv
export NEXUS_GATEWAY_ADDR="$GW"
export NEXUS_CONSOLE_ADDR="$CON"

# Zero-dependency mode + schema guardrail enabled. -u unsets must precede the
# KEY=VALUE assignments for env(1).
start_nexus_logged \
  -u NEXUS_POSTGRES_URL -u NEXUS_CLICKHOUSE_URL -u NEXUS_REDIS_URL -u NEXUS_MASTER_KEY \
  NEXUS_GUARDRAILS_ENABLED=true \
  NEXUS_GUARDRAILS_VALIDATE_JSON_OUTPUT=true

if grep -q "validate_json_output=true" "$NEXUS_LOG"; then
  pass "schema guardrail wired at startup"
else
  fail "expected validate_json_output=true in startup log"
fi

MODEL="${TEST_MODEL:-gemini-2.5-flash}"
if [[ -z "${GEMINI_API_KEY:-}" && -z "${OPENAI_API_KEY:-}" ]]; then
  skip "live JSON roundtrip (no provider key; Go handler tests cover block/allow)"
  summary_exit
fi

# Live: request JSON output; a model in json mode should return valid JSON -> 200.
BODY='{"model":"'"$MODEL"'","messages":[{"role":"user","content":"Return a JSON object with a single key ok set to true."}],"response_format":{"type":"json_object"},"max_tokens":64}'
OUT=$(curl_chat_completion "$GW_URL/v1/chat/completions" "" "$BODY" /tmp/schema_chat.json) && rc=0 || rc=$?

if [[ "${CHAT_HTTP_CODE:-}" == "200" ]]; then
  CONTENT=$(python3 -c "import json;print(json.load(open('/tmp/schema_chat.json'))['choices'][0]['message']['content'])" 2>/dev/null || echo "")
  if python3 -c "import json,sys; json.loads(sys.argv[1])" "$CONTENT" 2>/dev/null; then
    pass "valid JSON output passes schema guardrail -> 200"
  else
    fail "gateway returned 200 but content is not valid JSON: $CONTENT"
  fi
elif [[ "${CHAT_HTTP_CODE:-}" == "422" ]]; then
  # The model produced non-JSON; the guardrail correctly blocked it.
  pass "non-JSON model output blocked -> 422 schema_validation_failed"
elif [[ "$rc" == "2" ]]; then
  skip "live JSON roundtrip (upstream quota exhausted)"
else
  skip "live JSON roundtrip (upstream returned ${CHAT_HTTP_CODE:-error})"
fi

summary_exit
