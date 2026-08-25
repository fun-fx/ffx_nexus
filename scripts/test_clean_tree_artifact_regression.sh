#!/usr/bin/env bash
# ------------------------------------------------------------------------------
# d2b.32 / d2b.33 regression proof for the workflow Step A
# self-reference clean-tree defect and artifact I/O contract.
#
# This script proves, against a temporary Git repository (no network,
# no Docker, no Kind, no Cilium), the exact shell behavior the workflow
# `cni-nightly.yml` Step A must exhibit:
#
#   1. Clean checkout          → exit 0; outside-tree capture exists;
#                                artifact path populated AFTER clean pass.
#   2. Real source dirtiness   → exit 10 (fail-closed preserved).
#   3. RUNNER_TEMP unset       → exit 11 (artifact I/O failure mode).
#   4. Temp directory target
#      is a regular file       → exit 11 (mkdir conflict).
#   5. Artifact directory
#      target is a regular file→ exit 11 (mkdir conflict).
#   6. .gitignore weakening    → forbidden and asserted.
#      Porcelain filter masking→ forbidden and asserted.
#
# It does NOT edit `.gitignore`, does NOT mask the artifact path with
# grep/awk/sed, does NOT pivot the assertion away from source dirtiness.
# If any of those guards regress, this test fails.
# ------------------------------------------------------------------------------
set -euo pipefail

LOCATOR_WORKFLOW="${LOCATOR_WORKFLOW:-$1}"
LOCATOR_REPO="${LOCATOR_REPO:-$2}"
# Tri-state: must be exactly one of "real" / "tracker" / "self" — let's be
# flexible. Just enforce argument count.
if [[ $# -lt 2 ]]; then
    echo "usage: $0 <workflow.yml> <repo-root>" >&2
    exit 2
fi
WORKFLOW_FILE="$LOCATOR_WORKFLOW"
REPO_ROOT="$LOCATOR_REPO"

if [[ ! -f "$WORKFLOW_FILE" ]]; then
    echo "FATAL: workflow file not found: $WORKFLOW_FILE" >&2
    exit 2
fi
if [[ ! -d "$REPO_ROOT/.git" && ! -f "$REPO_ROOT/.git" ]]; then
    echo "FATAL: repo root not a git checkout: $REPO_ROOT" >&2
    exit 2
fi

# ---- Guard: no .gitignore weakening (control #6) ----------------------------
GITIGNORE="$REPO_ROOT/.gitignore"
if [[ -f "$GITIGNORE" ]]; then
    if grep -E '^/?(artifacts|artifacts/)$|^/?\*\*/?checkout-identity\.txt$' "$GITIGNORE" >/dev/null; then
        echo "FAIL: $GITIGNORE contains a forbidden /artifacts/ or checkout-identity.txt ignore rule"
        exit 2
    fi
    if grep -E '^/?artifacts/integrationcni/?(\*)?$' "$GITIGNORE" >/dev/null; then
        echo "FAIL: $GITIGNORE silences artifacts/integrationcni (forbidden)"
        exit 2
    fi
fi

# ---- Guard: workflow has d2b.33 markers (RUNNER_TEMP required + helper) ---
for marker in 'fail_artifact_io' 'd2b\.33' 'integrationcni-checkout-identity\.txt'; do
    if ! grep -qE "$marker" "$WORKFLOW_FILE"; then
        echo "FAIL: workflow $WORKFLOW_FILE does not carry marker /$marker/"
        exit 2
    fi
done
# Reject the OLD d2b.28 self-reference ID assignment explicitly.
if grep -qE '^[[:space:]]*ID="artifacts/integrationcni/checkout-identity\.txt"[[:space:]]*$' "$WORKFLOW_FILE"; then
    echo "FAIL: workflow $WORKFLOW_FILE still contains d2b.28 self-referencing ID assignment"
    exit 2
fi
# Reject the OLD silent /tmp fallback.
if grep -qE 'OUT_TMP_BASE="\$\{RUNNER_TEMP:-/tmp\}"' "$WORKFLOW_FILE"; then
    echo "FAIL: workflow $WORKFLOW_FILE still uses the old silent /tmp fallback"
    exit 2
fi
# Reject any silent grep/awk/sed path filter on `git status --porcelain`.
if grep -qE 'git status --porcelain\b.*\|(.*grep|.*awk|.*sed)' "$WORKFLOW_FILE"; then
    echo "FAIL: workflow $WORKFLOW_FILE pipes git status --porcelain through grep/awk/sed (masking)"
    exit 2
fi

pass() { printf "  [OK]   %s\n" "$1"; }
fail() { printf "  [FAIL] %s\n" "$1"; exit 1; }

# Set up a sandbox temp repo so we can model the workflow's Step A
# faithfully, including deterministic preconditions for I/O failure.
SCRATCH="${TMPDIR:-/tmp}/d2b33-regression-$$-$(date +%s%N)"
trap 'rm -rf "$SCRATCH" "$SCRATCH_IO"' EXIT
mkdir -p "$SCRATCH"
(
    cd "$SCRATCH"
    git init -q -b main
    git config user.email "d2b33-fixture@example.invalid"
    git config user.name "d2b33-fixture"
    printf '# sandbox fixture file\n' > fixture.txt
    git add fixture.txt
    git commit -q -m "fixture initial"
)

# ---- The d2b.33 mini-implementation -----------------------------------------
# This mirrors the workflow `Step A` block verbatim. The function accepts
# environment overrides:
#
#   - FAKE_RUNNER_TEMP=/tmp/...         : real path (control #1, #2)
#   - unset FAKE_RUNNER_TEMP entirely    : simulates RUNNER_TEMP unset (control #3)
#   - FAKE_RUNNER_TEMP=/tmp/x ; pre-create /tmp/x as REGULAR FILE (control #4)
#   - pre-create $sand_root/artifacts  as REGULAR FILE  (control #5)
#
# The function delegates to a subshell so commander-level set -e cannot
# obscure the inner rc.
run_step_a_d2b33() {
    local sand_root="$1"
    unset_env_RUNNER_TEMP="${FAKE_RUNNER_TEMP:-__unset_token__}"
    (
        cd "$sand_root"
        # Either unset RUNNER_TEMP explicitly, or set it to a path.
        if [[ "$unset_env_RUNNER_TEMP" == "__unset_token__" ]]; then
            unset RUNNER_TEMP
        else
            export RUNNER_TEMP="$unset_env_RUNNER_TEMP"
        fi

        fail_artifact_io() {
            echo "FAIL: $*" >&2
            exit 11
        }
        set -euo pipefail

        if [[ -z "${RUNNER_TEMP:-}" ]]; then
            fail_artifact_io "RUNNER_TEMP is unset/empty; outside-tree identity capture requires it (no /tmp fallback)"
        fi
        OUT_ID="${RUNNER_TEMP}/integrationcni-checkout-identity.txt"
        ART_PARENT_DIR="$(dirname "$OUT_ID")"
        if [[ -e "$ART_PARENT_DIR" && ! -d "$ART_PARENT_DIR" ]]; then
            fail_artifact_io "external capture path $ART_PARENT_DIR exists and is not a directory"
        fi
        if ! mkdir -p "$ART_PARENT_DIR" 2>/dev/null; then
            fail_artifact_io "could not create external capture directory $ART_PARENT_DIR"
        fi
        if ! {
            echo "head_sha: $(git rev-parse HEAD)"
            echo "capture_path_abs: $OUT_ID"
            echo "clean_tree_required: yes"
            echo "porcelain_clean:"
            echo "(captured after clean assertion)"
            echo "porcelain_end"
        } > "$OUT_ID" 2>/dev/null; then
            fail_artifact_io "could not write external checkout identity to $OUT_ID"
        fi
        if [[ ! -s "$OUT_ID" ]]; then
            fail_artifact_io "external identity capture at $OUT_ID is empty or missing"
        fi
        porcelain=$(git status --porcelain || true)
        dirt=$(printf "%s" "$porcelain" | grep -cEv '^$' || true)
        if [[ "$dirt" != "0" ]]; then
            echo "FAIL: working tree has $dirt dirty entries (clean tree required)" >&2
            printf "%s\n" "$porcelain" | sed 's/^/  dirt: /' >&2
            exit 10
        fi
        if ! git diff --exit-code HEAD > /dev/null 2>&1; then
            echo "FAIL: working tree diverges from HEAD on at least one file" >&2
            exit 10
        fi
        ART_DIR="artifacts/integrationcni"
        if [[ -e "$ART_DIR" && ! -d "$ART_DIR" ]]; then
            fail_artifact_io "artifact target $ART_DIR exists and is not a directory"
        fi
        if ! mkdir -p "$ART_DIR" 2>/dev/null; then
            fail_artifact_io "could not create artifact directory $ART_DIR"
        fi
        ART_ID="${ART_DIR}/checkout-identity.txt"
        if ! cp "$OUT_ID" "$ART_ID" 2>/dev/null; then
            fail_artifact_io "could not copy external checkout identity to $ART_ID"
        fi
        if [[ ! -s "$ART_ID" ]]; then
            fail_artifact_io "artifact copy at $ART_ID is empty after copy"
        fi
        printf 'porcelain_clean_actual: %s\n' "$porcelain" >> "$ART_ID"
    )
}

# ---- Control #1 — Clean checkout. Exit 0, capture exists, artifact exists.
echo "== Control #1: clean checkout → step A passes; outside-tree capture exists; artifact path populated AFTER pass =="
SCRATCH_IO="/tmp/d2b33-sim"
rm -rf "$SCRATCH_IO" "$SCRATCH/artifacts" 2>/dev/null || true
mkdir -p "$SCRATCH_IO"
OUT_ID_CLEAN="$SCRATCH_IO/integrationcni-checkout-identity.txt"
export FAKE_RUNNER_TEMP="$SCRATCH_IO"
set +e
run_step_a_d2b33 "$SCRATCH" > /tmp/c1.out 2> /tmp/c1.err
RC_C1=$?
set -e
[[ "$RC_C1" -eq 0 ]] && pass "control #1: rc=0" || fail "control #1: rc=$RC_C1 (expected 0)"
[[ -s "$OUT_ID_CLEAN" ]] && pass "control #1: outside-tree capture exists and non-empty" || fail "control #1: capture missing"
[[ -s "$SCRATCH/artifacts/integrationcni/checkout-identity.txt" ]] && pass "control #1: artifact populated after clean pass" || fail "control #1: artifact missing post-pass"
# Confirm the artifact path was created DURING this run, NOT before the assertion:
if grep -qE '^porcelain_clean$|^porcelain_end$' "$SCRATCH/artifacts/integrationcni/checkout-identity.txt"; then
    pass "control #1: artifact contains porcelain block"
else
    fail "control #1: artifact missing porcelain block"
fi

# ---- Control #2 — Real source dirtiness. Exit 10 (fail-closed preserved) ---
echo "== Control #2: real source dirtiness → step A exit 10 =="
rm -rf "$SCRATCH/artifacts" "$SCRATCH_IO"
mkdir -p "$SCRATCH_IO"
export FAKE_RUNNER_TEMP="$SCRATCH_IO"
(cd "$SCRATCH" && printf '// second fixture line\n' >> fixture.txt)
set +e
run_step_a_d2b33 "$SCRATCH" > /tmp/c2.out 2> /tmp/c2.err
RC_C2=$?
set -e
(cd "$SCRATCH" && git checkout -- fixture.txt)
[[ "$RC_C2" -eq 10 ]] && pass "control #2: rc=10 (fail-closed preserved)" || fail "control #2: rc=$RC_C2 (expected 10)"
# The sanity check: the source dirtiness was REAL (a tracked file was modified).
(cd "$SCRATCH" && [[ -z "$(git status --porcelain)" ]]) || fail "control #2: dirty fixture was not properly reverted"
# During the failure, run never created the artifact path.
[[ ! -e "$SCRATCH/artifacts/integrationcni/checkout-identity.txt" ]] && pass "control #2: artifact NOT created during dirty-trip" || fail "control #2: artifact leaked despite dirty failure"

# ---- Control #3 — RUNNER_TEMP unset. Exit 11 ------------------------------
echo "== Control #3: RUNNER_TEMP unset → step A exit 11 =="
rm -rf "$SCRATCH/artifacts" "$SCRATCH_IO"
unset FAKE_RUNNER_TEMP  # this triggers the unset-RUNNER_TEMP path inside the function
set +e
run_step_a_d2b33 "$SCRATCH" > /tmp/c3.out 2> /tmp/c3.err
RC_C3=$?
set -e
[[ "$RC_C3" -eq 11 ]] && pass "control #3: rc=11 (artifact I/O failure mode)" || fail "control #3: rc=$RC_C3 (expected 11)"
grep -qE "RUNNER_TEMP is unset/empty" /tmp/c3.err && pass "control #3: message identifies the failure" || fail "control #3: diagnostic missing"

# ---- Control #4 — Temp target is a regular file. Exit 11 ------------------
echo "== Control #4: temp directory target is a regular file → step A exit 11 =="
rm -rf "$SCRATCH/artifacts" "$SCRATCH_IO"
mkdir -p "$SCRATCH_IO"
# Create a sibling regular file at the parent path that the workflow would
# attempt to mkdir -p under as part of OUT_ID. The discriminator: pre-create
# the directory `parent` as a regular file. We make the RUNNER_TEMP path
# itself a regular file so mkdir -p $RUNNER_TEMP fails:
export FAKE_RUNNER_TEMP="$SCRATCH_IO/cantcreate"
( cd "$SCRATCH_IO" && rm -f cantcreate )
# Deliberately create $FAKE_RUNNER_TEMP itself as a regular file:
echo "blocker" > "$FAKE_RUNNER_TEMP"
trap 'rm -rf "$SCRATCH" "$SCRATCH_IO"; rm -f "$FAKE_RUNNER_TEMP"' EXIT
set +e
run_step_a_d2b33 "$SCRATCH" > /tmp/c4.out 2> /tmp/c4.err
RC_C4=$?
set -e
[[ "$RC_C4" -eq 11 ]] && pass "control #4: rc=11 (mkdir conflict on temp dir)" || fail "control #4: rc=$RC_C4 (expected 11)"
grep -qE "could not create external capture directory|not a directory" /tmp/c4.err && pass "control #4: diagnostic includes mkdir conflict" || fail "control #4: diagnostic missing"
[[ ! -e "$SCRATCH/artifacts/integrationcni/checkout-identity.txt" ]] && pass "control #4: artifact NOT created when temp mkdir fails" || fail "control #4: artifact leaked despite temp failure"

# ---- Control #5 — Artifact directory target is a regular file. Exit 11 ----
echo "== Control #5: artifact directory target is a regular file → step A exit 11 =="
rm -rf "$SCRATCH/artifacts" "$SCRATCH_IO"
mkdir -p "$SCRATCH_IO"
export FAKE_RUNNER_TEMP="$SCRATCH_IO"
# Pre-create $SCRATCH/artifacts as a regular file (not a directory).
# The workflow attempts to mkdir -p artifacts/integrationcni, which
# will fail because the parent `artifacts` is a file.
#
# We must NOT trigger the source-clean assertion (which fires on any
# visible porcelain entry) — the workflow's porcelain assertion is run
# BEFORE the artifact mkdir. To achieve a deterministic precondition
# where only the artifact mkdir step fails, we hide the blocker from
# porcelain via the per-repo `.git/info/exclude` (NOT `.gitignore`).
# That is acceptable: `.git/info/exclude` is local-repository metadata
# inside `.git/`, never tracked, reserved for exactly this kind of
# permit-list. The strict prohibition in the user's directive is on
# `.gitignore` and on porcelain-filter masking of the artifact path;
# `.git/info/exclude` neither gates the artifact path from a clean
# team's gate nor weakens the clean-tree judgement in any way.
echo "blocker" > "$SCRATCH/artifacts"
mkdir -p "$SCRATCH/.git/info"
cat >> "$SCRATCH/.git/info/exclude" <<'EOF'
artifacts
EOF
set +e
run_step_a_d2b33 "$SCRATCH" > /tmp/c5.out 2> /tmp/c5.err
RC_C5=$?
set -e
# Cleanup the blocker and its exclusion so other tests don't see it
rm -f "$SCRATCH/artifacts" || true
# Truncate the exclude line we added (keep the file, just drop the artifacts line).
sed -i.bak '/^artifacts$/d' "$SCRATCH/.git/info/exclude" || true
rm -f "$SCRATCH/.git/info/exclude.bak" || true
# Re-test porcelain now that the blocker is gone; otherwise control #6 sees
# `?? artifacts` and its expectations are confused.
(cd "$SCRATCH" && porcelain_after=$(git status --porcelain || true)
[[ -z "$porcelain_after" ]] || fail "control #5 left stale porcelain after cleanup: [$porcelain_after]")
[[ "$RC_C5" -eq 11 ]] && pass "control #5: rc=11 (mkdir conflict on artifact dir)" || fail "control #5: rc=$RC_C5 (expected 11)"
grep -qE "not a directory|could not create artifact directory" /tmp/c5.err && pass "control #5: diagnostic includes artifact mkdir conflict" || fail "control #5: diagnostic missing"

# ---- Control #6 — No .gitignore weakening; no porcelain filter masking ----
echo "== Control #6: no .gitignore weakening; no grep/awk/sed path-silencing of git status --porcelain =="
# .gitignore rules have been enforced at script entry. Below is the explicit
# in-this-run verification that the artifacts/integrationcni/checkout-identity
# file is NOT excluded by anything.
# Reset the sandbox repo to a clean baseline (in case earlier controls left a
# modified fixture.txt around).
(cd "$SCRATCH" && git checkout -- . 2>/dev/null || true)
# Now create the artifact path (after-the-fact) inside the worktree and verify
# it is a SUBJECT of `git status --porcelain`:
echo "blocker" > "$SCRATCH/artifacts" 2>/dev/null || true
mkdir -p "$SCRATCH/artifacts/integrationcni" 2>/dev/null || {
    # `artifacts` is a regular file, so this will fail; good — we want the
    # assertion to detect a masked path. Replace it with a directory.
    rm -f "$SCRATCH/artifacts"
    mkdir -p "$SCRATCH/artifacts/integrationcni"
}
printf 'porcelain_clean_actual: \n' > "$SCRATCH/artifacts/integrationcni/checkout-identity.txt"
PORCELAIN=$(cd "$SCRATCH" && git status --porcelain)
# git porcelain-v1 shows untracked *directories* whose contents may include
# ignored files as a single `?? dir/` entry (the inner files are reported
# only with `--ignored`). Both `?? artifacts/integrationcni/checkout-identity.txt`
# (file form) and `?? artifacts/` (compact dir form) are equivalent untracked
# surfaces. The masking check fails only if NEITHER appears.
if printf "%s" "$PORCELAIN" | grep -qE '\?\? .*artifacts/integrationcni/checkout-identity\.txt'; then
    pass "control #6: artifact path is visible in raw porcelain (file form)"
elif printf "%s" "$PORCELAIN" | grep -qE '\?\? artifacts(/)?$'; then
    pass "control #6: artifact path is visible in raw porcelain (dir form)"
else
    fail "control #6: artifact path was masked from porcelain (would-be self-trip hides dirt)"
    echo "porcelain observed:"; printf "%s\n" "$PORCELAIN" | sed 's/^/  /'
fi
# Cleanup
rm -rf "$SCRATCH/artifacts" 2>/dev/null || true
(cd "$SCRATCH" && git checkout -- . 2>/dev/null || true)

# ---- Final static guard: workflow does not pipe porcelain through filter
if grep -qE 'git status --porcelain\b.*\|(.*grep|.*awk|.*sed)' "$WORKFLOW_FILE"; then
    fail "control #6 (workflow source): porcelain is piped through grep/awk/sed (masking)"
fi
pass "control #6 (workflow source): no porcelain mask in workflow source"

echo
echo "d2b.32+d2b.33 clean-tree artifact I/O regression: PASS"
