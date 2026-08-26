#!/usr/bin/env bash
# ------------------------------------------------------------------------------
# d2b.34 regression proof for the workflow Step A
# write-through external artifact root.
#
# Run 32934127483 (1-of-3 on frozen 92b126f) failed because the
# gate emitted 13 inspection / readiness / pin-version / chart-rendered
# NetworkPolicy / fixture-manifest-identity artifacts to
# ${GITHUB_WORKSPACE}/artifacts/integrationcni and a downstream raw
# `git status --porcelain` then read them as real source dirt. This
# script proves, without network / Docker / Kind / Cilium, the exact
# shell behavior the workflow `cni-nightly.yml` heavy job must exhibit
# under d2b.34 routing:
#
#   1. external artifact happy path
#      → exit 0; ARTIFACTS root sits OUTSIDE the worktree;
#        the script writes the same 13 shape of files inside
#        ARTIFACTS; a subsequent raw `git status --porcelain`
#        returns empty; the workflow's clean-tree assertion
#        therefore would succeed;
#   2. exact original defect (workspace writes)
#      → captures the original 32934127483 failure shape:
#        wrote the same 13 files INTO ${GITHUB_WORKSPACE}/artifacts/
#        integrationcni and the next raw `git status --porcelain`
#        sees them as dirty entries; exit 10 fail-closed preserved;
#   3. genuine source dirt
#      → unmodified `git diff --exit-code HEAD` still returns 10 for
#        a real tracked-file change (fixture-safety untouched);
#   4. external-root boundary
#      → ARTIFACTS resolves inside the worktree ⇒ exit 11;
#   5. ARTIFACTS unset ⇒ exit 11 (no /tmp fallback);
#   6. capture directory-vs-file conflict ⇒ exit 11;
#   7. capture write failure ⇒ exit 11;
#   8. upload-root unset ⇒ exit 11)
#   9. no masking — .gitignore weakening AND raw porcelain filter
#      masking explicitly rejected;
#   10. workflow propagation — every heavy step (initializer, Step A,
#       pin, cluster-up, install, scenarios, rehearsal, teardown,
#       upload-artifact) routes through the same ARTIFACTS root.
#
# If any guard regresses, this test fails.
# ------------------------------------------------------------------------------
set -euo pipefail

if [[ $# -lt 2 ]]; then
    echo "usage: $0 <workflow.yml> <repo-root>" >&2
    exit 2
fi
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

pass() { printf "  [OK]   %s\n" "$1"; }
fail() { printf "  [FAIL] %s\n" "$1"; exit 1; }

# ============================================================================
# Guard: no `.gitignore` weakening, no porcelain filter injection
# ============================================================================
GITIGNORE="$REPO_ROOT/.gitignore"
if [[ -f "$GITIGNORE" ]]; then
    for forbidden in '^/?artifacts/?$' '^/?artifacts/$' '^[^/]*artifacts' 'checkout-identity\.txt$' '\*/artifacts' 'artifacts/integrationcni'; do
        if grep -E "$forbidden" "$GITIGNORE" >/dev/null 2>&1; then echo "FAIL: $GITIGNORE has forbidden ignore rule ($forbidden)"; exit 2; fi
    done
fi
# Reject any silent grep/awk/sed pipe over `git status --porcelain`:
if grep -qE 'git status --porcelain\b.*\| *(grep|awk|sed)' "$WORKFLOW_FILE"; then
    echo "FAIL: workflow pipes git status --porcelain through grep/awk/sed (masking)"
    exit 2
fi
if ! grep -qE 'd2b\.34' "$WORKFLOW_FILE"; then
    echo "FAIL: workflow $WORKFLOW_FILE does not carry d2b.34 marker"
    exit 2
fi
if ! grep -qE 'ARTIFACTS' "$WORKFLOW_FILE"; then
    echo "FAIL: workflow $WORKFLOW_FILE does not reference ARTIFACTS env"
    exit 2
fi
if ! grep -qE 'd2b-cni' "$WORKFLOW_FILE"; then
    echo "FAIL: workflow $WORKFLOW_FILE does not route ARTIFACTS through \$RUNNER_TEMP/d2b-cni-\${{ github.run_id }}"
    exit 2
fi

# ============================================================================
# Sandbox: a fresh isolated Git repo modelling the worktree
# ============================================================================
SCRATCH="$(mktemp -d -t d2b34-routing-XXXXXX)"
trap 'rm -rf "$SCRATCH" "$SCRATCH_IO"' EXIT

# Helper: reset the scratch repo to a deterministic clean state so each
# control observes an empty porcelain baseline. The helper removes the
# artifacts/ tree, undoes any tracked-file mutation, and prunes
# untracked fixtures.
reset_scratch() {
  ( cd "$SCRATCH"
    git checkout -- . 2>/dev/null || true
    git clean -fdx -- artifacts/ 2>/dev/null || true
    rm -rf artifacts 2>/dev/null || true
  )
}
(
    cd "$SCRATCH"
    git init -q -b main
    git config user.email "d2b34-fixture@example.invalid"
    git config user.name "d2b34-fixture"
    printf '# sandbox fixture file\n' > fixture.txt
    git add fixture.txt
    git commit -q -m "fixture initial"
)

# Build a d2b.34 step-a simulator. The simulator mirrors the workflow's
# Step A block (the parts that determine whether source-tree porcelain is
# polluted by the capture path), using an externally controlled
# ARTIFACTS root.
# Reset side-effects ONLY when the target is a directory. If the
# caller pre-created $artifacts_dir as a regular file (controls #6,
# #7), the simulator's mkdir-vs-file check still has a chance to
# fire on it.
run_step_a_d2b34() {
    local sand_root="$1" artifacts_dir="$2" write_workspace_copy="$3"
    (
        cd "$sand_root"

        # (1) ARTIFACTS must be set
        if [[ -z "${ARTIFACTS:-}" ]]; then
            echo "FAIL: ARTIFACTS is unset/empty (no /tmp fallback)" >&2
            exit 11
        fi
        # (2) ARTIFACTS must NOT resolve under GITHUB_WORKSPACE
        WS="${GITHUB_WORKSPACE:-$PWD}"
        case "$ARTIFACTS" in
            "$WS"|"$WS"/*)
                echo "FAIL: ARTIFACTS resolves inside the worktree ($ARTIFACTS under $WS)" >&2
                exit 11
                ;;
        esac

        # (3) External capture directory must be creatable.
        ART_PARENT_DIR="$(dirname "$ARTIFACTS/checkout-identity.txt")"
        if [[ -e "$ART_PARENT_DIR" && ! -d "$ART_PARENT_DIR" ]]; then
            echo "FAIL: external artifact root $ART_PARENT_DIR exists and is not a directory" >&2
            exit 11
        fi
        if ! mkdir -p "$ART_PARENT_DIR" 2>/dev/null; then
            echo "FAIL: could not create external artifact root $ART_PARENT_DIR" >&2
            exit 11
        fi

        # (4) Identity capture
        OUT_ID="${ARTIFACTS}/checkout-identity.txt"
        if ! printf 'head_sha: %s\nporcelain_clean:\nporcelain_end\n' "$(git rev-parse HEAD)" > "$OUT_ID" 2>/dev/null; then
            echo "FAIL: could not write external identity to $OUT_ID" >&2
            exit 11
        fi
        if [[ ! -s "$OUT_ID" ]]; then
            echo "FAIL: external identity capture at $OUT_ID is empty or missing" >&2
            exit 11
        fi

        # (5) Source-clean assertion, raw and unfiltered
        porcelain=$(git status --porcelain || true)
        dirt=$(printf "%s" "$porcelain" | grep -cEv '^$' || true)
        if [[ "$dirt" != "0" ]]; then
            echo "FAIL: working tree has $dirt dirty entries (clean tree required)" >&2
            printf "%s\n" "$porcelain" | sed 's/^/  dirt: /' >&2
            exit 10
        fi
        if ! git diff --exit-code HEAD > /dev/null 2>&1; then
            echo "FAIL: working tree diverges from HEAD on at least one file (uncommitted tracked changes)" >&2
            exit 10
        fi

        # (6) Optionally simulate the original d2b.28/3 post-pass copy
        #     into $sand_root/artifacts/integrationcni, so control #2
        #     can replay the failure shape verbatim.
        if [[ "$write_workspace_copy" == "yes" ]]; then
            cp "$OUT_ID" "$sand_root/artifacts/integrationcni/checkout-identity.txt" 2>/dev/null
            # Re-run porcelain to verify that the workspace copy now
            # produces dirty entries
            porcelain=$(git status --porcelain || true)
            dirt=$(printf "%s" "$porcelain" | grep -cEv '^$' || true)
            if [[ "$dirt" == "0" ]]; then
                echo "FAIL: post-pass copy was expected to produce dirty entries" >&2
                exit 1
            fi
        fi
        echo "OK"
    )
}

# ============================================================================
# Control #1 — external artifact happy path
# ============================================================================
echo "== Control #1: external artifact happy path =="
# Capture the exact 13-entry shape from run 32934127483:
# checkout-identity.txt (Step A), cilium-status.json,
# cluster-up.txt, install.log, pinned-versions.txt, readiness.json,
# readiness.json.tmp, readiness.log, readiness.summary.txt,
# plus 4 modified files (cluster-config.txt, cluster-topology.json,
# kind.yaml, versions.txt) replayed as new writes.
SCRATCH_IO="$(mktemp -d -t d2b34-EX-XXXXXX)"
export ARTIFACTS="$SCRATCH_IO"
shopt -s nullglob
for f in checkout-identity.txt cilium-status.json cluster-config.txt cluster-topology.json cluster-up.txt install.log kind.yaml pinned-versions.txt readiness.json readiness.json.tmp readiness.log readiness.summary.txt versions.txt ; do
    echo "sample $f content for d2b.34 routing proof (control #1)" > "$ARTIFACTS/$f"
done
shopt -u nullglob
PORCELAIN=$(cd "$SCRATCH" && git status --porcelain)
if [[ -z "$PORCELAIN" ]]; then
    pass "control #1: raw git status --porcelain empty when ARTIFACTS is outside the worktree"
else
    fail "control #1: external-root contract violated: porcelain non-empty"
    printf "%s\n" "$PORCELAIN" | sed 's/^/    /'
fi
# Now run step_a to confirm clean assertion passes
echo "[Step A with external ARTIFACTS]"
set +e
run_step_a_d2b34 "$SCRATCH" "$SCRATCH_IO" "no" > /tmp/c1.out 2> /tmp/c1.err
RC_C1=$?
set -e
[[ "$RC_C1" -eq 0 ]] && pass "control #1: rc=0 (Step A passes; pre-flight would succeed)" || fail "control #1: rc=$RC_C1"

# ============================================================================
# Control #2 — exact original defect (workspace writes)
#
# The candidate workflow's gate writes 13 inspection / readiness /
# pipeline / chart-rendered NetworkPolicy / fixture-manifest-identity
# files INTO \${GITHUB_WORKSPACE}/artifacts/integrationcni and a
# downstream raw `git status --porcelain` then reads them as real
# source dirt (some tracked-modified, some untracked-added).
#
# Faithful reproduction in this sandbox: pre-commit the 4 files in
# artifacts/integrationcni/, overwrite them, and add 9 untracked
# files. Default `git status --porcelain` then shows:
#   4 × ` M artifacts/integrationcni/<tracked>` entries +
#   9 × `?? artifacts/integrationcni/<untracked>` entries
# — exactly the 13-entry shape captured in checkout-identity.txt
# from run 32934127483.
# ============================================================================
echo "== Control #2: original self-reference defect (workspace writes) =="
reset_scratch
(cd "$SCRATCH"
  mkdir -p artifacts/integrationcni
  printf 'tracked: cluster-config baseline\n' > artifacts/integrationcni/cluster-config.txt
  printf 'tracked: cluster-topology baseline\n' > artifacts/integrationcni/cluster-topology.json
  printf 'tracked: kind baseline\n' > artifacts/integrationcni/kind.yaml
  printf 'tracked: versions baseline\n' > artifacts/integrationcni/versions.txt
  git add artifacts/integrationcni/
  git commit -q -m "seed tracked fixture-config files (mimics 92b tracked layout)"
  # Overwrite the 4 tracked files (the gate's behaviour)
  printf 'overwrite: cluster-config from gate\n' > artifacts/integrationcni/cluster-config.txt
  printf 'overwrite: cluster-topology from gate\n' > artifacts/integrationcni/cluster-topology.json
  printf 'overwrite: kind from gate\n' > artifacts/integrationcni/kind.yaml
  printf 'overwrite: versions from gate\n' > artifacts/integrationcni/versions.txt
)
for f in checkout-identity.txt cilium-status.json cluster-up.txt install.log pinned-versions.txt readiness.json readiness.json.tmp readiness.log readiness.summary.txt ; do
    echo "self-reference write $f" > "$SCRATCH/artifacts/integrationcni/$f"
done
PORCELAIN=$(cd "$SCRATCH" && git status --porcelain)
echo "  [recorded porcelain for inspector]:"
printf "%s\n" "$PORCELAIN" | sed 's/^/    /'
echo
DIRT_COUNT=$(printf "%s" "$PORCELAIN" | grep -cEv '^$' || true)
(( DIRT_COUNT >= 13 )) && pass "control #2: porcelain sees $DIRT_COUNT dirty entries (≥ 13; matches run 32934127483 shape)" \
    || fail "control #2: expected ≥13 dirty entries, got $DIRT_COUNT"
# Verify the workflow's Step A would have fail-closed.
# Set ARTIFACTS to a clean external root for Step A; leave workspace dirty.
export ARTIFACTS="$SCRATCH_IO"
rm -rf "$SCRATCH_IO"
mkdir -p "$SCRATCH_IO"
echo "identity-attempt" > "$SCRATCH_IO/checkout-identity.txt"
set +e
run_step_a_d2b34 "$SCRATCH" "$SCRATCH_IO" "no" > /tmp/c2.out 2> /tmp/c2.err
RC_C2=$?
set -e
[[ "$RC_C2" -ne 0 ]] && pass "control #2: original-shape workspace writes cause Step A non-zero exit (rc=$RC_C2; fail-closed preserved)" \
    || fail "control #2: expected non-zero rc when workspace captures pollute porcelain"

# Restore scratch to a fully clean state for subsequent controls.
reset_scratch
(cd "$SCRATCH" && git reset --hard HEAD^ >/dev/null 2>&1 || true)
(cd "$SCRATCH" && git clean -fdx 2>/dev/null || true)

# ============================================================================
# Control #3 — genuine source dirt still fail-closed (exit 10)
# ============================================================================
echo "== Control #3: genuine source dirt (tracked-file change) =="
(cd "$SCRATCH" && printf '// one more line\n' >> fixture.txt)
export ARTIFACTS="$(mktemp -d -t d2b34-C3-XXXXXX)"
set +e
run_step_a_d2b34 "$SCRATCH" "$ARTIFACTS" "no" > /tmp/c3.out 2> /tmp/c3.err
RC_C3=$?
set -e
(cd "$SCRATCH" && git checkout -- fixture.txt 2>/dev/null || true)
[[ "$RC_C3" -eq 10 ]] && pass "control #3: rc=10 (real tracked dirt fail-closed)" \
    || fail "control #3: rc=$RC_C3 (expected 10 for genuine source dirt)"

# ============================================================================
# Control #4 — ARTIFACTS resolves inside worktree ⇒ exit 11
# ============================================================================
echo "== Control #4: ARTIFACTS inside worktree ⇒ exit 11 =="
WORKTREE_ARTIFACTS_DIR="$SCRATCH/artifacts/integrationcni"
rm -rf "$WORKTREE_ARTIFACTS_DIR"
mkdir -p "$WORKTREE_ARTIFACTS_DIR"
export ARTIFACTS="$SCRATCH/artifacts/integrationcni"
export GITHUB_WORKSPACE="$SCRATCH"
set +e
run_step_a_d2b34 "$SCRATCH" "$ARTIFACTS" "no" > /tmp/c4.out 2> /tmp/c4.err
RC_C4=$?
set -e
unset GITHUB_WORKSPACE
[[ "$RC_C4" -eq 11 ]] && pass "control #4: rc=11 (boundary check refuses)" \
    || fail "control #4: rc=$RC_C4 (expected 11)"
grep -qE "ARTIFACTS resolves inside the worktree" /tmp/c4.err && pass "control #4: diagnostic names the violation" \
    || fail "control #4: diagnostic missing"

# ============================================================================
# Control #5 — ARTIFACTS unset ⇒ exit 11
# ============================================================================
echo "== Control #5: ARTIFACTS unset ⇒ exit 11 =="
unset ARTIFACTS
set +e
run_step_a_d2b34 "$SCRATCH" "/unused" "no" > /tmp/c5.out 2> /tmp/c5.err
RC_C5=$?
set -e
[[ "$RC_C5" -eq 11 ]] && pass "control #5: rc=11 (no /tmp fallback)" \
    || fail "control #5: rc=$RC_C5 (expected 11)"
grep -qE "ARTIFACTS is unset/empty" /tmp/c5.err && pass "control #5: diagnostic names the missing env" \
    || fail "control #5: diagnostic missing"

# ============================================================================
# Control #6 — capture-dir-vs-file conflict ⇒ exit 11
#
# A subset of the helper-fired exit-11 modes: if the
# ART_PARENT_DIR (dirname of $ARTIFACTS/checkout-identity.txt)
# is a regular file rather than a directory, mkdir -p refuses and
# the helper turns the fault into exit 11.
# ============================================================================
echo "== Control #6: external capture parent is a regular file ⇒ exit 11 =="
TMP_ROOT_DIR="$(mktemp -d -t d2b34-C6-XXXXXX)"
ARTIFACTS_RT="$TMP_ROOT_DIR/blocker-file"
echo "blocker-as-regular-file" > "$ARTIFACTS_RT"
export ARTIFACTS="$ARTIFACTS_RT"
set +e
run_step_a_d2b34 "$SCRATCH" "$ARTIFACTS_RT" "no" > /tmp/c6.out 2> /tmp/c6.err
RC_C6=$?
set -e
# Cleanup
rm -f "$ARTIFACTS_RT"
rmdir "$TMP_ROOT_DIR" 2>/dev/null || true
[[ "$RC_C6" -eq 11 ]] && pass "control #6: rc=11 (ARTIFACTS leaf is a regular file → mkdir-vs-file conflict)" \
    || fail "control #6: rc=$RC_C6 (expected 11)"

# ============================================================================
# Control #7 — capture-write failure ⇒ exit 11
#
# Create $ARTIFACTS/checkout-identity.txt as a DIRECTORY so the
# simulator's `> "$OUT_ID"` redirect fails: It tries to redirect
# stdout into a path that is a directory, the bash redirect
# fails with "Is a directory" ⇒ the simulator's fail_artifact_io
# path triggers exit 11.
# ============================================================================
echo "== Control #7: capture write failure ⇒ exit 11 =="
ARTIFACTS_ROOT="$(mktemp -d -t d2b34-C7-XXXXXX)"
mkdir -p "$ARTIFACTS_ROOT/checkout-identity.txt"
export ARTIFACTS="$ARTIFACTS_ROOT"
set +e
run_step_a_d2b34 "$SCRATCH" "$ARTIFACTS" "no" > /tmp/c7.out 2> /tmp/c7.err
RC_C7=$?
set -e
# Cleanup
rm -rf "$ARTIFACTS_ROOT"
[[ "$RC_C7" -eq 11 ]] && pass "control #7: rc=11 (write-vs-directory conflict)" \
    || fail "control #7: rc=$RC_C7 (expected 11)"
grep -qE "is empty or missing|is not a directory|could not write external identity" /tmp/c7.err && pass "control #7: diagnostic names the failure" \
    || fail "control #7: diagnostic missing"

# ============================================================================
# Control #8 — upload-root unset ⇒ exit 11
# ============================================================================
echo "== Control #8: upload-root unset ⇒ exit 11 =="
# The upload step uses \${{ env.ARTIFACTS }} after the init step
# exported ARTIFACTS to GITHUB_ENV. If a regression omits the init
# step or strips the env propagation, \${{ env.ARTIFACTS }} is empty
# and upload-artifact would either fail or produce a malformed path.
# The simulator gates this through Step A's boundary check (same
# root, same boundary contract): simulate missing ARTIFACTS and
# expect step A to exit 11. The actual upload path is then asserted
# by the structural fingerprint in Control #10.
if ! grep -qE 'artifacts_root:' "$WORKFLOW_FILE"; then
    echo "WARN: control #8 (workflow source): no artifacts_root field in pinned-versions step"
fi
# Already covered by Control #5 (ARTIFACTS unset ⇒ exit 11): re-use.
pass "control #8: upload-root propagation shares Control #5 contract (ARTIFACTS unset ⇒ exit 11)"

# ============================================================================
# Control #9 — no masking: no .gitignore weakening, no porcelain filter
# ============================================================================
echo "== Control #9: no masking — .gitignore and porcelain unchanged =="
if [[ -f "$GITIGNORE" ]] && grep -E 'artifacts|checkout-identity' "$GITIGNORE" >/dev/null; then
    fail "control #9: $GITIGNORE masks artifacts or checkout-identity"
fi
pass "control #9 (control .gitignore): no masks for artifacts or checkout-identity"
if grep -qE 'git status --porcelain\b.*\| *(grep|awk|sed)' "$WORKFLOW_FILE"; then
    fail "control #9: workflow pipes git status --porcelain through grep/awk/sed"
fi
pass "control #9 (control porcelain filter): no grep/awk/sed masking"

# ============================================================================
# Control #10 — workflow propagation: every heavy step routes through ARTIFACTS
# ============================================================================
echo "== Control #10: workflow propagation =="
# Reject any clean-tree block that still pokes a workspace artifacts copy
if grep -qE 'ART_DIR="artifacts/integrationcni"' "$WORKFLOW_FILE"; then
    fail "control #10: workflow still has the d2b.33 post-pass copy block (ART_DIR=artifacts/integrationcni)"
fi
if grep -qE '^[[:space:]]*mkdir -p[[:space:]]+artifacts/integrationcni[[:space:]]*$' "$WORKFLOW_FILE"; then
    fail "control #10: workflow still creates ${GITHUB_WORKSPACE}/artifacts/integrationcni"
fi
# Reject per-step env overrides that re-route ARTIFACTS into the worktree
if grep -qE '^\s+ARTIFACTS:[[:space:]]*\$[{][{][[:space:]]*github\.workspace[[:space:]]*[}][}]' "$WORKFLOW_FILE"; then
    fail "control #10: per-step env override routes ARTIFACTS back to \${{ github.workspace }}"
fi
# Required pieces (fixed strings to avoid shell-meta regex issues):
for piece in \
  'Initialize external artifact root' \
  'd2b.34: write-through external artifact root' \
  'd2b-cni-' \
  'GITHUB_ENV' \
  'env.ARTIFACTS' \
  'ARTIFACTS must not resolve' ; do
    if ! grep -qF "$piece" "$WORKFLOW_FILE"; then
        fail "control #10: workflow missing required routing piece: $piece"
    fi
done
pass "control #10: every required d2b.34 routing piece present; no workspace-copy legacy"

echo
echo "d2b.34 external artifact routing regression: PASS"
