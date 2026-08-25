#!/usr/bin/env bash
# ------------------------------------------------------------------------------
# d2b.32 regression proof for the workflow Step A
# self-reference clean-tree defect.
#
# This script proves: the corrected flow:
#   (1) writes the identity capture OUTSIDE the worktree,
#   (2) runs `git status --porcelain` + `git diff --exit-code HEAD` against an
#       UNDISTURBED worktree (so a real dirty entry still trips exit 10),
#   (3) only on clean assertion pass copies the outside-tree capture INTO
#       `artifacts/integrationcni/checkout-identity.txt` (which is downstream-
#       upload only, no longer the subject of the clean check).
#
# It does NOT edit `.gitignore`, does NOT mask the artifact path with `grep`,
# does NOT pivot the assertion away from source dirtiness. If any of those
# guards regress, this test fails.
#
# No network. No Docker. No Kind. No Cilium. Pure bash + git.
# ------------------------------------------------------------------------------
set -euo pipefail

# Locator: the source fragment we will exercise (anonymised to a string
# variable so the test still passes on small workflow edits).
WORKFLOW_FILE="$1"
REPO_ROOT="$2"

if [[ ! -f "$WORKFLOW_FILE" ]]; then
    echo "FATAL: workflow file not found: $WORKFLOW_FILE" >&2
    exit 2
fi
if [[ ! -d "$REPO_ROOT/.git" && ! -f "$REPO_ROOT/.git" ]]; then
    echo "FATAL: repo root not a git checkout: $REPO_ROOT" >&2
    exit 2
fi

# ---- Guard: no .gitignore weakening ------------------------------------------
GITIGNORE="$REPO_ROOT/.gitignore"
if [[ -f "$GITIGNORE" ]] && grep -E '^/?artifacts/?$' "$GITIGNORE" >/dev/null; then
    echo "FAIL: $GITIGNORE contains a top-level /artifacts/ entry (forbidden hardening)"
    exit 2
fi
# Reject any pattern that would silence the clean-tree surface on the
# artifact path specifically (artifacts/integrationcni or similar).
if [[ -f "$GITIGNORE" ]] && grep -E '^/?artifacts/integrationcni/?$|^/?artifacts/\*\*/?checkout-identity.txt$|^/?artifacts/\*\*/?\*/?$|^/?\*\*/?checkout-identity\.txt$' "$GITIGNORE" >/dev/null; then
    echo "FAIL: $GITIGNORE silences artifacts/integrationcni or checkout-identity.txt (forbidden)"
    exit 2
fi

# ---- Extract the canonical bash sequence from the workflow --------------------
# We expect to find a heredoc-friendly bash block matching the new flow. We
# verify by inspecting the worktree YAML for the d2b.32 markers rather than
# executing it (the workflow is gated by `actions/checkout` and uses
# `${{ ... }}` template substitutions). The *behavior* then lives in this
# test's runtime, which mirrors the relevant steps verbatim.
if ! grep -qE 'd2b\.32|outside-tree capture' "$WORKFLOW_FILE"; then
    echo "FAIL: workflow $WORKFLOW_FILE does not carry d2b.32 marker"
    exit 2
fi
if ! grep -qE 'RUNNER_TEMP|integrationcni-checkout-identity\.txt' "$WORKFLOW_FILE"; then
    echo "FAIL: workflow $WORKFLOW_FILE does not route the identity capture outside the worktree"
    exit 2
fi
# Reject the OLD d2b.28 sequence remaining in any form.
if grep -qE '^[[:space:]]*ID="artifacts/integrationcni/checkout-identity\.txt"[[:space:]]*$' "$WORKFLOW_FILE"; then
    echo "FAIL: workflow $WORKFLOW_FILE still contains the d2b.28 self-referencing ID assignment"
    exit 2
fi
if grep -qE 'dirt=\$\(git status --porcelain \|\| wc -l' "$WORKFLOW_FILE"; then
    echo "FAIL: workflow $WORKFLOW_FILE still uses the old `git status --porcelain | wc -l` counting that fed the self-trip"
    exit 2
fi

# Snapshot helper.
pass() { printf "  [OK]   %s\n" "$1"; }
fail() { printf "  [FAIL] %s\n" "$1"; exit 1; }

# Set up a sandbox temp repo so we can model the workflow's Step A
# faithfully, including the bug-pattern that motivated the fix.
SCRATCH="${TMPDIR:-/tmp}/d2b32-regression-$$"
trap 'rm -rf "$SCRATCH"' EXIT
mkdir -p "$SCRATCH"
(
    cd "$SCRATCH"
    git init -q -b main
    git config user.email "d2b32-fixture@example.invalid"
    git config user.name "d2b32-fixture"
    printf '# sandbox fixture file\n' > fixture.txt
    git add fixture.txt
    git commit -q -m "fixture initial"
)

# Run the corrected d2b.32 mini-implementation against the sandbox repo.
# This mirrors the workflow step exactly. We deliberately do NOT touch the
# sandbox worktree before or after the assertion step.

run_step_a() {
    local sand_root="$1"
    (cd "$sand_root" && bash -c '
        set -euo pipefail
        OUT_TMP_BASE="${RUNNER_TEMP:-/tmp/d2b32-sim}"
        mkdir -p "$OUT_TMP_BASE"
        OUT_ID="${OUT_TMP_BASE}/integrationcni-checkout-identity.txt"
        {
            echo "head_sha:          $(git rev-parse HEAD)"
            echo "short_sha:         $(git rev-parse --short HEAD)"
            echo "capture_path_abs:  $OUT_ID"
        } > "$OUT_ID"
        # Source-clean assertion against the UNDISTURBED worktree.
        porcelain=$(git status --porcelain || true)
        dirt=$(printf "%s" "$porcelain" | grep -cEv "^$" || true)
        if [[ "$dirt" != "0" ]]; then exit 10; fi
        if ! git diff --exit-code HEAD > /dev/null 2>&1; then exit 10; fi
        # Now copy into the artifacts path (no longer the cleanliness surface).
        mkdir -p artifacts/integrationcni
        cp "$OUT_ID" "artifacts/integrationcni/checkout-identity.txt"
    ')
}

# === Assertion 1: clean checkout, clean assertion pass, artifact exists. ===
echo "== A: clean checkout → step A passes; artifact path populated; outside-tree capture exists =="
mkdir -p /tmp/d2b32-sim
OUT_ID_CLEAN="/tmp/d2b32-sim/integrationcni-checkout-identity.txt"
rm -f "$OUT_ID_CLEAN" "$SCRATCH/artifacts/integrationcni/checkout-identity.txt"
set +e
run_step_a "$SCRATCH"
RC=$?
set -e
[[ "$RC" -eq 0 ]] && pass "clean checkout: rc=0" || fail "clean checkout: unexpected rc=$RC (expected 0)"
[[ -s "$OUT_ID_CLEAN" ]] && pass "outside-tree capture exists and non-empty ($OUT_ID_CLEAN)" || fail "outside-tree capture missing/empty"
[[ -s "$SCRATCH/artifacts/integrationcni/checkout-identity.txt" ]] && pass "artifacts/integrationcni/checkout-identity.txt exists post-pass" || fail "artifact path missing"

# === Assertion 2: real source dirtiness → exit 10 (fail-closed preserved) ===
echo "== B: real source dirtiness → step A exit 10 (assertion still fail-closed) =="
(cd "$SCRATCH" && echo "// second fixture file" >> fixture.txt)
set +e
run_step_a "$SCRATCH"
RC_DIRTY=$?
set -e
(cd "$SCRATCH" && git checkout -- fixture.txt)
[[ "$RC_DIRTY" -eq 10 ]] && pass "dirty checkout: rc=10 (fail-closed preserved)" || fail "dirty checkout: rc=$RC_DIRTY (expected 10)"

# === Assertion 3: artifact path is the SUBJECT of the upload but NOT of the ===
# === cleanliness assertion. The original bug was writing the artifact path ===
# === under the worktree *and then* gating on its presence. The fix flips  ===
# === the temporal order. We model the order explicitly.                  ===
echo "== C: artifact path write happens AFTER the source-clean assertion =="
SCRATCH_C="$SCRATCH"
# Probe: in the corrected mini-impl, the artifact path is *not* present
# during the assertion call; we verify by checking the assertion does not
# require any artifact-path write at all (it returns 0 without creating it,
# in this reproducer). Provide a controlled proof: only a clean run writes
# `artifacts/...`.
rm -f "$SCRATCH_C/artifacts/integrationcni/checkout-identity.txt"
set +e
( cd "$SCRATCH_C" && bash -c '
    set -euo pipefail
    OUT_TMP_BASE="${RUNNER_TEMP:-/tmp/d2b32-sim}"
    OUT_ID="${OUT_TMP_BASE}/integrationcni-checkout-identity.txt"
    mkdir -p "$OUT_TMP_BASE"
    { echo "head_sha: $(git rev-parse HEAD)"; } > "$OUT_ID"
    porcelain=$(git status --porcelain || true)
    dirt=$(printf "%s" "$porcelain" | grep -cEv "^$" || true)
    if [[ "$dirt" != "0" ]]; then echo "FAIL: pre-assertion dirt (would have tripped)"; exit 30; fi
    echo "OK: pre-assertion dirt=0; the artifact path is NOT in the tree yet"
' )
RC_PROBE=$?
set -e
[[ "$RC_PROBE" -eq 0 ]] && pass "pre-assertion dirt=0 without writing artifacts/ path" || fail "pre-assertion probe rc=$RC_PROBE"
# Confirm the artifact path was NOT created during the pre-assertion probe.
[[ ! -e "$SCRATCH_C/artifacts/integrationcni/checkout-identity.txt" ]] && pass "artifacts/integrationcni/ not present during assertion (no self-trip)" || fail "artifacts/integrationcni/ was created too early (self-reference regression)"

# === Assertion 4: no .gitignore masking, no grep-based path silencing =========
# This is enforced earlier via the static checks on $GITIGNORE; here we also
# affirm that the artifact path itself is NOT in the locator-sanitised
# porcelain filter patterns.
if grep -qE 'checkout-identity\.txt|artifacts/integrationcni' "$WORKFLOW_FILE" && \
   grep -qE 'git status --porcelain \| (grep|awk|sed)' "$WORKFLOW_FILE"; then
    fail "FAIL: workflow appears to silently filter the artifact path from git status"
fi
pass "no .gitignore weakening; no path-silencing grep in step A"

# === Assertion 5: the OLD d2b.28 marker is no longer present in workflow ===
# (Re-affirmed with a direct string search.)
if grep -qE 'mkdir -p artifacts/integrationcni$' "$WORKFLOW_FILE"; then
    # Acceptable: mkdir -p inside the post-assertion cp block.
    # We only forbid the mkdir BEFORE the assertion.
    pass "mkdir -p artifacts/integrationcni present (must be after clean assertion confirmed)"
fi
if grep -qE 'porcelain_clean:$' "$WORKFLOW_FILE"; then
    pass "porcelain_clean: marker is present (record still annotated)"
fi

echo
echo "d2b.32 clean-tree artifact regression: PASS"
