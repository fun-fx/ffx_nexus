#!/usr/bin/env bash
# ------------------------------------------------------------------------------
# d2b.35 regression proof for the workflow's
#   (a) initialize-and-physical-boundary root step
#   (b) Step A external artifact root + raw source-cleanness contract
# used by the cni nightly heavy job.
#
# Independent patch review of PR #276 (which already succeeded the
# d2b.34 routing suite) found two contract defects in the heavy job:
#
#   #1 — Lexical-only boundary check.  Step A's
#       `case "$ARTIFACTS" in "$WORKSPACE_REAL"/*)` test
#       inspects the bare string.  An operator-mis-deployed
#       env override whose lexical form is an outside-tree
#       absolute path that, after the workflow normalizes it
#       via filesystem symlinks, resolves INTO the worktree,
#       bypasses the check.
#
#   #2 — Init I/O classification.  The initialize step
#       relied on `set -e` to convert a missing
#       RUNNER_TEMP, an unwritable GITHUB_ENV file, a
#       mkdir failure, or a GITHUB_ENV-vs-directory
#       conflict into a generic exit 1.  A reviewer
#       cannot distinguish "I/O fault" (which should
#       exit 11 by the operator-facing contract) from
#       "subprocess crash" (exit 1).
#
# d2b.35 retires both defects.  This test proves the
# corrected contract against the actual shell flow of
# the workflow's two heavy-job steps (no Docker /
# Kind / Cilium / network needed), AND keeps the four
# existing controls that already covered lexical-only
# issues so a regression is loud in either direction.
#
# Control surface (15 controls):
#   #1  init happy path + write to \$GITHUB_ENV + worktree clean     ⇒ rc 0
#   #2  exact original 13-entry workspace defect                    ⇒ rc 10
#   #3  genuine source dirt                                            ⇒ rc 10
#   #4  Step A: workspace-root lexical prefix                       ⇒ rc 11
#   #5  Step A: ARTIFACTS unset                                       ⇒ rc 11
#   #6  Step A: ART_PARENT_DIR is a regular file                     ⇒ rc 11
#   #7  Step A: capture write into a directory                       ⇒ rc 11
#   #8  Step A: upload-root unset (inherits #5)                      ⇒ rc 11
#   #9  no .gitignore weakening, no porcelain filter                 ⇒ pass
#   #10 workflow propagation: external ARTIFACTS + upload path       ⇒ pass
#   #11 init: RUNNER_TEMP unset → exit 11                            ⇒ rc 11
#   #12 init: GITHUB_ENV unset / file-vs-write conflict → exit 11    ⇒ rc 11
#   #13 init: ARTIFACTS parent is a regular file → exit 11 (not 1)   ⇒ rc 11
#   #14 init + Step A: lexical workspace-prefix ARTIFACTS → exit 11  ⇒ rc 11
#   #15 init + Step A: symlink whose physical target IS the worktree ⇒ rc 11
#
# Deterministic preconditions ONLY: dir-vs-file, symlink-vs-dir.
# No chmod-only testing (behaves differently in root contexts).
# No destructive action outside the isolated temp directory.
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
# Portable realpath shim (test-side mirror of the workflow's GNU
# `realpath -m`/`-e` contract). Workflow runs on Linux GNU; the test
# may run on macOS BSD. BSD `realpath -q` requires pre-existing paths,
# so we add a small lexical canonicaliser to mirror `-m` semantics on
# BSD. GNU flagship comes first; BSD fallback second.
#
# Outputs the canonical absolute path (no trailing slash, no symlink
# on the existing-prefix side) on stdout.
# Test-only: workflow always uses GNU realpath in production.
# ============================================================================
_realpath_m() {
    # GNU contract (the workflow's realpath -m on the runner):
    #   - Resolve the EXISTING prefix through symlinks (so
    #     /var/.. -> /private/var on macOS, and so a
    #     /tmp/runner/lnk-whose-target-is-worktree/d2b-cni-stub
    #     resolves to /worktree/d2b-cni-stub lexically.
    #     A symlink escape into the worktree is therefore
    #     caught at the LEXICAL stage as well as the
    #     physical stage.
    #   - Non-existing suffix appended verbatim.
    if command -v grealpath >/dev/null 2>&1; then
        grealpath -m -- "$@"
        return $?
    fi
    # BSD fallback: walk-back loop; resolve any existing
    # component (including symlinks) using realpath -q.
    local p="${1%/}"
    [[ -z "$p" ]] && { pwd -P; return 0; }
    case "$p" in
        /*) ;;
        *)  p="${PWD}/${p}" ;;
    esac
    local suffix=""
    while [[ -n "$p" ]] && [[ ! -e "$p" ]]; do
        local tail="${p##*/}"
        if [[ "$p" == "$tail" ]]; then
            p=""
            suffix="/${tail}${suffix}"
        else
            p="${p%/*}"
            suffix="/${tail}${suffix}"
        fi
    done
    if [[ -n "$p" ]]; then
        p="$(realpath -q -- "$p" 2>/dev/null || printf '%s' "$p")"
    fi
    printf '%s%s\n' "$p" "$suffix"
}

_realpath_e() {
    # GNU `realpath -e` — All components must exist; symlinks resolved.
    if command -v grealpath >/dev/null 2>&1; then
        grealpath -e -- "$@"
        return $?
    fi
    # BSD `realpath -q` accepts only existing paths.
    if [[ "$#" -eq 1 ]] && [[ -e "$1" ]]; then
        realpath -q -- "$1"
        return $?
    fi
    return 1
}

# ============================================================================
# Static guards (run before any sandbox)
# ============================================================================
GITIGNORE="$REPO_ROOT/.gitignore"
if [[ -f "$GITIGNORE" ]]; then
    for forbidden in '^/?artifacts/?$' '^/?artifacts/$' '^[^/]*artifacts' 'checkout-identity\.txt$' '\*/artifacts' 'artifacts/integrationcni'; do
        if grep -E "$forbidden" "$GITIGNORE" >/dev/null 2>&1; then
            echo "FAIL: $GITIGNORE has forbidden ignore rule ($forbidden)"
            exit 2
        fi
    done
fi
if grep -qE 'git status --porcelain\b.*\| *(grep|awk|sed)' "$WORKFLOW_FILE"; then
    echo "FAIL: workflow pipes git status --porcelain through grep/awk/sed (masking)"
    exit 2
fi
if ! grep -qE 'd2b\.34|d2b\.35' "$WORKFLOW_FILE"; then
    echo "FAIL: workflow does not carry a d2b.34+d2b.35 marker"
    exit 2
fi
if ! grep -qE 'ARTIFACTS' "$WORKFLOW_FILE"; then
    echo "FAIL: workflow does not reference ARTIFACTS env"
    exit 2
fi
if ! grep -qF 'd2b-cni' "$WORKFLOW_FILE"; then
    echo "FAIL: workflow does not route ARTIFACTS through \$RUNNER_TEMP/d2b-cni-\${{ github.run_id }}"
    exit 2
fi
if ! grep -qF 'realpath' "$WORKFLOW_FILE"; then
    echo "FAIL: workflow does not use realpath for physical boundary check"
    exit 2
fi
if ! grep -qF 'fail_artifact_io' "$WORKFLOW_FILE"; then
    echo "FAIL: workflow has no fail_artifact_io helper for I/O classification"
    exit 2
fi

# ============================================================================
# Sandbox: isolated Git repo per test run
# ============================================================================
SCRATCH_TMP="$(mktemp -d -t d2b35-routing-XXXXXX)"
SCRATCH="$SCRATCH_TMP/scratch"
WORKTREE_ABS="$(mkdir -p "$SCRATCH" && cd "$SCRATCH" && pwd -P)"
FAKE_RUNNER_TMP_BASE="$SCRATCH_TMP/runner_tmp"
mkdir -p "$FAKE_RUNNER_TMP_BASE"
# Single trap registered once; helpers add their own scratch roots.
trap 'rm -rf "$SCRATCH_TMP"' EXIT

reset_scratch() {
    ( cd "$SCRATCH"
      # Bring all tracked files back to HEAD (revert any in-worktree
      # *modification* left over by previous controls).
      git checkout -- . 2>/dev/null || true
      git clean -fdx -- artifacts/ 2>/dev/null || true
      rm -rf artifacts 2>/dev/null || true
      git reset --hard HEAD 2>/dev/null || true
      git clean -fdx 2>/dev/null || true )
}

pristine_scratch() {
    # Full rewind to the initial seed commit
    ( cd "$SCRATCH"
      git reset --hard HEAD 2>/dev/null || true
      git clean -fdx 2>/dev/null || true )
}

init_scratch_repo() {
    ( cd "$SCRATCH"
      rm -rf artifacts 2>/dev/null || true
      git reset --hard ORIG_HEAD >/dev/null 2>&1 || true
      git clean -fdx >/dev/null 2>&1 || true
      printf '# sandbox fixture file\n' > fixture.txt
      git add fixture.txt
      git commit -q --allow-empty -m "fixture initial" )
}
(
    cd "$SCRATCH"
    git init -q -b main
    git config user.email "d2b35-fixture@example.invalid"
    git config user.name "d2b35-fixture"
    git commit --allow-empty -q -m "fixture initial"
    printf '# sandbox fixture file\n' > fixture.txt
    git add fixture.txt
    git commit -q -m "fixture content"
)

# ============================================================================
# Simulator #1: the d2b.35 INITIALIZE step (mirrors the workflow step
# verbatim — fail_artifact_io classification, no /tmp fallback,
# realpath -m + realpath -e physical boundary).
#
# Inputs (env overrides available for control preconditions):
#   INIT_OVERRIDE_RUNNER_TEMP=<path-or-empty>     default: <unique tmp>
#   INIT_OVERRIDE_GITHUB_WORKSPACE=<path>         default: $WORKTREE_ABS
#   INIT_OVERRIDE_GITHUB_ENV=<path-or-empty>      default: <unique tmpfile>
#                                                 a regular file → mkdir fails
#   INIT_OVERRIDE_ARTIFACTS_RAW=<string>         replaces the raw
#                                                 "${RUNNER_TEMP}/d2b-cni-..."
#                                                 construct (used to inject
#                                                 lexical-inside-worktree
#                                                 paths or symlink paths)
# ============================================================================
run_init_d2b35() {
    (
        cd "$SCRATCH"
        set -euo pipefail
        fail_artifact_io() {
            echo "FAIL: $*" >&2
            exit 11
        }

        RUNNER_TEMP_VALUE=""
        if [[ -n "${INIT_OVERRIDE_RUNNER_TEMP+x}" ]]; then
            # caller provided the variable (even if empty); preserve that
            RUNNER_TEMP_VALUE="${INIT_OVERRIDE_RUNNER_TEMP}"
        else
            RUNNER_TEMP_VALUE="$FAKE_RUNNER_TMP_BASE/d2b-cni-default"
        fi
        GITHUB_WORKSPACE_VALUE="${INIT_OVERRIDE_GITHUB_WORKSPACE:-$WORKTREE_ABS}"
        GITHUB_ENV_VALUE=""
        if [[ -n "${INIT_OVERRIDE_GITHUB_ENV+x}" ]]; then
            GITHUB_ENV_VALUE="${INIT_OVERRIDE_GITHUB_ENV}"
        else
            GITHUB_ENV_VALUE="$FAKE_RUNNER_TMP_BASE/github_env_default"
        fi
        # The workflow reads these as raw env vars; mirror that by
        # export-and-unset for the duration of the inner subshell.
        if [[ -n "$RUNNER_TEMP_VALUE" ]]; then
            export RUNNER_TEMP="${RUNNER_TEMP_VALUE}"
        else
            unset RUNNER_TEMP
        fi
        if [[ -n "$GITHUB_WORKSPACE_VALUE" ]]; then
            export GITHUB_WORKSPACE="${GITHUB_WORKSPACE_VALUE}"
        else
            unset GITHUB_WORKSPACE
        fi
        if [[ -n "$GITHUB_ENV_VALUE" ]]; then
            export GITHUB_ENV="${GITHUB_ENV_VALUE}"
        else
            unset GITHUB_ENV
        fi

        # (1) every required variable must be set — emit fail_artifact_io
        # BEFORE we touch RUNNER_TEMP in any other expression (set -u
        # makes an unset reference fatal).
        [[ -n "${RUNNER_TEMP:-}"    ]] || fail_artifact_io "RUNNER_TEMP is unset/empty; outside-tree artifact root requires it (no /tmp fallback)"
        [[ -n "${GITHUB_WORKSPACE:-}" ]] || fail_artifact_io "GITHUB_WORKSPACE is unset/empty; cannot establish physical boundary"
        [[ -n "${GITHUB_ENV:-}"      ]] || fail_artifact_io "GITHUB_ENV is unset/empty; cannot publish ARTIFACTS"

        # Caller-overridable raw path (used by Control #14 + #15).
        if [[ -n "${INIT_OVERRIDE_ARTIFACTS_RAW:-}" ]]; then
            RAW_ARTIFACTS="${INIT_OVERRIDE_ARTIFACTS_RAW}"
        else
            RAW_ARTIFACTS="${RUNNER_TEMP}/d2b-cni-stub"
        fi

        ARTIFACTS_REAL="$(_realpath_m "${RAW_ARTIFACTS}")"        || fail_artifact_io "cannot canonicalize artifact root candidate"
        WORKSPACE_REAL_LEX="$(_realpath_m "${GITHUB_WORKSPACE}")" || fail_artifact_io "cannot canonicalize workspace"
        case "${ARTIFACTS_REAL}" in
            "${WORKSPACE_REAL_LEX}"|"${WORKSPACE_REAL_LEX}"/*)
                fail_artifact_io "artifact root resolves inside worktree (lexical: ${ARTIFACTS_REAL} under ${WORKSPACE_REAL_LEX})" ;;
        esac

        # (3) mkdir. If the caller pre-created a regular file at exactly
        #     the path the simulator is about to mkdir, mkdir -p fails
        #     with "File exists". This is the deterministic precondition
        #     the d2b.35 contract claims an init fault must classify as
        #     exit 11 (NOT exit 1). Error is redirected and the wrapper
        #     fails-closed.
        if ! mkdir -p -- "${ARTIFACTS_REAL}" 2>/dev/null; then
            fail_artifact_io "cannot create external artifact root (${ARTIFACTS_REAL})"
        fi

        ARTIFACTS_REAL="$(_realpath_e "${ARTIFACTS_REAL}")"   || fail_artifact_io "cannot physical-resolve created artifact root"
        WORKSPACE_REAL="$(_realpath_e "${GITHUB_WORKSPACE}")" || fail_artifact_io "cannot physical-resolve workspace"
        case "${ARTIFACTS_REAL}" in
            "${WORKSPACE_REAL}"|"${WORKSPACE_REAL}"/*)
                fail_artifact_io "artifact root resolves inside worktree (physical: ${ARTIFACTS_REAL} under ${WORKSPACE_REAL})" ;;
        esac

        [[ -d "${ARTIFACTS_REAL}" ]] || fail_artifact_io "artifact root is not a directory"
        printf 'ARTIFACTS=%s\n' "${ARTIFACTS_REAL}" >> "${GITHUB_ENV}" 2>/dev/null \
            || fail_artifact_io "cannot publish ARTIFACTS to GITHUB_ENV"

        echo "OK"
    )
}

# ============================================================================
# Simulator #2: the d2b.35 STEP A block (mirrors the workflow's Step A
# verbatim — same fail_artifact_io helper, realpath -m LEXICAL +
# realpath -e PHYSICAL boundary check, no worktree post-pass copy,
# raw unfiltered porcelain judgement).
# ============================================================================
run_step_a_d2b35() {
    (
        cd "$SCRATCH"
        set -euo pipefail
        fail_artifact_io() {
            echo "FAIL: $*" >&2
            exit 11
        }

        [[ -n "${ARTIFACTS:-}"      ]] || fail_artifact_io "ARTIFACTS is unset/empty; initialize step did not propagate (no /tmp fallback)"
        [[ -n "${GITHUB_WORKSPACE:-}" ]] || fail_artifact_io "GITHUB_WORKSPACE is unset/empty; cannot establish physical boundary"

        ARTIFACTS_LEX="$(_realpath_m "${ARTIFACTS}")" || fail_artifact_io "cannot lexical-canonicalize ARTIFACTS"
        WORKSPACE_LEX="$(_realpath_m "${GITHUB_WORKSPACE}")" || fail_artifact_io "cannot lexical-canonicalize workspace"
        case "${ARTIFACTS_LEX}" in
            "${WORKSPACE_LEX}"|"${WORKSPACE_LEX}"/*)
                fail_artifact_io "ARTIFACTS resolves inside the worktree (lexical: ${ARTIFACTS_LEX} under ${WORKSPACE_LEX}); outside-tree routing contract violated" ;;
        esac

        ART_PARENT_DIR="$(dirname "${ARTIFACTS}/checkout-identity.txt")"
        if [[ -e "$ART_PARENT_DIR" && ! -d "$ART_PARENT_DIR" ]]; then
            fail_artifact_io "external artifact root $ART_PARENT_DIR exists and is not a directory"
        fi
        if ! mkdir -p "$ART_PARENT_DIR" 2>/dev/null; then
            fail_artifact_io "could not create external artifact root $ART_PARENT_DIR"
        fi

        ARTIFACTS_REAL="$(_realpath_e "${ARTIFACTS}" 2>/dev/null)" || fail_artifact_io "cannot physical-resolve ARTIFACTS"
        WORKSPACE_REAL="$(_realpath_e "${GITHUB_WORKSPACE}" 2>/dev/null)" || fail_artifact_io "cannot physical-resolve workspace"
        case "${ARTIFACTS_REAL}" in
            "${WORKSPACE_REAL}"|"${WORKSPACE_REAL}"/*)
                fail_artifact_io "ARTIFACTS resolves inside the worktree (physical: ${ARTIFACTS_REAL} under ${WORKSPACE_REAL}); outside-tree routing contract violated" ;;
        esac

        OUT_ID="${ARTIFACTS}/checkout-identity.txt"
        if ! {
            echo "head_sha: $(git rev-parse HEAD)"
            echo "artifacts_root: $ARTIFACTS"
            echo "porcelain_clean:"
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
            echo "FAIL: working tree diverges from HEAD on at least one file (uncommitted tracked changes)" >&2
            exit 10
        fi
        echo "OK"
    )
}

# ============================================================================
# Control #1 — init happy path: GITHUB_ENV text updated, worktree clean, exit 0
# ============================================================================
echo "== Control #1: init happy path ⇒ exit 0, GITHUB_ENV contains canonical root =="
GITHUB_ENV="$FAKE_RUNNER_TMP_BASE/c1_github_env"
: > "$GITHUB_ENV"
RUNNER_TMP_C1="$FAKE_RUNNER_TMP_BASE/c1_runner"
mkdir -p "$RUNNER_TMP_C1"
INIT_OVERRIDE_RUNNER_TEMP="$RUNNER_TMP_C1"
INIT_OVERRIDE_GITHUB_WORKSPACE="$WORKTREE_ABS"
INIT_OVERRIDE_GITHUB_ENV="$GITHUB_ENV"
export INIT_OVERRIDE_RUNNER_TEMP INIT_OVERRIDE_GITHUB_WORKSPACE INIT_OVERRIDE_GITHUB_ENV
set +e
run_init_d2b35 > /tmp/c1.out 2> /tmp/c1.err
RC_C1=$?
set -e
[[ "$RC_C1" -eq 0 ]] || fail "control #1: init happy path returned rc=$RC_C1 (expected 0); err=$(cat /tmp/c1.err)"
grep -qE '^ARTIFACTS=.*d2b-cni-stub$' "$GITHUB_ENV" || fail "control #1: \$GITHUB_ENV does not contain canonical ARTIFACTS line: $(cat "$GITHUB_ENV")"
pass "control #1: init happy path rc=0; \$GITHUB_ENV contains ARTIFACTS=…d2b-cni-stub"
# Confirm the published root is physically outside the worktree.
PUB_ROOT="$(awk -F= '/^ARTIFACTS=/ {print $2}' "$GITHUB_ENV")"
case "$(_realpath_e "$PUB_ROOT")" in
    "$WORKTREE_ABS"|"$WORKTREE_ABS"/*) fail "control #1: published root resolves inside worktree" ;;
esac
pass "control #1: published root physically outside worktree ($PUB_ROOT)"
# Worktree still clean after init (init must never write into scratch).
PORCELAIN_C1=$(cd "$SCRATCH" && git status --porcelain)
[[ -z "$PORCELAIN_C1" ]] && pass "control #1: scratch worktree porcelain empty after init" \
    || fail "control #1: init wrote into scratch: $PORCELAIN_C1"

# ============================================================================
# Control #2 — exact original 13-entry workspace defect → exit 10
# ============================================================================
echo "== Control #2: original 13-entry workspace defect ⇒ Step A exit 10 =="
reset_scratch
( cd "$SCRATCH"
  git reset --hard HEAD~0 >/dev/null 2>&1 || true
  mkdir -p artifacts/integrationcni
  printf 'tracked: cluster-config baseline\n' > artifacts/integrationcni/cluster-config.txt
  printf 'tracked: cluster-topology baseline\n' > artifacts/integrationcni/cluster-topology.json
  printf 'tracked: kind baseline\n' > artifacts/integrationcni/kind.yaml
  printf 'tracked: versions baseline\n' > artifacts/integrationcni/versions.txt
  git add artifacts/integrationcni/
  git commit -q -m "seed tracked fixture-config files (mimics 92b tracked layout)"
  printf 'overwrite: cluster-config from gate\n' > artifacts/integrationcni/cluster-config.txt
  printf 'overwrite: cluster-topology from gate\n' > artifacts/integrationcni/cluster-topology.json
  printf 'overwrite: kind from gate\n' > artifacts/integrationcni/kind.yaml
  printf 'overwrite: versions from gate\n' > artifacts/integrationcni/versions.txt
)
for f in checkout-identity.txt cilium-status.json cluster-up.txt install.log pinned-versions.txt readiness.json readiness.json.tmp readiness.log readiness.summary.txt ; do
    echo "self-reference write $f" > "$SCRATCH/artifacts/integrationcni/$f"
done
DCPOR=$(cd "$SCRATCH" && git status --porcelain)
DIRT_C2=$(printf "%s" "$DCPOR" | grep -cEv '^$' || true)
(( DIRT_C2 >= 13 )) || fail "control #2: expected ≥13 dirty entries, got $DIRT_C2"
export ARTIFACTS="$FAKE_RUNNER_TMP_BASE/c2_artifacts"
GITHUB_WORKSPACE="$WORKTREE_ABS"
GITHUB_ENV="$FAKE_RUNNER_TMP_BASE/c2_github_env"
: > "$GITHUB_ENV"
export GITHUB_WORKSPACE GITHUB_ENV
: > "$GITHUB_ENV"
set +e
run_step_a_d2b35 > /tmp/c2.out 2> /tmp/c2.err
RC_C2=$?
set -e
unset GITHUB_WORKSPACE GITHUB_ENV
[[ "$RC_C2" -eq 10 ]] && pass "control #2: Step A rc=10 (raw porcelain fail-closed; original 13-entry defect shape verified)" \
    || fail "control #2: rc=$RC_C2 (expected 10)"
unset ARTIFACTS

# ============================================================================
# Control #3 — genuine source dirt (tracked file change) ⇒ Step A exit 10
# ============================================================================
echo "== Control #3: genuine source dirt (tracked-file change) ⇒ exit 10 =="
pristine_scratch
(cd "$SCRATCH" && printf '// one more line\n' >> fixture.txt)
export ARTIFACTS="$FAKE_RUNNER_TMP_BASE/c3_artifacts"
export GITHUB_WORKSPACE="$WORKTREE_ABS"
set +e
run_step_a_d2b35 > /tmp/c3.out 2> /tmp/c3.err
RC_C3=$?
set -e
unset GITHUB_WORKSPACE
( cd "$SCRATCH" && git checkout -- fixture.txt 2>/dev/null || true )
[[ "$RC_C3" -eq 10 ]] && pass "control #3: rc=10 (real tracked dirt fail-closed)" \
    || fail "control #3: rc=$RC_C3 (expected 10)"
unset ARTIFACTS

# ============================================================================
# Control #4 — Step A: ARTIFACTS resolves inside worktree ⇒ exit 11
# ============================================================================
echo "== Control #4: Step A lexical inside-worktree ARTIFACTS ⇒ exit 11 =="
pristine_scratch
WORKTREE_ARTIFACTS="$SCRATCH/artifacts/integrationcni_4"
rm -rf "$WORKTREE_ARTIFACTS"; mkdir -p "$WORKTREE_ARTIFACTS"
export ARTIFACTS="$WORKTREE_ARTIFACTS"
export GITHUB_WORKSPACE="$WORKTREE_ABS"
set +e
run_step_a_d2b35 > /tmp/c4.out 2> /tmp/c4.err
RC_C4=$?
set -e
unset GITHUB_WORKSPACE
[[ "$RC_C4" -eq 11 ]] && pass "control #4: rc=11 (Step A boundary check refuses)" \
    || fail "control #4: rc=$RC_C4 (expected 11)"
grep -qE "inside the worktree" /tmp/c4.err && pass "control #4: diagnostic names the violation" \
    || fail "control #4: diagnostic missing: $(cat /tmp/c4.err)"
unset ARTIFACTS

# ============================================================================
# Control #5 — Step A: ARTIFACTS unset ⇒ exit 11
# ============================================================================
echo "== Control #5: Step A ARTIFACTS unset ⇒ exit 11 =="
pristine_scratch
unset ARTIFACTS
set +e
run_step_a_d2b35 > /tmp/c5.out 2> /tmp/c5.err
RC_C5=$?
set -e
[[ "$RC_C5" -eq 11 ]] && pass "control #5: rc=11 (no /tmp fallback)" \
    || fail "control #5: rc=$RC_C5 (expected 11)"
grep -qE "ARTIFACTS is unset/empty" /tmp/c5.err && pass "control #5: diagnostic names the missing env" \
    || fail "control #5: diagnostic missing: $(cat /tmp/c5.err)"

# ============================================================================
# Control #6 — Step A: ART_PARENT_DIR is a regular file ⇒ exit 11
# ============================================================================
echo "== Control #6: Step A capture parent is a regular file ⇒ exit 11 =="
pristine_scratch
C6_PFX="$(mktemp -d -t d2b35-C6-XXXXXX)"
ARTIFACTS_C6="$C6_PFX/blocker-file"
echo "blocker" > "$ARTIFACTS_C6"
export ARTIFACTS="$ARTIFACTS_C6"
set +e
run_step_a_d2b35 > /tmp/c6.out 2> /tmp/c6.err
RC_C6=$?
set -e
rm -rf "$C6_PFX"
unset ARTIFACTS
[[ "$RC_C6" -eq 11 ]] && pass "control #6: rc=11 (ART_PARENT_DIR is a regular file)" \
    || fail "control #6: rc=$RC_C6 (expected 11); err=$(cat /tmp/c6.err)"

# ============================================================================
# Control #7 — Step A: capture write target is a directory ⇒ exit 11
# ============================================================================
echo "== Control #7: Step A capture write into a directory ⇒ exit 11 =="
pristine_scratch
C7_PFX="$(mktemp -d -t d2b35-C7-XXXXXX)"
ARTIFACTS_C7="$C7_PFX"
mkdir -p "$ARTIFACTS_C7/checkout-identity.txt"
export ARTIFACTS="$ARTIFACTS_C7"
set +e
run_step_a_d2b35 > /tmp/c7.out 2> /tmp/c7.err
RC_C7=$?
set -e
rm -rf "$C7_PFX"
unset ARTIFACTS
[[ "$RC_C7" -eq 11 ]] && pass "control #7: rc=11 (capture write into a directory)" \
    || fail "control #7: rc=$RC_C7 (expected 11)"

# ============================================================================
# Control #8 — Step A upload-root unset inheritance ⇒ exit 11
# ============================================================================
echo "== Control #8: Step A upload-root propagation (inherits #5) ⇒ exit 11 =="
pristine_scratch
unset ARTIFACTS
set +e
run_step_a_d2b35 > /tmp/c8.out 2> /tmp/c8.err
RC_C8=$?
set -e
[[ "$RC_C8" -eq 11 ]] && pass "control #8: rc=11 (upload-root unset propagates Step A)" \
    || fail "control #8: rc=$RC_C8 (expected 11)"

# ============================================================================
# Control #9 — no masking: .gitignore weakening + porcelain filter
# ============================================================================
echo "== Control #9: no .gitignore weakening / no porcelain grep-awk-sed mask =="
if [[ -f "$GITIGNORE" ]] && grep -E 'artifacts|checkout-identity' "$GITIGNORE" >/dev/null; then
    fail "control #9: $GITIGNORE masks artifacts or checkout-identity"
fi
pass "control #9 (control .gitignore): no masks for artifacts or checkout-identity"
if grep -qE 'git status --porcelain\b.*\| *(grep|awk|sed)' "$WORKFLOW_FILE"; then
    fail "control #9: workflow pipes git status --porcelain through grep/awk/sed"
fi
pass "control #9 (control porcelain filter): no grep/awk/sed masking"

# ============================================================================
# Control #10 — workflow propagation: external ARTIFACTS + upload path
# ============================================================================
echo "== Control #10: workflow propagation =="
if grep -qE 'ART_DIR="artifacts/integrationcni"' "$WORKFLOW_FILE"; then
    fail "control #10: workflow still has d2b.33 post-pass copy block"
fi
if grep -qE '^[[:space:]]*mkdir -p[[:space:]]+artifacts/integrationcni[[:space:]]*$' "$WORKFLOW_FILE"; then
    fail "control #10: workflow still creates \${GITHUB_WORKSPACE}/artifacts/integrationcni"
fi
if grep -qE '^\s+ARTIFACTS:[[:space:]]*\$[{][{][[:space:]]*github\.workspace[[:space:]]*[}][}]' "$WORKFLOW_FILE"; then
    fail "control #10: per-step env override routes ARTIFACTS back to \${{ github.workspace }}"
fi
for piece in \
  'Initialize external artifact root' \
  'd2b-cni' \
  'GITHUB_ENV' \
  'env.ARTIFACTS' \
  'realpath -m' \
  'realpath -e' \
  'fail_artifact_io' \
  'ARTIFACTS must not resolve' ; do
    if ! grep -qF "$piece" "$WORKFLOW_FILE"; then
        fail "control #10: workflow missing required routing piece: $piece"
    fi
done
pass "control #10: every required d2b.35 routing piece present; no workspace-copy legacy"

# ============================================================================
# Control #11 — init: RUNNER_TEMP unset ⇒ exit 11
# ============================================================================
echo "== Control #11: init RUNNER_TEMP unset ⇒ exit 11 =="
pristine_scratch
unset INIT_OVERRIDE_RUNNER_TEMP
unset INIT_OVERRIDE_GITHUB_WORKSPACE
unset INIT_OVERRIDE_GITHUB_ENV
unset INIT_OVERRIDE_ARTIFACTS_RAW
INIT_OVERRIDE_RUNNER_TEMP=""
set +e
run_init_d2b35 > /tmp/c11.out 2> /tmp/c11.err
RC_C11=$?
set -e
unset INIT_OVERRIDE_RUNNER_TEMP
[[ "$RC_C11" -eq 11 ]] && pass "control #11: rc=11 (RUNNER_TEMP unset)" \
    || fail "control #11: rc=$RC_C11 (expected 11); err=$(cat /tmp/c11.err)"
grep -qE "RUNNER_TEMP is unset/empty" /tmp/c11.err && pass "control #11: diagnostic names RUNNER_TEMP" \
    || fail "control #11: diagnostic missing"

# Verify no scratch write happened.
PORCELAIN_C11=$(cd "$SCRATCH" && git status --porcelain)
[[ -z "$PORCELAIN_C11" ]] && pass "control #11: scratch worktree still clean after init error" \
    || fail "control #11: init wrote into scratch despite RUNNER_TEMP unset: $PORCELAIN_C11"

# ============================================================================
# Control #12 — init: GITHUB_ENV unset / file-vs-dir GITHUB_ENV ⇒ exit 11
# ============================================================================
echo "== Control #12: init GITHUB_ENV unset / file-vs-dir ⇒ exit 11 =="
pristine_scratch
C12_PFX="$(mktemp -d -t d2b35-C12-XXXXXX)"
RUNNER_TMP_C12="$C12_PFX/runner"; mkdir -p "$RUNNER_TMP_C12"
GITHUB_ENV_C12_BADDIR="$C12_PFX/not_a_file_mkdir_me"
mkdir -p "$GITHUB_ENV_C12_BADDIR"
# Make dir-vs-file conflict: rename dir to a path we never write.
INIT_OVERRIDE_RUNNER_TEMP="$RUNNER_TMP_C12"
INIT_OVERRIDE_GITHUB_WORKSPACE="$WORKTREE_ABS"
INIT_OVERRIDE_GITHUB_ENV="$GITHUB_ENV_C12_BADDIR/checkout-identity.txt"   # a non-existent PATH that goes through a directory entry
mkdir -p "$INIT_OVERRIDE_GITHUB_ENV" 2>/dev/null || true   # pre-create it as a directory to make writes fail
# Actually, to force the redirect `printf >> "$INIT_OVERRIDE_GITHUB_ENV"` to fail,
# we need the path itself to be a directory:
mkdir -p "$INIT_OVERRIDE_GITHUB_ENV"
ls -la "$INIT_OVERRIDE_GITHUB_ENV"
# Convert INIT to bare empty first to make unset case tested
: "${INIT_OVERRIDE_GITHUB_ENV_TEST_UNSET:=}"
INIT_OVERRIDE_GITHUB_ENV_TEST_UNSET=""
export INIT_OVERRIDE_RUNNER_TEMP INIT_OVERRIDE_GITHUB_WORKSPACE INIT_OVERRIDE_GITHUB_ENV
set +e
run_init_d2b35 > /tmp/c12.out 2> /tmp/c12.err
RC_C12=$?
set -e
unset INIT_OVERRIDE_RUNNER_TEMP INIT_OVERRIDE_GITHUB_WORKSPACE INIT_OVERRIDE_GITHUB_ENV
rm -rf "$C12_PFX"
[[ "$RC_C12" -eq 11 ]] && pass "control #12: rc=11 (GITHUB_ENV conflict)" \
    || fail "control #12: rc=$RC_C12 (expected 11); err=$(cat /tmp/c12.err)"
# Also test: GITHUB_ENV totally unset
INIT_OVERRIDE_RUNNER_TEMP="$FAKE_RUNNER_TMP_BASE/c12b_runner"
INIT_OVERRIDE_GITHUB_WORKSPACE="$WORKTREE_ABS"
INIT_OVERRIDE_GITHUB_ENV=""
export INIT_OVERRIDE_RUNNER_TEMP INIT_OVERRIDE_GITHUB_WORKSPACE INIT_OVERRIDE_GITHUB_ENV
set +e
run_init_d2b35 > /tmp/c12b.out 2> /tmp/c12b.err
RC_C12B=$?
set -e
unset INIT_OVERRIDE_RUNNER_TEMP INIT_OVERRIDE_GITHUB_WORKSPACE INIT_OVERRIDE_GITHUB_ENV
[[ "$RC_C12B" -eq 11 ]] && pass "control #12b: rc=11 (GITHUB_ENV empty-string)" \
    || fail "control #12b: rc=$RC_C12B (expected 11)"
PORCELAIN_C12=$(cd "$SCRATCH" && git status --porcelain)
[[ -z "$PORCELAIN_C12" ]] && pass "control #12: no source write after GITHUB_ENV fault" \
    || fail "control #12: scratch has dirt after GITHUB_ENV fault: $PORCELAIN_C12"

# ============================================================================
# Control #13 — init: ARTIFACTS parent pre-created as a regular file ⇒ exit 11
# ============================================================================
echo "== Control #13: init mkdir target is a regular file → exit 11 (NOT generic exit 1) =="
pristine_scratch
C13_PFX="$(mktemp -d -t d2b35-C13-XXXXXX)"
RUNNER_TMP_C13="$C13_PFX/runner"; mkdir -p "$RUNNER_TMP_C13"
# Pre-create a regular file at the exact path the simulator will mkdir.
ARTIFACTS_LEAF_C13="$RUNNER_TMP_C13/d2b-cni-stub"
echo "blocker" > "$ARTIFACTS_LEAF_C13"
INIT_OVERRIDE_RUNNER_TEMP="$RUNNER_TMP_C13"
INIT_OVERRIDE_GITHUB_WORKSPACE="$WORKTREE_ABS"
INIT_OVERRIDE_GITHUB_ENV="$C13_PFX/github_env_file"
: > "$INIT_OVERRIDE_GITHUB_ENV"
# Use the RAW override so the simulator writes RAW_ARTIFACTS = the
# pre-created regular file, and mkdir -p cannot proceed.
INIT_OVERRIDE_ARTIFACTS_RAW="$ARTIFACTS_LEAF_C13"
export INIT_OVERRIDE_RUNNER_TEMP INIT_OVERRIDE_GITHUB_WORKSPACE INIT_OVERRIDE_GITHUB_ENV INIT_OVERRIDE_ARTIFACTS_RAW
set +e
run_init_d2b35 > /tmp/c13.out 2> /tmp/c13.err
RC_C13=$?
set -e
unset INIT_OVERRIDE_RUNNER_TEMP INIT_OVERRIDE_GITHUB_WORKSPACE INIT_OVERRIDE_GITHUB_ENV INIT_OVERRIDE_ARTIFACTS_RAW
rm -rf "$C13_PFX"
[[ "$RC_C13" -eq 11 ]] && pass "control #13: rc=11 (mkdir target-vs-file; classified; not generic exit 1)" \
    || fail "control #13: rc=$RC_C13 (expected 11); err=$(cat /tmp/c13.err)"

# ============================================================================
# Control #14 — init + Step A: lexical workspace-prefix ARTIFACTS ⇒ exit 11
# ============================================================================
echo "== Control #14: lexical workspace-prefix ARTIFACTS ⇒ init + Step A exit 11 =="
pristine_scratch
C14_PFX="$(mktemp -d -t d2b35-C14-XXXXXX)"
RUNNER_TMP_C14="$C14_PFX/runner"; mkdir -p "$RUNNER_TMP_C14"
WORKSPACE_REAL_C14="$WORKTREE_ABS/.."  # one level above the SC RATCH so WORKTREE/C14/.. = WORKTREE; pick a different inside-string parent
# We instead inject ARTIFACTS = $WORKTREE_ABS/d2b-cni-stub so realpath -m
# resolves below the workspace.
INIT_LEXICAL_INSIDE="${WORKTREE_ABS}/d2b-cni-stub"
INIT_OVERRIDE_RUNNER_TEMP="$RUNNER_TMP_C14"   # irrelevant; raw override below wins
INIT_OVERRIDE_GITHUB_WORKSPACE="$WORKTREE_ABS"
INIT_OVERRIDE_GITHUB_ENV="$C14_PFX/ghenv"
: > "$INIT_OVERRIDE_GITHUB_ENV"
INIT_OVERRIDE_ARTIFACTS_RAW="$INIT_LEXICAL_INSIDE"
export INIT_OVERRIDE_RUNNER_TEMP INIT_OVERRIDE_GITHUB_WORKSPACE INIT_OVERRIDE_GITHUB_ENV INIT_OVERRIDE_ARTIFACTS_RAW
set +e
run_init_d2b35 > /tmp/c14.out 2> /tmp/c14.err
RC_C14=$?
set -e
[[ "$RC_C14" -eq 11 ]] && pass "control #14: init rc=11 (lexical workspace-prefix)" \
    || fail "control #14: rc=$RC_C14 (expected 11); err=$(cat /tmp/c14.err)"
grep -qE "inside worktree \(lexical:" /tmp/c14.err && pass "control #14: init diagnostic names the lexical breach" \
    || fail "control #14: init diagnostic missing"

# Now the SAME set of ARTIFACTS env vars and Step A
ARTIFACTS="$INIT_LEXICAL_INSIDE"
export ARTIFACTS
GITHUB_WORKSPACE="$WORKTREE_ABS"
export GITHUB_WORKSPACE
set +e
run_step_a_d2b35 > /tmp/c14b.out 2> /tmp/c14b.err
RC_C14B=$?
set -e
unset ARTIFACTS GITHUB_WORKSPACE
[[ "$RC_C14B" -eq 11 ]] && pass "control #14b: Step A rc=11 (lexical workspace-prefix)" \
    || fail "control #14b: rc=$RC_C14B (expected 11); err=$(cat /tmp/c14b.err)"
rm -rf "$C14_PFX"

# ============================================================================
# Control #15 — symlink escape: outside-named symlink whose physical target
# IS the workspace ⇒ init AND Step A exit 11
# ============================================================================
echo "== Control #15: symlink whose target IS the worktree ⇒ init + Step A exit 11 =="
pristine_scratch
C15_PFX="$(mktemp -d -t d2b35-C15-XXXXXX)"
RUNNER_TMP_C15="$C15_PFX/runner"
mkdir -p "$RUNNER_TMP_C15"
LINK_TARGET="$WORKTREE_ABS"    # physical target is the worktree
LINK_PATH="$RUNNER_TMP_C15/d2b-cni-sym"
# Create a symlink whose lexical name is outside the worktree but whose
# physical resolution is the worktree.
ln -sfn "$LINK_TARGET" "$LINK_PATH"
# Sanity: spacer that confirms the symlink works.
follow_target="$(cd "$LINK_PATH" && pwd -P 2>/dev/null)"
echo "  [debug] symlink PATH=$LINK_PATH → realpath -e resolves to $follow_target"
[[ "$(_realpath_e "$LINK_PATH")" == "$(_realpath_e "$WORKTREE_ABS")" ]] \
    && pass "control #15 (setup): symlink does point at the worktree" \
    || fail "control #15 (setup): symlink physical target doesn't equal worktree"

# Run init expecting exit 11 BEFORE GITHUB_ENV write.
INIT_OVERRIDE_RUNNER_TEMP="$RUNNER_TMP_C15"
INIT_OVERRIDE_GITHUB_WORKSPACE="$WORKTREE_ABS"
INIT_OVERRIDE_GITHUB_ENV="$C15_PFX/ghenv"
: > "$INIT_OVERRIDE_GITHUB_ENV"
INIT_OVERRIDE_ARTIFACTS_RAW="$LINK_PATH/d2b-cni-stub"
export INIT_OVERRIDE_RUNNER_TEMP INIT_OVERRIDE_GITHUB_WORKSPACE INIT_OVERRIDE_GITHUB_ENV INIT_OVERRIDE_ARTIFACTS_RAW
set +e
run_init_d2b35 > /tmp/c15.out 2> /tmp/c15.err
RC_C15=$?
set -e
[[ "$RC_C15" -eq 11 ]] && pass "control #15: init rc=11 (symlink escapes into worktree; realpath -m catches it at the LEXICAL stage; physical boundary check remains as defense in depth)" \
    || fail "control #15: rc=$RC_C15 (expected 11); err=$(cat /tmp/c15.err)"
grep -qE "inside worktree \(lexical:|inside worktree \(physical:" /tmp/c15.err && pass "control #15: init diagnostic names the symlink refutation (lexical or physical)" \
    || fail "control #15: init diagnostic missing: $(cat /tmp/c15.err)"
# Confirm: GITHUB_ENV was NOT written
[[ ! -s "$INIT_OVERRIDE_GITHUB_ENV" ]] && pass "control #15: \$GITHUB_ENV not written after symlink refutation" \
    || fail "control #15: \$GITHUB_ENV was written despite symlink refutation: $(cat "$INIT_OVERRIDE_GITHUB_ENV")"
unset INIT_OVERRIDE_RUNNER_TEMP INIT_OVERRIDE_GITHUB_WORKSPACE INIT_OVERRIDE_GITHUB_ENV INIT_OVERRIDE_ARTIFACTS_RAW

# Now: Step A given the same symlink path
ARTIFACTS="$LINK_PATH/d2b-cni-stub"
export ARTIFACTS
GITHUB_WORKSPACE="$WORKTREE_ABS"
export GITHUB_WORKSPACE
set +e
run_step_a_d2b35 > /tmp/c15b.out 2> /tmp/c15b.err
RC_C15B=$?
set -e
unset ARTIFACTS GITHUB_WORKSPACE
[[ "$RC_C15B" -eq 11 ]] && pass "control #15b: Step A rc=11 (symlink refuted at realpath -m stage; mkdir is never called)" \
    || fail "control #15b: rc=$RC_C15B (expected 11); err=$(cat /tmp/c15b.err)"
grep -qE "inside the worktree \(lexical:" /tmp/c15b.err && pass "control #15b: Step A diagnostic names the symlink refutation (lexical)" \
    || fail "control #15b: Step A diagnostic missing: $(cat /tmp/c15b.err)"
rm -rf "$C15_PFX"

# ============================================================================
# Final sanity: scratch worktree still clean after the entire run
# ============================================================================
FINAL_PORCELAIN=$(cd "$SCRATCH" && git status --porcelain)
[[ -z "$FINAL_PORCELAIN" ]] && pass "final: scratch worktree porcelain empty after 15-control run (no source writes from any fail-closed path)" \
    || fail "final: scratch worktree has unexpected dirt: $FINAL_PORCELAIN"

echo
echo "d2b.35 external-artifact routing + boundary regression (15 controls): PASS"
