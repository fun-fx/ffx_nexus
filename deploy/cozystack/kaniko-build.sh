#!/usr/bin/env bash
# In-cluster image build with kaniko, parameterized by tag.
#
# Usage:
#   IMAGE_TAG=0.3.6 ./deploy/cozystack/kaniko-build.sh
#   IMAGE_TAG=main-latest ./deploy/cozystack/kaniko-build.sh
#
# The script reuses deploy/cozystack/kaniko-build.yaml as a template and
# patches the `:TAG` destination before applying the Job. The Job then builds
# the multi-stage Dockerfile from refs/heads/main of github.com/fun-fx/ffx_nexus
# and pushes the result to harbor.<tailnet>.ts.net/ffx/nexus:$IMAGE_TAG.
#
# Run from a kubectl-handling machine (operator laptop or CI runner with
# cluster admin). For automated run-on-push-to-main, the cd-prod.yml workflow
# invokes this script with IMAGE_TAG=main-latest.

set -euo pipefail

: "${IMAGE_TAG:?must set IMAGE_TAG (e.g. 0.3.6 or main-latest)}"
: "${NAMESPACE:=tenant-nexus}"
: "${REGISTRY:=harbor.<tailnet>.ts.net}"
: "${PROJECT:=ffx}"
: "${IMAGE:=nexus}"
: "${REVISION:=refs/heads/main}"
: "${JOB_NAME:=nexus-build}"
: "${WAIT_TIMEOUT_SECS:=600}"
# Git basic-auth for cloning the PRIVATE repo context. In CI these come from
# the workflow (github.actor + ephemeral GITHUB_TOKEN); for a manual operator
# build, export a PAT first. Left empty they will simply produce an auth error.
: "${GIT_USERNAME:=}"
: "${GIT_TOKEN:=}"

# Render manifest with sed to keep the template portable (no helm dependency).
# GNU sed-only `0,/PATTERN/{...}` first-occurrence syntax was tempting but
# breaks under BSD sed (macOS / busybox). We instead use python3 which is
# universally available in CI runners and in the operator's typical macOS
# environment (`brew install python` or the bundled Xcode toolchain).
TEMPLATE="$(cd "$(dirname "$0")" && pwd)/kaniko-build.yaml"
RENDERED="$(mktemp -t kaniko-build.XXXXXX.yaml)"
trap 'rm -f "$RENDERED"' EXIT

DEST="${REGISTRY}/${PROJECT}/${IMAGE}:${IMAGE_TAG}"
REVISION="${REVISION}" \
JOB_NAME="${JOB_NAME}" \
DEST="${DEST}" \
GIT_USERNAME="${GIT_USERNAME}" \
GIT_TOKEN="${GIT_TOKEN}" \
    python3 - "$TEMPLATE" "$RENDERED" <<'PY'
import os, sys
src, dst = sys.argv[1], sys.argv[2]
replacements = {
    "PLACEHOLDER_DESTINATION":  os.environ["DEST"],
    "PLACEHOLDER_JOB_NAME":     os.environ["JOB_NAME"],
    "PLACEHOLDER_REVISION":     os.environ["REVISION"],
    "PLACEHOLDER_GIT_USERNAME": os.environ.get("GIT_USERNAME", ""),
    "PLACEHOLDER_GIT_TOKEN":    os.environ.get("GIT_TOKEN", ""),
}
text = open(src).read()
for needle, val in replacements.items():
    if text.count(needle) != 1:
        sys.exit(f"expected exactly one occurrence of {needle}, "
                 f"got {text.count(needle)}; bailing out")
    text = text.replace(needle, val, 1)
open(dst, "w").write(text)
PY

# Drop any previous run of the build job; if pod is still terminating we want
# the new one to own resources afresh rather than queue.
kubectl -n "$NAMESPACE" delete job "$JOB_NAME" --ignore-not-found >/dev/null 2>&1 || true

echo "==> applying Kaniko build job (tag=${IMAGE_TAG}, dest=${DEST})"
kubectl -n "$NAMESPACE" apply -f "$RENDERED"

echo "==> waiting for build job to finish (timeout ${WAIT_TIMEOUT_SECS}s)"
# `kubectl wait --for=condition=Complete` never returns on a Failed job, so a
# broken build would hang here for the full timeout. Poll for *either* terminal
# condition and bail the moment the job fails.
deadline=$(( $(date +%s) + WAIT_TIMEOUT_SECS ))
status=""
while :; do
    if kubectl -n "$NAMESPACE" get "job/${JOB_NAME}" \
        -o 'jsonpath={.status.conditions[?(@.type=="Complete")].status}' 2>/dev/null \
        | grep -q True; then
        status="complete"; break
    fi
    if kubectl -n "$NAMESPACE" get "job/${JOB_NAME}" \
        -o 'jsonpath={.status.conditions[?(@.type=="Failed")].status}' 2>/dev/null \
        | grep -q True; then
        status="failed"; break
    fi
    if (( $(date +%s) >= deadline )); then
        status="timeout"; break
    fi
    sleep 5
done

if [[ "$status" != "complete" ]]; then
    echo "build job did not complete cleanly (status=${status}); dumping recent logs:" >&2
    kubectl -n "$NAMESPACE" logs --tail=200 "job/${JOB_NAME}" >&2 || true
    kubectl -n "$NAMESPACE" describe "job/${JOB_NAME}" >&2 || true
    exit 1
fi

kubectl -n "$NAMESPACE" logs --tail=40 "job/${JOB_NAME}" || true
echo "==> build OK: ${DEST}"
