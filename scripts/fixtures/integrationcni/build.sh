#!/usr/bin/env bash
# scripts/fixtures/integrationcni/build.sh
#
# Phase D-2b.25: build the deterministic
# cni-listener fixture image used by every
# pod in scripts/fixtures/integrationcni and
# pin it by SHA256 digest in the fixture
# yamls.
#
# Why this script exists as the SINGLE build
# entrypoint:
#
#   The fixture used to reference external
#   registry images (ghcr.io/fun-fx/nexus-fixture
#   :httpecho, :metrics, :probe, :gateway, :worker)
#   by mutable tag. A registry-side rewrite or
#   transient pull failure could be misread
#   as a chart-side regression. The fix is to
#   build the fixture locally, pin the digest,
#   and rewrite every fixture yaml to reference
#   the same digest.
#
#   This script computes that digest once and
#   emits it on stdout so a follow-up sed/patch
#   step rewrites the fixture yamls. The script
#   refuses to run if docker is missing or if
#   the build cannot complete.
#
# Inputs (env):
#   IMAGE_REF (default cni-listener:local)
#     The local tag to apply to the resulting
#     image. Defaults are fine for `kind load
#     docker-image cni-listener:local` in
#     scripts/install-nexus-test.sh and the
#     subsequent digest rewrite.
#
# Outputs:
#   - IMAGE_REF id on success
#   - sha256 digest on stdout (single line)
#
# Exit codes:
#   0  SUCCESS - image built and digest emitted
#   2  DOCKER_NOT_FOUND / BUILD_FAILED - tools not available
#
set -euo pipefail

# Initialise digests up-front so the
# `set -u` strict-mode does not emit
# "unbound variable" when DIGEST_RAW is
# empty (some toolchains refuse to record a
# RepoDigests entry because the image was
# only just-built and not pushed to a
# registry). The `-u` guard then catches
# references to unset names elsewhere.
DIGEST=""
DIGEST_RAW=""

IMAGE_REF="${IMAGE_REF:-cni-listener:local}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

if ! command -v docker >/dev/null 2>&1; then
  echo 'cni-listener:build failed: docker not on PATH' >&2
  exit 2
fi

echo "== cni-listener build ==" >&2
echo "image:     $IMAGE_REF" >&2
echo "head SHA:  $(git rev-parse HEAD 2>/dev/null || echo not-in-git)" >&2

docker build \
  -f "$SCRIPT_DIR/control-netpol-gate.Dockerfile" \
  -t "$IMAGE_REF" \
  "$SCRIPT_DIR" 1>&2

DIGEST_RAW=$(docker inspect --format='{{index .RepoDigests 0}}' "$IMAGE_REF" 2>/dev/null || true)
if [[ -n "$DIGEST_RAW" && "$DIGEST_RAW" == *"@sha256:"* ]]; then
  DIGEST="${DIGEST_RAW##*@sha256:}"
elif [[ -n "$DIGEST_RAW" ]]; then
  DIGEST="$DIGEST_RAW"
fi
if [[ -z "$DIGEST" ]]; then
  DIGEST=$(docker images --no-trunc --quiet "$IMAGE_REF" | head -1)
  DIGEST="${DIGEST#sha256:}"
fi
if [[ -z "$DIGEST" ]]; then
  echo 'cni-listener:build failed: could not compute sha256 digest' >&2
  exit 2
fi

echo
echo "== result ==" >&2
echo "image: $IMAGE_REF" >&2
echo "sha256: sha256:$DIGEST" >&2
echo "fixtures will reference by @sha256:$DIGEST" >&2
echo

# Emit on stdout a single line: the digest reference
# ready for fixture yaml substitution.
echo "sha256:$DIGEST"
