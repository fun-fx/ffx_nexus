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

# 4) Missing required flag => error exit 2 (remote default requires -service-url).
if "$BATCH_BIN" -dataset "$DATASET" >/tmp/evalbatch_err.txt 2>&1; then
  fail "missing -service-url should error for remote evaluator"
else
  pass "missing -service-url (remote) -> non-zero exit"
fi

echo ""
echo "-- heuristic evaluator (hermetic, no eval service) --"

# 5) Heuristic mode runs without any external service.
if "$BATCH_BIN" -dataset "$DATASET" -evaluator heuristic -out /tmp/evalbatch_heur.json >/tmp/evalbatch_heur.txt 2>&1; then
  pass "heuristic batch run exits 0 (no -service-url)"
else
  fail "heuristic batch run failed: $(cat /tmp/evalbatch_heur.txt)"
fi

if grep -q '"pii_leak"' /tmp/evalbatch_heur.json && grep -q '"completeness"' /tmp/evalbatch_heur.json; then
  pass "heuristic report contains pii_leak + completeness metrics"
else
  fail "heuristic report missing expected metrics: $(cat /tmp/evalbatch_heur.json)"
fi

# Scores should be perfect on the example dataset (clean outputs, no PII).
if python3 - "$REPORT" <<'PY' 2>/dev/null; then
import json, sys
d = json.load(open("/tmp/evalbatch_heur.json"))
for m in ("pii_leak", "completeness"):
    assert d["metrics"][m]["mean_score"] == 1.0, f"{m} mean != 1.0"
PY
  pass "heuristic scores are 1.0 on example dataset"
else
  fail "expected perfect heuristic scores on example dataset"
fi

# 6) CI gate path: pass against the committed baseline.
COMMITTED_BASELINE="$ROOT/datasets/regression_baseline.json"
if [[ ! -f "$COMMITTED_BASELINE" ]]; then
  fail "committed baseline missing: $COMMITTED_BASELINE"
elif "$BATCH_BIN" -dataset "$DATASET" -evaluator heuristic -baseline "$COMMITTED_BASELINE" -tolerance 0.05 >/tmp/evalbatch_ci_gate.txt 2>&1; then
  pass "CI gate path: no regression vs committed baseline -> exit 0"
else
  fail "CI gate should pass on clean dataset: $(cat /tmp/evalbatch_ci_gate.txt)"
fi

# 7) Regression detection with heuristic evaluator (bad outputs vs inflated baseline).
python3 - "$COMMITTED_BASELINE" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
for m in d["metrics"].values():
    m["mean_score"] = 1.0
json.dump(d, open("/tmp/evalbatch_heur_inflated.json", "w"))
PY
# Dataset with empty + PII outputs scores below 1.0 on heuristics.
printf '{"id":"bad-empty","input":"q","output":""}\n{"id":"bad-pii","input":"q","output":"Email jane@example.com"}\n' >/tmp/evalbatch_heur_regress.jsonl
if "$BATCH_BIN" -dataset /tmp/evalbatch_heur_regress.jsonl -evaluator heuristic -baseline /tmp/evalbatch_heur_inflated.json -tolerance 0.05 >/tmp/evalbatch_heur_reg.txt 2>&1; then
  fail "heuristic inflated baseline should trigger regression exit 1"
else
  rc=$?
  if [[ "$rc" == "1" ]] && grep -q "REGRESSIONS" /tmp/evalbatch_heur_reg.txt; then
    pass "heuristic regression detected -> exit 1"
  else
    fail "expected exit 1 with REGRESSIONS, got rc=$rc: $(cat /tmp/evalbatch_heur_reg.txt)"
  fi
fi

# 8) Heuristic flags PII in a bad output.
printf '{"id":"bad-pii","input":"contact","output":"Email jane.doe@example.com"}\n' >/tmp/evalbatch_bad_pii.jsonl
if "$BATCH_BIN" -dataset /tmp/evalbatch_bad_pii.jsonl -evaluator heuristic -out /tmp/evalbatch_pii_report.json >/tmp/evalbatch_pii.txt 2>&1; then
  if python3 - <<'PY' 2>/dev/null; then
import json
d = json.load(open("/tmp/evalbatch_pii_report.json"))
assert d["metrics"]["pii_leak"]["mean_score"] == 0.0
PY
    pass "heuristic flags PII output (pii_leak=0.0)"
  else
    fail "expected pii_leak=0.0 on email output"
  fi
else
  fail "PII case heuristic run failed: $(cat /tmp/evalbatch_pii.txt)"
fi

# 9) Invalid -evaluator value => error.
if "$BATCH_BIN" -dataset "$DATASET" -evaluator bogus >/tmp/evalbatch_bad_eval.txt 2>&1; then
  fail "invalid -evaluator should error"
else
  pass "invalid -evaluator -> non-zero exit"
fi

summary_exit
