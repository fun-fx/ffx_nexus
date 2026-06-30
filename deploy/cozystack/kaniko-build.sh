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

# Render manifest with sed to keep the template portable (no helm dependency).
TEMPLATE="$(cd "$(dirname "$0")" && pwd)/kaniko-build.yaml"
RENDERED="$(mktemp -t kaniko-build.XXXXXX.yaml)"
trap 'rm -f "$RENDERED"' EXIT

# Replace the placeholder --destination line to a single positional arg.
DEST="${REGISTRY}/${PROJECT}/${IMAGE}:${IMAGE_TAG}"
sed -E \
    -e "s#(--destination=)[^[:space:]]+#\1${DEST}#" \
    -e "s#(\"name\":[[:space:]]*\")[^\"]*(\")#\1${JOB_NAME}\2#" \
    "$TEMPLATE" > "$RENDERED"

# Drop any previous run of the build job; if pod is still terminating we want
# the new one to own resources afresh rather than queue.
kubectl -n "$NAMESPACE" delete job "$JOB_NAME" --ignore-not-found >/dev/null 2>&1 || true

echo "==> applying Kaniko build job (tag=${IMAGE_TAG}, dest=${DEST})"
kubectl -n "$NAMESPACE" apply -f "$RENDERED"

echo "==> waiting for build job to finish (timeout ${WAIT_TIMEOUT_SECS}s)"
if ! kubectl -n "$NAMESPACE" wait --for=condition=Complete --timeout="${WAIT_TIMEOUT_SECS}s" "job/${JOB_NAME}"; then
    echo "build job did not complete cleanly; dumping recent logs:" >&2
    kubectl -n "$NAMESPACE" logs --tail=200 "job/${JOB_NAME}" >&2 || true
    kubectl -n "$NAMESPACE" describe "job/${JOB_NAME}" >&2 || true
    exit 1
fi

kubectl -n "$NAMESPACE" logs --tail=40 "job/${JOB_NAME}" || true
echo "==> build OK: ${DEST}"
