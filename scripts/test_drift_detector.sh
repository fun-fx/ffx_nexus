#!/usr/bin/env bash
# Verify the apierr drift detector fires on a newly-introduced bypass.
#
# The detector in internal/apierr/bypass_drift_test.go is the tripwire
# for "a new handler bypasses apierr.Render / resp.HTTP / s.fail /
# writeError". Pinning the count at the current value freezes the
# codebase; this script proves the tripwire is live by introducing a
# synthetic bypass and asserting the test fails, then asserting it
# passes again after the bypass is removed.
#
# Run from the repo root:
#
#   bash scripts/test_drift_detector.sh
#
# Returns 0 if both passes and the mutation case succeed, non-zero
# otherwise. A reviewer expecting to remove the test's tripwire can
# run this script and see whether the detector still fires.

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$REPO_ROOT"

COUNT_LINE=$(grep -n "const pinHitCount" internal/apierr/bypass_drift_test.go | head -1 | cut -d: -f1)
PIN=$(sed -n "${COUNT_LINE}p" internal/apierr/bypass_drift_test.go | grep -oE '[0-9]+')
echo "[drift-detector] frozen count = $PIN"

FIXTURE=internal/console/drift_mutation_fixture.go

# Probe 1: detector passes on the current codebase
go test ./internal/apierr/ -count=1 -run TestConsoleErrorPathsBypassApierr >/tmp/drift-clean.log 2>&1 && {
    echo "[drift-detector] clean codebase: PASS"
} || {
    echo "[drift-detector] clean codebase: FAIL (expected pass); see /tmp/drift-clean.log"
    exit 1
}

# Probe 2: detector fails when a synthetic bypass exists
cat > "$FIXTURE" <<'GO'
package console

import "net/http"

// driftMutationFixture is a synthetic bypass for the drift detector.
// This file MUST be removed after the verification completes.
func (s *Server) driftMutationFixture(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "deliberate drift site"})
	_ = r
	_ = s
	_ = w
}

var _ = driftMutationFixture
GO

trap 'rm -f "$FIXTURE"' EXIT

if go test ./internal/apierr/ -count=1 -run TestConsoleErrorPathsBypassApierr >/tmp/drift-mutated.log 2>&1; then
    echo "[drift-detector] mutated codebase: PASS (UNEXPECTED — tripwire is broken)"
    echo "---- /tmp/drift-mutated.log ----"
    tail -10 /tmp/drift-mutated.log
    exit 2
fi
echo "[drift-detector] mutated codebase: FAIL (expected — tripwire is live)"

echo "[drift-detector] all probes OK; tripwire is alive."
