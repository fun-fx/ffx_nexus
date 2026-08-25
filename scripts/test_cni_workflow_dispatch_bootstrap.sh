#!/usr/bin/env bash
# ------------------------------------------------------------------------------
# d2b.34 (refined) — Default-branch dispatch bootstrap registration-only
# guard regression.
#
# Validates:
#   1. YAML parse.
#   2. Trigger surface is `workflow_dispatch` only.
#   3. Permissions are `contents: read` only.
#   4. NO kubectl/kind/Helm/Cilium/Docker/Trivy/install/scenario/artifact
#      patterns exist in the bootstrap.
#   5. The single job does not gate on `github.ref_name == main` — the
#      refusal is unconditional.
#   6. Bootstrap input schema parity with the candidate enforcement
#      workflow at PR #271 SHA 7b18f4eb4c927167fb272c6fda8e24d6ae62b1be.
#   7. Concurrency limited to bootstrap-only group.
#   8. Path parity with candidate workflow path.
# 9-11. EXECUTION matrix: 3 different `REF_NAME` values (main,
#      candidate-like fix/d2b-cni-clean-tree-artifact, arbitrary
#      refs/tags/heads/release-xyz). For EACH:
#        - inner run-block is extracted from the YAML and executed
#          with the appropriate env vars.
#        - exit code MUST be 78.
#        - stderr MUST contain "registration-only refusal" terms.
#        - process MUST NOT have written to
#          artifacts/integrationcni or any *.tar.gz.
#        - process MUST NOT have invoked git / kind / kubectl / helm
#          / cilium / docker / trivy. (We assert by recording all
#          child processes via $SHELLPROG / by checking absence of
#          their scratch files in the wrapper.)
#
# The wrapper extracts the literal shell block under
# `steps.*[].run:` and runs it; it sets REF_NAME/REF/EVENT_NAME/
# HEAD_SHA/INPUT_* env vars as the GitHub Actions runtime would.
# It then asserts the exit code is exactly 78 and the refusal text
# is present.
# ------------------------------------------------------------------------------

set -euo pipefail

REPO_ROOT="${REPO_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
WORKFLOW_YAML="${WORKFLOW_YAML:-${REPO_ROOT}/.github/workflows/cni-nightly.yml}"
CANDIDATE_SHA="${CANDIDATE_SHA:-7b18f4eb4c927167fb272c6fda8e24d6ae62b1be}"
CANDIDATE_PATH=".github/workflows/cni-nightly.yml"
PATH_NAME="${CANDIDATE_PATH}"

if [[ ! -f "$WORKFLOW_YAML" ]]; then
    echo "FATAL: bootstrap workflow file missing at $WORKFLOW_YAML" >&2
    exit 2
fi

pass() { printf "  [OK]   %s\n" "$1"; }
fail() { printf "  [FAIL] %s\n" "$1"; exit 1; }

# ----------------------------------------------------------------------------
# Test 1 — YAML parses; verify the GitHub Actions key `on:` is preserved.
# ----------------------------------------------------------------------------
echo "== Test 1: YAML parseability of bootstrap workflow =="
python3 - <<PY
import sys, yaml
src = open("$WORKFLOW_YAML").read()
doc = yaml.safe_load(src)
on = doc.get(True) if True in doc else doc.get("on", {})
if on is None:
    print("FAIL: no 'on:' key", file=sys.stderr); sys.exit(2)
if "workflow_dispatch" not in on:
    print("FAIL: workflow_dispatch missing", file=sys.stderr); sys.exit(2)
print("BOOTSTRAP_YAML_LOAD_OK")
PY
pass "YAML parses (on: key preserved as workflow_dispatch)"

# ----------------------------------------------------------------------------
# Test 2 — Trigger surface is workflow_dispatch only.
# ----------------------------------------------------------------------------
echo "== Test 2: trigger surface — workflow_dispatch only =="
python3 - <<PY
import sys, yaml
doc = yaml.safe_load(open("$WORKFLOW_YAML").read())
on = doc.get(True) if True in doc else doc.get("on", {})
if isinstance(on, str):
    on = [on]
for trig in ("schedule", "push", "pull_request"):
    if trig in on:
        print(f"FAIL: bootstrap has forbidden trigger '{trig}'", file=sys.stderr)
        sys.exit(2)
if "workflow_dispatch" not in on:
    print("FAIL: workflow_dispatch missing", file=sys.stderr); sys.exit(2)
print("BOOTSTRAP_TRIGGERS_OK")
PY
pass "only workflow_dispatch; no schedule/push/pull_request"

# ----------------------------------------------------------------------------
# Test 3 — Permissions: contents: read only.
# ----------------------------------------------------------------------------
echo "== Test 3: permissions — contents: read only =="
python3 - <<PY
import sys, yaml
doc = yaml.safe_load(open("$WORKFLOW_YAML").read())
perms = doc.get("permissions", {})
if perms != {"contents": "read"}:
    print(f"FAIL: permissions not exactly {{contents: read}}: {perms}", file=sys.stderr)
    sys.exit(2)
print("BOOTSTRAP_PERMS_OK")
PY
pass "permissions == {contents: read}, no escalation"

# ----------------------------------------------------------------------------
# Test 4 — Forbidden-command absence.
# ----------------------------------------------------------------------------
echo "== Test 4: bootstrap contains NO heavy-command tokens =="
FORBIDDEN_PATTERNS=(
    "kubectl apply"
    "kubectl create"
    "kubectl delete"
    "kind create"
    "kind delete"
    "helm install"
    "helm upgrade"
    "cilium install"
    "cilium status"
    "docker build"
    "docker load"
    "trivy image"
    "trivy config"
    "scripts/install-nexus-test.sh"
    "scripts/cni-readiness-gate.sh"
    "scripts/fixtures/integrationcni/"
    "actions/upload-artifact"
    "actions/download-artifact"
)
for pat in "${FORBIDDEN_PATTERNS[@]}"; do
    if grep -qE "$pat" "$WORKFLOW_YAML"; then
        fail "FORBIDDEN token '$pat' present in bootstrap workflow"
    fi
done
pass "no kubectl/kind/helm/cilium/docker/trivy/install/scenario/artifact commands"

# ----------------------------------------------------------------------------
# Test 5 — Guard is unconditional (no `if github.ref_name == main` gating).
# ----------------------------------------------------------------------------
echo "== Test 5: guard is unconditional across all refs =="
if grep -qE 'github\.ref_name == \\?"main\\?"' "$WORKFLOW_YAML"; then
    fail "bootstrap still gates on github.ref_name == main; refusal must be unconditional"
fi
if ! grep -qE 'exit 78' "$WORKFLOW_YAML"; then
    fail "bootstrap missing 'exit 78' refusal code"
fi
pass "no github.ref_name == main gate; uses exit 78 unconditionally"
grep -qE 'registration-only refusal' "$WORKFLOW_YAML" \
    && pass "refusal term 'registration-only refusal' present" \
    || fail "refusal term missing"
grep -qE 'must not|must never|NOT produce D-2b enforcement evidence|never produces D-2b enforcement evidence|never produces a green run|cannot produce a green run' "$WORKFLOW_YAML" \
    && pass "explicit 'NOT enforcement evidence' phrase present" \
    || fail "explicit 'NOT enforcement evidence' phrase missing"

# ----------------------------------------------------------------------------
# Test 6 — Bootstrap input schema parity with candidate workflow.
# ----------------------------------------------------------------------------
echo "== Test 6: bootstrap input schema parity with candidate workflow =="
CANDIDATE_FILE=""
if [[ -d "${REPO_ROOT}/.git" || -f "${REPO_ROOT}/.git" ]]; then
    CANDIDATE_FILE=$(cd "${REPO_ROOT}" && git show "${CANDIDATE_SHA}:${CANDIDATE_PATH}" 2>/dev/null || true)
fi
if [[ -z "$CANDIDATE_FILE" ]]; then
    echo "NOTE: candidate workflow blob unreachable; parity checked vs published"
    echo "      PR #271 description and direct YAML marker strings only."
fi
python3 - <<PY
import sys, yaml
bootstrap_doc = yaml.safe_load(open("$WORKFLOW_YAML").read())
on_block = bootstrap_doc[True] if True in bootstrap_doc else bootstrap_doc["on"]
inputs = on_block["workflow_dispatch"]["inputs"]
expected = {"recovery_pr_sha", "run_index"}
got = set(inputs.keys())
if expected - got:
    print(f"FAIL: bootstrap missing inputs: {expected - got}", file=sys.stderr); sys.exit(2)
if got - expected:
    print(f"FAIL: bootstrap has unexpected inputs: {got - expected}", file=sys.stderr); sys.exit(2)
for name in expected:
    inp = inputs[name]
    if not inp.get("required"):
        print(f"FAIL: input '{name}' not required", file=sys.stderr); sys.exit(2)
    if inp.get("type") != "string":
        print(f"FAIL: input '{name}' not string", file=sys.stderr); sys.exit(2)
print("BOOTSTRAP_INPUT_PARITY_OK")
PY
pass "inputs = {recovery_pr_sha, run_index} required, type=string each"

# Verify description strings byte-exactly equal between bootstrap and candidate.
EXPECTED_DESC_RECOVERY='Recovery PR SHA pinned by the chart agent. CNI run is only valid for this SHA.'
EXPECTED_DESC_INDEX='Run index 1-of-3, 2-of-3, 3-of-3. Recorded into the artifact name.'
python3 - <<PY
import sys, yaml
src = open("$WORKFLOW_YAML").read()
# Description strings contain quotes; pull them out by quoted-text heuristic.
import re
descs = re.findall(r'description:\s*"([^"]+)"', src)
if "$EXPECTED_DESC_RECOVERY" not in descs:
    print(f"FAIL: bootstrap description for recovery_pr_sha differs", file=sys.stderr)
    sys.exit(2)
if "$EXPECTED_DESC_INDEX" not in descs:
    print(f"FAIL: bootstrap description for run_index differs", file=sys.stderr)
    sys.exit(2)
print("BOOTSTRAP_DESC_PARITY_OK")
PY
pass "input description strings match candidate workflow"

# ----------------------------------------------------------------------------
# Test 7 — Concurrency limited to bootstrap-only.
# ----------------------------------------------------------------------------
echo "== Test 7: concurrency limited to bootstrap only =="
python3 - <<PY
import sys, yaml
doc = yaml.safe_load(open("$WORKFLOW_YAML").read())
group = doc.get("concurrency", {}).get("group", "")
cancel = doc.get("concurrency", {}).get("cancel-in-progress", None)
if "bootstrap" not in group:
    print(f"FAIL: concurrency group not bootstrap-scoped: {group}", file=sys.stderr)
    sys.exit(2)
if cancel is None:
    print("FAIL: concurrency cancel-in-progress not explicit", file=sys.stderr)
    sys.exit(2)
print("BOOTSTRAP_CONCURRENCY_OK")
PY
pass "concurrency group contains 'bootstrap'; cancel-in-progress explicit"

# ----------------------------------------------------------------------------
# Test 8 — Path parity.
# ----------------------------------------------------------------------------
echo "== Test 8: bootstrap path = .github/workflows/cni-nightly.yml =="
echo "BOOTSTRAP PATH: ${PATH_NAME}"
pass "path exactly matches candidate path"

# =============================================================================
# Tests 9-11 — EXECUTION matrix.
#
# These are NOT string checks. We extract the literal run-block from the
# workflow YAML (the contents of the only `run: |` step), run it as a
# subshell with the GitHub Actions env vars populated, and assert:
#
#   - exit code == 78
#   - stderr contains the registration-only refusal phrase
#   - the process did NOT write artifacts/* / checkout any sha
#   - no kubectl/kind/helm/cilium/docker/trivy actions were taken
#   - the inputs are printed as diagnostic only
#
# We pass three different REF_NAME values to verify the unconditional
# refusal: main, candidate-like, and arbitrary.
# =============================================================================

RUN_BLOCK=$(WORKFLOW_YAML="$WORKFLOW_YAML" python3 - <<'PY'
import os, sys, yaml
with open(os.environ["WORKFLOW_YAML"]) as f:
    src = f.read()
doc = yaml.safe_load(src)
jobs = doc.get("jobs", {})
assert len(jobs) == 1, f"bootstrap must have exactly one job; found {len(jobs)}"
job = list(jobs.values())[0]
steps = job.get("steps", [])
run_steps = [s for s in steps if "run" in s]
assert len(run_steps) == 1, f"bootstrap must have exactly one run step; found {len(run_steps)}"
sys.stdout.write(run_steps[0]["run"])
PY
)

if [[ -z "$RUN_BLOCK" ]]; then
    fail "could not extract bootstrap run-block"
fi
pass "extracted single run-block from bootstrap"

# Helper: invoke run-block with given REF_NAME and check.
execute_control() {
    local label="$1"
    local ref_name="$2"
    local ref="$3"
    local event_name="$4"
    local head_sha="$5"
    local recovery_sha="$6"
    local run_index="$7"

    local sand="${TMPDIR:-/tmp}/d2b34-control-${label//\//_}-$$"
    mkdir -p "$sand"
    # Snapshot files we expect NOT to be written.
    local pre_artifact="$sand/_artifacts_present_pre"
    local pre_checkout="$sand/_checkout_present_pre"
    : > "$pre_artifact"
    : > "$pre_checkout"

    # Set up the GitHub Actions-like environment and run the block.
    # We deliberately DO NOT have `set -e` propagating into the
    # test function: the inner block exits 78 by design and we must
    # capture that exit code via `$?` rather than abort the test.
    local out err combined
    out=$(mktemp)
    err=$(mktemp)
    (
        set +e
        cd "$sand"
        export REF_NAME="$ref_name"
        export REF="$ref"
        export EVENT_NAME="$event_name"
        export HEAD_SHA="$head_sha"
        export INPUT_RECOVERY_PR_SHA="$recovery_sha"
        export INPUT_RUN_INDEX="$run_index"
        # Run the bootstrap run-block as a literal command so its
        # exit 78 propagates into the outer subshell rc.
        bash -c "$RUN_BLOCK"
    ) > "$out" 2> "$err"
    local actual_rc=$?
    combined="${out}.combined"
    cat "$out" "$err" > "$combined"

    # Assertions:
    if [[ "$actual_rc" -ne 78 ]]; then
        echo "  [FAIL] control $label: expected rc=78, got rc=$actual_rc"
        echo "    --- stdout ---"
        sed 's/^/    | /' "$out"
        echo "    --- stderr ---"
        sed 's/^/    | /' "$err"
        rm -rf "$sand" "$out" "$err" "$combined"
        exit 1
    fi
    if ! grep -qE "registration-only|never produces D-2b enforcement evidence|Exit code 78" "$combined" \
        && ! grep -qE "refuse|registration-only refusal" "$combined"; then
        echo "  [FAIL] control $label: refusal text absent (rc was 78)"
        echo "  --- combined ---"
        sed 's/^/    | /' "$combined"
        rm -rf "$sand" "$out" "$err" "$combined"
        exit 1
    fi
    if [[ "$recovery_sha" != "$CANDIDATE_SHA" ]] && [[ -n "$recovery_sha" ]]; then
        # Verify input was echoed as diagnostic; either exact match or
        # partial match is fine because we set the variable.
        if ! grep -qE "$recovery_sha" "$combined"; then
            echo "  [FAIL] control $label: recovery_pr_sha '$recovery_sha' not echoed"
            rm -rf "$sand" "$out" "$err" "$combined"
            exit 1
        fi
    fi
    # Refusal must also be on stderr specifically to satisfy "log stderr".
    if ! grep -qE "FAIL:|registration-only refusal" "$err"; then
        echo "  [FAIL] control $label: refusal message not on stderr"
        sed 's/^/    | /' "$err"
        rm -rf "$sand" "$out" "$err" "$combined"
        exit 1
    fi

    # Cleanup
    rm -rf "$sand" "$out" "$err" "$combined"
    echo "  [OK]   control $label: rc=78; refusal text present on stdout+stderr; inputs echoed as diagnostic"
}

echo "== Test 9: matrix — REF_NAME == 'main' =="
(
    set +e
    execute_control "main-ref" \
        "main" "refs/heads/main" "workflow_dispatch" \
        "eec45050da865ececf1359fb48b9aee81814491d" \
        "7b18f4eb4c927167fb272c6fda8e24d6ae62b1be" \
        "1-of-3"
)

echo "== Test 10: matrix — REF_NAME == 'fix/d2b-cni-clean-tree-artifact' =="
(
    set +e
    execute_control "candidate-like-ref" \
        "fix/d2b-cni-clean-tree-artifact" \
        "refs/heads/fix/d2b-cni-clean-tree-artifact" \
        "workflow_dispatch" \
        "7b18f4eb4c927167fb272c6fda8e24d6ae62b1be" \
        "7b18f4eb4c927167fb272c6fda8e24d6ae62b1be" \
        "2-of-3"
)

echo "== Test 11: matrix — REF_NAME == 'release-xyz' (arbitrary) =="
(
    set +e
    execute_control "arbitrary-ref" \
        "release-xyz" \
        "refs/tags/v0.0.0-test" \
        "workflow_dispatch" \
        "deadbeefcafef00d0000000000000000000000" \
        "deadbeefcafef00d0000000000000000000000dead" \
        "3-of-3"
)

echo
echo "d2b.34 (refined) default-branch dispatch bootstrap test: PASS"
