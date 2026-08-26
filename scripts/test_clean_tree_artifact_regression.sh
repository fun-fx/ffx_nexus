#!/usr/bin/env bash
# ------------------------------------------------------------------------------
# d2b.32 / d2b.33 / d2b.34 regression compatibility alias.
#
# Historical context:
#   d2b.32 was the first self-reference-clean-tree guard,
#   d2b.33 tightened the artifact I/O contract (RUNNER_TEMP
#   capture + workspace post-pass copy), and the original
#   scripts/test_clean_tree_artifact_regression.sh was the
#   companion test for those contract variants. The d2b.33
#   contract — specifically the post-pass copy of an external
#   capture into ${GITHUB_WORKSPACE}/artifacts/integrationcni
#   — was retired by d2b.34 after run 32934127483 proved that
#   the copy path itself was the source of the same class of
#   self-reference bug we were guarding against: the gate had
#   emitted 13 inspection / readiness / fixture-manifest
#   artifacts into the workspace's artifacts/integrationcni
#   directory and a downstream raw `git status --porcelain`
#   then fail-closed them as source dirtiness.
#
# d2b.34 retires BOTH the workspace copy AND the d2b.33
# companion test. The canonical deterministic regression
# suite is now:
#
#   scripts/test_clean_tree_artifact_routing.sh
#
# and exercises 10 controls against an isolated Git sandbox:
#
#   #1  external artifact happy path           ⇒ exit 0
#   #2  exact original defect (workspace writes)
#        — reproduces the 13-entry porcelain shape
#          from run 32934127483 verbatim        ⇒ exit 10
#   #3  genuine source dirt (tracked file changed)
#                                                ⇒ exit 10
#   #4  ARTIFACTS inside the worktree           ⇒ exit 11
#   #5  ARTIFACTS unset (no /tmp fallback)      ⇒ exit 11
#   #6  capture parent is a regular file        ⇒ exit 11
#   #7  capture write fails (path is a dir)     ⇒ exit 11
#   #8  upload-root unset (inherits #5)         ⇒ exit 11
#   #9  no .gitignore weakening, no porcelain grep/awk/sed
#   #10 workflow propagation: every heavy
#       substep + actions/upload-artifact
#       path uses ${ARTIFACTS} — no workspace
#       artifacts/integrationcni route
#
# This alias preserves names so historical runbooks and CI
# checks that still invoke
# `scripts/test_clean_tree_artifact_regression.sh` continue
# to find a deterministic contract under that name. The
# alias has no test assertions of its own; it delegates
# verbatim to the canonical suite and propagates the exit
# code. Direct invocation with two positional arguments
# (workflow file, repo root) is also forwarded unchanged.
# ------------------------------------------------------------------------------
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CANONICAL="${ROOT_DIR}/scripts/test_clean_tree_artifact_routing.sh"

if [[ ! -x "$CANONICAL" ]]; then
    echo "FATAL: canonical routing-test missing or not executable at $CANONICAL" >&2
    exit 2
fi

echo "[compat] scripts/test_clean_tree_artifact_regression.sh → delegating to scripts/test_clean_tree_artifact_routing.sh (canonical d2b.34 suite, 10 controls)"
exec bash "$CANONICAL" \
  "${1:-${ROOT_DIR}/.github/workflows/cni-nightly.yml}" \
  "${2:-${ROOT_DIR}}"
