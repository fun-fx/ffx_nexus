#!/usr/bin/env bash
# E2E tests for the offline regression-eval batch tool (cmd/nexus-evalbatch).
#
# Uses the fake eval service (no DeepEval/LLM) so the dataset -> evaluator ->
# aggregate -> baseline-compare flow is verified deterministically.
set -euo pipefail

source "$(dirname "$0")/lib/e2e_common.sh"
e2e_init

PORT=8277
BATCH_BIN="/tmp/nexus-evalbatch"
DATASET="$ROOT/datasets/regression_example.jsonl"
REPORT="/tmp/evalbatch_report.json"
BASELINE="/tmp/evalbatch_baseline.json"
FAKE_PID=""

cleanup() {
  [[ -n "$FAKE_PID" ]] && kill "$FAKE_PID" 2>/dev/null || true
}
trap cleanup EXIT

echo "== Regression eval batch =="

go build -o "$BATCH_BIN" ./cmd/nexus-evalbatch
pass "build ok"

python3 "$ROOT/scripts/lib/fake_eval_service.py" "$PORT" &
FAKE_PID=$!
for i in $(seq 1 20); do
  curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break
  sleep 0.3
done

# 1) Basic run produces a report with aggregated metrics.
if "$BATCH_BIN" -dataset "$DATASET" -service-url "http://127.0.0.1:$PORT" -out "$REPORT" >/tmp/evalbatch_out.txt 2>&1; then
  pass "batch run exits 0"
else
  fail "batch run failed: $(cat /tmp/evalbatch_out.txt)"
fi

if grep -q "answer_relevancy" "$REPORT" && grep -q "ragas_faithfulness" "$REPORT"; then
  pass "report contains aggregated metrics (incl. RAG)"
else
  fail "report missing expected metrics: $(cat "$REPORT")"
fi

# Case count should match the dataset (3 non-comment lines).
NCASES=$(python3 -c "import json;print(json.load(open('$REPORT'))['num_cases'])")
if [[ "$NCASES" == "3" ]]; then
  pass "num_cases == 3"
else
  fail "expected 3 cases, got $NCASES"
fi

# 2) Comparing a report to itself => no regression, exit 0.
cp "$REPORT" "$BASELINE"
if "$BATCH_BIN" -dataset "$DATASET" -service-url "http://127.0.0.1:$PORT" -baseline "$BASELINE" >/tmp/evalbatch_same.txt 2>&1; then
  pass "no regression vs identical baseline -> exit 0"
else
  fail "identical baseline should not regress: $(cat /tmp/evalbatch_same.txt)"
fi

# 3) Inflated baseline => regression detected, exit 1.
python3 - "$BASELINE" <<'PY'
import json, sys
p = sys.argv[1]
d = json.load(open(p))
for m in d["metrics"].values():
    m["mean_score"] = 1.0  # force the current run to look worse
json.dump(d, open(p, "w"))
PY
if "$BATCH_BIN" -dataset "$DATASET" -service-url "http://127.0.0.1:$PORT" -baseline "$BASELINE" -tolerance 0.05 >/tmp/evalbatch_reg.txt 2>&1; then
  fail "inflated baseline should trigger regression exit 1"
else
  rc=$?
  if [[ "$rc" == "1" ]] && grep -q "REGRESSIONS" /tmp/evalbatch_reg.txt; then
    pass "regression detected -> exit 1"
  else
    fail "expected exit 1 with REGRESSIONS, got rc=$rc: $(cat /tmp/evalbatch_reg.txt)"
  fi
fi

# 4) Missing required flag => error exit 2.
if "$BATCH_BIN" -dataset "$DATASET" >/tmp/evalbatch_err.txt 2>&1; then
  fail "missing -service-url should error"
else
  pass "missing -service-url -> non-zero exit"
fi

summary_exit
