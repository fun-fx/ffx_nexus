#!/usr/bin/env bash
# E2E tests for RAG eval context plumbing (nexus_eval -> eval sidecar).
set -euo pipefail

source "$(dirname "$0")/lib/e2e_common.sh"
e2e_init

FAKE_PORT=8203
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

cleanup() { stop_fake; }
trap cleanup EXIT

echo "== RAG eval context =="

start_fake
pass "fake eval-service started"

# Without contexts: RAG metrics should not be requested by the fake (contract sanity).
RESP=$(curl -sf -X POST "$FAKE_URL/evaluate" -H 'Content-Type: application/json' \
  -d '{"input":"q","output":"a","metrics":["answer_relevancy","ragas_faithfulness"]}')
if echo "$RESP" | python3 -c "import json,sys; d=json.load(sys.stdin); print(any(s.get('metric')=='ragas_faithfulness' for s in d.get('scores',[])))" | grep -q True; then
  pass "fake service returns ragas_faithfulness when requested"
else
  fail "expected ragas_faithfulness in response: $RESP"
fi

# With contexts: faithfulness score returned (simulates RemoteEvaluator with nexus_eval).
RESP=$(curl -sf -X POST "$FAKE_URL/evaluate" -H 'Content-Type: application/json' \
  -d '{"input":"Capital of France?","output":"Paris","contexts":["Paris is the capital of France."],"reference":"Paris","metrics":["ragas_faithfulness","hallucination"]}')
SCORE=$(python3 -c "import json,sys; d=json.load(sys.stdin); s=next(x for x in d['scores'] if x['metric']=='ragas_faithfulness'); print(s['score'])" <<<"$RESP" 2>/dev/null || echo "")
if [[ "$SCORE" == "0.88" ]]; then
  pass "contexts present -> ragas_faithfulness stub score"
else
  fail "unexpected faithfulness score: $RESP"
fi

summary_exit
