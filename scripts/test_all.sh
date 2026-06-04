#!/usr/bin/env bash
# Run the full E2E suite for all implemented Nexus features.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

SCRIPTS=(
  test_phase2.sh
  test_phase234.sh
  test_eval_routing.sh
  test_zero_dep.sh
  test_guardrails.sh
)

FAILED=0
for s in "${SCRIPTS[@]}"; do
  echo ""
  echo "########################################################"
  echo "# Running scripts/$s"
  echo "########################################################"
  if ! "./scripts/$s"; then
    FAILED=$((FAILED + 1))
  fi
done

echo ""
echo "== Full suite =="
if [[ "$FAILED" -gt 0 ]]; then
  echo "  $FAILED script(s) failed"
  exit 1
fi
echo "  all ${#SCRIPTS[@]} scripts passed"
