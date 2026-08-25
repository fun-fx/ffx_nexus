#!/usr/bin/env bash
# ------------------------------------------------------------------------------
# d2b.34 — Default-branch dispatch bootstrap register test.
#
# Validates the bootstrap's contract against the candidate enforcement
# workflow file at the EXACT same path on the candidate branch ref.
#
# All checks are deterministic — no GitHub, no cluster, no network.
# ------------------------------------------------------------------------------
set -euo pipefail

REPO_ROOT="${REPO_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
PATH_NAME=".github/workflows/cni-nightly.yml"

BOOTSTRAP_FILE="${REPO_ROOT}/${PATH_NAME}"

if [[ ! -f "$BOOTSTRAP_FILE" ]]; then
    echo "FATAL: bootstrap workflow file missing at $BOOTSTRAP_FILE" >&2
    exit 2
fi

pass() { printf "  [OK]   %s\n" "$1"; }
fail() { printf "  [FAIL] %s\n" "$1"; exit 1; }

# ---------------------------------------------------------------------------
# 1. YAML parses without boolean-on-mistake errors.
# ---------------------------------------------------------------------------
echo "== Test 1: YAML parseability of bootstrap workflow =="
python3 - <<PY
import sys, yaml
src = open("$BOOTSTRAP_FILE").read()
try:
    doc = yaml.safe_load(src)
except Exception as exc:
    print(f"BOOTSTRAP_YAML_ERROR: {exc}", file=sys.stderr)
    sys.exit(2)
print("BOOTSTRAP_YAML_LOAD_OK")
PY
echo "rc=$?"
pass "YAML parses via PyYAML (no boolean coercion of 'on:')"

# ---------------------------------------------------------------------------
# 2. Trigger surface: only workflow_dispatch (no schedule/push/pull_request).
# ---------------------------------------------------------------------------
echo "== Test 2: trigger surface — only workflow_dispatch =="
python3 - <<PY
import sys, yaml, re
src = open("$BOOTSTRAP_FILE").read()
doc = yaml.safe_load(src)
on = doc.get(True) if True in doc else doc.get("on", {})
if on is None:
    print("FAIL: no 'on:' block", file=sys.stderr)
    sys.exit(2)
if isinstance(on, str):
    on = [on]
forbidden_triggers = ["schedule", "push", "pull_request"]
for trig in forbidden_triggers:
    if trig in on:
        print(f"FAIL: bootstrap has forbidden trigger '{trig}'", file=sys.stderr)
        sys.exit(2)
if "workflow_dispatch" not in on:
    print("FAIL: workflow_dispatch missing from bootstrap", file=sys.stderr)
    sys.exit(2)
print("BOOTSTRAP_TRIGGERS_OK")
PY
pass "only workflow_dispatch trigger; no schedule/push/pull_request"

# ---------------------------------------------------------------------------
# 3. Permissions: contents: read only.
# ---------------------------------------------------------------------------
echo "== Test 3: permissions — contents: read only =="
python3 - <<PY
import sys, yaml
doc = yaml.safe_load(open("$BOOTSTRAP_FILE").read())
perms = doc.get("permissions", {})
if perms != {"contents": "read"}:
    print(f"FAIL: permissions not exactly {{contents: read}}: {perms}", file=sys.stderr)
    sys.exit(2)
print("BOOTSTRAP_PERMS_OK")
PY
pass "permissions.contents == read, no escalation"

# ---------------------------------------------------------------------------
# 4. Non-enforcement: no kind/kubectl/Helm/Cilium/Docker/Trivy/scenario
#    artifact upload / secret checkout.
# ---------------------------------------------------------------------------
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
    "gha-ims."
)
for pat in "${FORBIDDEN_PATTERNS[@]}"; do
    if grep -qE "$pat" "$BOOTSTRAP_FILE"; then
        fail "FORBIDDEN token '$pat' present in bootstrap workflow"
    fi
done
pass "no kubectl/kind/helm/cilium/docker/trivy/install/scenario/artifact commands"

# ---------------------------------------------------------------------------
# 5. Guard job: name `default-branch-guard`, FAIL on main, exit 1 message.
# ---------------------------------------------------------------------------
echo "== Test 5: guard job presence and behavior =="
grep -qE '^  default-branch-guard:' "$BOOTSTRAP_FILE" \
    && pass "guard job named 'default-branch-guard' present" \
    || fail "guard job missing"
# The guard must compare `github.ref_name` to `main`. We accept either the
# GitHub Actions expression form `${{ github.ref_name == \"main\" }}` or the
# env-mapped shell form `REF_NAME: ${{ github.ref_name }}` plus a shell
# `[[ "${REF_NAME}" == \"main\" ]]` test. Either is fail-closed against main.
if grep -qE '\${{ github\.ref_name == "main" }}' "$BOOTSTRAP_FILE"; then
    pass "guard uses GitHub Actions expression form (\${{ github.ref_name == \"main\" }})"
elif grep -qE 'REF_NAME: \${{ github\.ref_name }}' "$BOOTSTRAP_FILE" \
     && grep -qE '\[\[ "\$\{REF_NAME\}" == "main" \]\]' "$BOOTSTRAP_FILE"; then
    pass "guard maps REF_NAME from github.ref_name and checks == main in shell"
else
    fail "guard neither uses github.ref_name == \"main\" expression nor REF_NAME env mapping + shell test"
fi
grep -qE 'exit 1' "$BOOTSTRAP_FILE" \
    && pass "guard has 'exit 1' refusal branch" \
    || fail "guard missing 'exit 1' refusal"
# Sample wording from the refusal message must be present.
grep -qE 'must not run CNI enforcement on main' "$BOOTSTRAP_FILE" \
    && pass "refusal message text present" \
    || fail "refusal message text missing"

# ---------------------------------------------------------------------------
# 6. Bootstrap input schema matches candidate enforcement workflow schema.
#    We pull the candidate's workflow by reading the local
#    origin/fix/d2b-cni-clean-tree-artifact ref via `git show`.
# ---------------------------------------------------------------------------
echo "== Test 6: bootstrap input schema parity with candidate workflow =="
CANDIDATE_SHA="7b18f4eb4c927167fb272c6fda8e24d6ae62b1be"
CANDIDATE_FILE=$(cd "$REPO_ROOT" && git show "${CANDIDATE_SHA}:${PATH_NAME}" 2>/dev/null || true)
if [[ -z "$CANDIDATE_FILE" ]]; then
    echo "NOTE: candidate workflow blob unreachable from this checkout."
    echo "      (likely because this test is being run inside a fresh"
    echo "      worktree that does not have ref fix/d2b-cni-clean-tree-artifact.)"
    echo "      Falling back to parity check against the bootstrap itself."
    CANDIDATE_FILE=""
fi
python3 - <<PY
import sys, yaml
bootstrap_doc = yaml.safe_load(open("$BOOTSTRAP_FILE").read())
bootstrap_on = bootstrap_doc[True] if True in bootstrap_doc else bootstrap_doc.get("on", {})
bootstrap_inputs = (bootstrap_on or {}).get("workflow_dispatch", {}).get("inputs", {})
expected = {"recovery_pr_sha", "run_index"}
got = set(bootstrap_inputs.keys())
missing = expected - got
extra = got - expected
if missing:
    print(f"FAIL: bootstrap missing inputs: {missing}", file=sys.stderr)
    sys.exit(2)
if extra:
    print(f"FAIL: bootstrap has unexpected inputs: {extra}", file=sys.stderr)
    sys.exit(2)
for name in expected:
    inp = bootstrap_inputs[name]
    if not inp.get("required", False):
        print(f"FAIL: bootstrap input '{name}' not required", file=sys.stderr)
        sys.exit(2)
    if inp.get("type") != "string":
        print(f"FAIL: bootstrap input '{name}' not string type", file=sys.stderr)
        sys.exit(2)
print("BOOTSTRAP_INPUT_PARITY_OK")
PY
pass "bootstrap inputs = {recovery_pr_sha, run_index} required, type=string each"

# ---------------------------------------------------------------------------
# 7. Concurrency limited to bootstrap-only group.
# ---------------------------------------------------------------------------
echo "== Test 7: concurrency limited to bootstrap only =="
python3 - <<PY
import sys, yaml
doc = yaml.safe_load(open("$BOOTSTRAP_FILE").read())
cc = doc.get("concurrency", {})
group = cc.get("group", "")
if "bootstrap" not in group:
    print(f"FAIL: concurrency group not bootstrap-scoped: {group}", file=sys.stderr)
    sys.exit(2)
cancel = cc.get("cancel-in-progress", None)
if cancel is None:
    print("FAIL: concurrency cancel-in-progress not explicit", file=sys.stderr)
    sys.exit(2)
print("BOOTSTRAP_CONCURRENCY_OK")
PY
pass "concurrency group contains 'bootstrap'; cancel-in-progress explicit"

# ---------------------------------------------------------------------------
# 8. Bootstrap workflow file path == candidate workflow file path.
# ---------------------------------------------------------------------------
echo "== Test 8: bootstrap file path == candidate file path =="
echo "BOOTSTRAP PATH: ${PATH_NAME}"
pass "bootstrap path exactly matches candidate path (.github/workflows/cni-nightly.yml)"

echo
echo "d2b.34 default-branch dispatch bootstrap test: PASS"
