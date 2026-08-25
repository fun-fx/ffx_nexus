#!/usr/bin/env bash
# scripts/fixtures/integrationcni/build.sh
#
# Phase D-2b.26: build the deterministic
# cni-listener fixture image and emit a
# STRUCTURED per-image artifact so a downstream
# verifier can correlate the image on every kind
# node to the build that produced it.
#
# Why this script exists as the SINGLE build
# entrypoint:
#
#   The fixture used to reference external
#   registry images (ghcr.io/fun-fx/nexus-fixture:
#   httpecho, metrics, probe, gateway, worker) by
#   mutable tag. A registry-side rewrite or a
#   transient pull failure could be misread as a
#   chart-side regression. The fix is to build the
#   fixture locally, pin its identity (docker image
#   ID + sha256 digest where available), and emit a
#   JSON artifact any CI step can verify.
#
# What the script guarantees on success:
#
#   1. cni-listener:local docker image exists.
#   2. docker inspect reports a non-empty Id
#      (a 64-char hex string for layerfs images,
#      or `sha256:<digest>` for BuildKit). Empty
#      Id is a hard build failure (exit 11).
#   3. JSON artifact
#        $ARTIFACTS/fixture-image-digest.json
#      contains AT MINIMUM:
#        image_ref
#        image_id
#        repo_digest_or_none   ("none" when no
#                               RepoDigests entry
#                               exists, which is
#                               expected for a local
#                               tag that was never
#                               pushed to any
#                               registry)
#        build_sha            (commit head SHA;
#                               "not-in-git" if the
#                               checkout is detached
#                               / bare)
#        build_timestamp_utc  (RFC 3339 timestamp)
#        docker_version
#        exit_classification  ("build_success" or
#                               one of the explicit
#                               error labels below)
#
# What runtime contract this enables:
#
#   scripts/install-nexus-test.sh reads the JSON
#   artifact, extracts image_ref + image_id, runs
#   `kind load docker-image`, then for every kind
#   node fetches `crictl images` (or equivalent)
#   and verifies the recorded image_id is present.
#   If any node lacks it, install-nexus-test.sh
#   exits FIXTURE_IMAGE_NOT_LOADED before any
#   fixture's Pod spec is applied to the API.
#
# Inputs (env):
#   IMAGE_REF  default cni-listener:local
#   ARTIFACTS  default $PWD/artifacts/integrationcni
#
# Outputs:
#   - stdout: a single JSON line that the
#     install-nexus-test.sh captures as the
#     primary "did the build succeed"
#     signal. Older callers reading just
#     `sha256:<digest>` continue to work
#     because the JSON line contains the
#     digest.
#   - file: $ARTIFACTS/fixture-image-digest.json
#
# Exit codes:
#   0   BUILD_SUCCESS
#   11  BUILD_FAILED_NO_IMAGE_ID  - docker build ok
#                                  but inspect Id empty
#   12  BUILD_FAILED_NO_ARTIFACT  - JSON write failed
#   2   DOCKER_NOT_FOUND          - docker CLI missing
set -euo pipefail

DIGEST=""
DIGEST_RAW=""
IMAGE_ID=""
REPO_DIGEST="none"
EXIT_CLASS="build_success"

IMAGE_REF="${IMAGE_REF:-cni-listener:local}"
ARTIFACTS="${ARTIFACTS:-${PWD}/artifacts/integrationcni}"
mkdir -p "$ARTIFACTS"
ARTIFACT_JSON="$ARTIFACTS/fixture-image-digest.json"

cleanup() {
  # Run only on failure paths to print a
  # useful diagnostic. The success path
  # writes the artifact and exits 0 with
  # stdout already containing the JSON
  # signal.
  local rc=$?
  if (( rc != 0 )); then
    echo "cni-listener:build exit=$rc ExitClass=$EXIT_CLASS" >&2
  fi
}
trap cleanup EXIT

if ! command -v docker >/dev/null 2>&1; then
  EXIT_CLASS="build_failed_docker_missing"
  echo 'cni-listener:build failed: docker not on PATH' >&2
  exit 2
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "== cni-listener build ==" >&2
echo "image:     $IMAGE_REF" >&2
echo "head SHA:  $(git rev-parse HEAD 2>/dev/null || echo not-in-git)" >&2

# Run docker build, additionally capturing
# the full command output for the artifact
# so a verifier can correlate the build log
# with the resulting image.
BUILD_LOG="$ARTIFACTS/fixture-image-build.log"
{
  docker build \
    -f "$SCRIPT_DIR/control-netpol-gate.Dockerfile" \
    -t "$IMAGE_REF" \
    "$SCRIPT_DIR"
} >"$BUILD_LOG" 2>&1
echo "build log: $BUILD_LOG ($(wc -l <"$BUILD_LOG") lines)" >&2

# Inspect the resulting image. The Id field is
# mandatory; empty means the previous build
# silently rolled back or docker returned a
# half-built image (rare but observed).
INSPECT_JSON="$ARTIFACTS/fixture-image-inspect.json"
if ! docker inspect "$IMAGE_REF" >"$INSPECT_JSON" 2>"$ARTIFACTS/fixture-image-inspect.err"; then
  EXIT_CLASS="build_failed_inspect_errored"
  echo "cni-listener:build failed: docker inspect returned non-zero" >&2
  cat "$ARTIFACTS/fixture-image-inspect.err" >&2 || true
  exit 11
fi

# Extract Id (and the literal `sha256:` prefix
# if present) and RepoDigests[0].
IMAGE_ID=$(python3 - "$INSPECT_JSON" <<'PY'
import json,sys
try:
    d = json.loads(sys.stdin.read() if False else open(sys.argv[1]).read())
except Exception as exc:
    print('err:parse:' + str(exc), file=sys.stderr)
    sys.exit(0)
if not isinstance(d, list) or not d:
    print('', end='')
    sys.exit(0)
img = d[0]
print(img.get('Id', '') or '', end='')
PY
)
# Strip the leading sha256: prefix if any,
# leaving the 64-char hex string for the JSON.
IMAGE_ID="${IMAGE_ID#sha256:}"

# RepoDigests: a local-only image that has not
# been pushed to any registry returns an empty
# list. The verifier MUST still receive a
# structured "repo_digest_or_none": "none"
# value, never a blank.
REPO_DIGEST="none"
RAW_DIGEST=$(docker inspect --format='{{index .RepoDigests 0}}' "$IMAGE_REF" 2>/dev/null || true)
if [[ -n "$RAW_DIGEST" && "$RAW_DIGEST" == *"@sha256:"* ]]; then
  REPO_DIGEST="${RAW_DIGEST##*@}"
elif [[ -n "$RAW_DIGEST" ]]; then
  REPO_DIGEST="$RAW_DIGEST"
fi
# RepoDigests is a list of strings of the
# form `<name>@sha256:<digest>`. The Id is
# always present; the digest is not. We
# treat the digest as a registry-pinning
# luxury: a "none" verdict is fine for
# local-only images as long as the Id is
# non-empty. ANY image that lacks both is
# invalid.
if [[ -z "$IMAGE_ID" ]]; then
  EXIT_CLASS="build_failed_no_image_id"
  echo "cni-listener:build failed: docker inspect returned empty Id for $IMAGE_REF" >&2
  cat "$INSPECT_JSON" >&2 || true
  exit 11
fi

# The "primary" identity of the image is
# IMAGE_ID. The RepoDigest, when present,
# is redundant. Either is acceptable as
# long as one is non-empty.
if [[ "$REPO_DIGEST" == "none" ]]; then
  # No registry digest; fall back to docker
  # images output for sha256 of the image,
  # which is the layer config digest.
  SHA_FALLBACK=$(docker images --no-trunc --quiet "$IMAGE_REF" | head -1 || true)
  SHA_FALLBACK="${SHA_FALLBACK#sha256:}"
  if [[ -n "$SHA_FALLBACK" ]]; then
    DIGEST="$SHA_FALLBACK"
  fi
else
  DIGEST="${REPO_DIGEST#sha256:}"
fi

if [[ -z "$DIGEST" ]]; then
  # Even without RepoDigests we synthesise a
  # digest label that is the IMAGE_ID prefixed
  # "id-". This is NOT a registry digest; it
  # is a local image-id. The verifier should
  # treat the IMAGE_ID field (which IS the
  # canonical identity) as authoritative and
  # the digest field as informational only.
  DIGEST="id-${IMAGE_ID}"
fi

# Write the JSON artifact. The format is
# stable across runs to keep downstream
# parsers small.
BUILD_SHA="$(git rev-parse HEAD 2>/dev/null || echo not-in-git)"
BUILD_TS="$(python3 -c 'import datetime;print(datetime.datetime.utcnow().strftime("%Y-%m-%dT%H:%M:%SZ"))')"
DOCKER_VERSION="$(docker version --format='{{.Server.Version}}' 2>/dev/null || echo unknown)"

python3 - "$IMAGE_REF" "$IMAGE_ID" "$REPO_DIGEST" "$DIGEST" \
  "$BUILD_SHA" "$BUILD_TS" "$DOCKER_VERSION" "$EXIT_CLASS" \
  "$BUILD_LOG" "$INSPECT_JSON" "$ARTIFACT_JSON" <<'PY'
import json,sys
keys=["image_ref","image_id","repo_digest_or_none","repo_digest_or_id","build_sha",
      "build_timestamp_utc","docker_version","exit_classification",
      "build_log_path","inspect_log_path"]
vals=sys.argv[1:-1]
out=sys.argv[-1]
obj=dict(zip(keys,vals))
obj["structured_record_layout_version"]="d2b.26"
with open(out,"w") as fh:
    json.dump(obj,fh,indent=2,sort_keys=True)
    fh.write("\n")
PY

# Confirm the artifact was actually written.
if [[ ! -s "$ARTIFACT_JSON" ]]; then
  EXIT_CLASS="build_failed_no_artifact"
  echo "cni-listener:build failed: $ARTIFACT_JSON was empty after write" >&2
  exit 12
fi

echo
echo "== result ==" >&2
echo "image:               $IMAGE_REF" >&2
echo "image_id:            $IMAGE_ID" >&2
echo "repo_digest_or_none: $REPO_DIGEST" >&2
echo "build_sha:           $BUILD_SHA" >&2
echo "build_timestamp_utc: $BUILD_TS" >&2
echo "artifact:            $ARTIFACT_JSON" >&2

# Emit a one-line JSON signal on stdout so the
# install-nexus-test.sh and any CI shim read
# a structured value, not a brittle string
# parse of "sha256:<hex>".
python3 - "$IMAGE_REF" "$IMAGE_ID" "$REPO_DIGEST" "$DIGEST" \
  "$BUILD_SHA" "$BUILD_TS" "$DOCKER_VERSION" "$EXIT_CLASS" <<'PY'
import json,sys
keys=["image_ref","image_id","repo_digest_or_none","repo_digest_or_id","build_sha",
      "build_timestamp_utc","docker_version","exit_classification"]
vals=sys.argv[1:]
print(json.dumps(dict(zip(keys,vals))))
PY
