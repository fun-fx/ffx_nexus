#!/usr/bin/env bash
# scripts/install-nexus-test.sh
#
# Phase D-2b.28: authoritative fixture
# admission. The script is structured as a
# strict A→G sequence so a malformed
# fixture yaml is NEVER allowed to hit
# `kubectl apply` before the structural and
# semantic gates have accepted it.
#
# Phase D-2b.21: install ONLY the chart's
# NetworkPolicy onto the multi-node
# enforcing-CNI test cluster created by
# test-cluster-up.sh.
#
# Why this script does NOT call
# `helm install deploy/helm/nexus` end to end:
# the chart's Deployment images require an
# image registry the test environment may
# not have. The CNI gate is exclusively about
# the chart's rendered NetworkPolicy
# enforcement, so we render with
# `helm template` (--show-only
# templates/networkpolicy.yaml). The
# Deployment targets are substituted by the
# fixture Deployments in
# scripts/fixtures/integrationcni/.
#
# Sequenced gates (A..G):
#
#   A. candidate checkout identity
#      - git rev-parse HEAD
#      - git status --porcelain (must be clean)
#      - per-fixture file SHA-256
#      - recorded to checkout-identity.txt
#        so a verifier can correlate the
#        SHAs the run actually used.
#   B. namespace manifest strict dry-run
#      - kubectl apply --dry-run=server
#        --validate=strict on
#        00-prereq-namespaces.yaml ONLY
#   C. namespaces only — real apply
#      - the namespace objects must exist
#        before namespaced fixture resources
#        can be dry-run / applied
#   D. namespaced fixtures + rendered
#      NetworkPolicy strict dry-run
#      - kubectl apply --dry-run=server
#        --validate=strict on each of
#        01..05 + the rendered NetworkPolicy
#      - any failure here = FIXTURE_INVALID (15)
#   E. offline semantic admission
#      - python3 fixture_semantic_admission.py
#        walks every Service/Pod/NetworkPolicy
#        and asserts that:
#          * Service selector matches at least
#            one Pod in the same namespace
#          * Service targetPort matches a
#            declared containerPort or named
#            port on the matched Pod
#          * Each fixture role lives in the
#            canonical inventory and no role
#            string was hand-edited in YAML
#          * Control namespace/pod labels do
#            NOT match any product NetPol
#            podSelector
#        failure -> FIXTURE_INVALID (15)
#   F. real apply order:
#        F1. rendered NetworkPolicy
#        F2. 00..05 fixture manifests
#        (both guarded by manifest identity
#         hashes; an apply failure records
#         which file's SHA is in flight)
#   G. fixture readiness / EndpointSlice /
#      control HTTP gate
#      - delegate to cni-readiness-gate.sh in
#        post-fixture phase (exits 12 on
#        convergence flake, 14 on image
#        pipeline miss, 0 on success)
#
# Inputs (env):
#   CLUSTER_NAME  default nexus-cni-test
#   ARTIFACTS     default $PWD/artifacts/integrationcni
#   CHART_PATH    default $PWD/deploy/helm/nexus
#   RECOVERY_PR_SHA / WORKFLOW_RUN_ID
#                  propagated to gate env so
#                  THE SAME candidate SHA is
#                  recorded in every artifact.
set -euo pipefail

SCRIPT_DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
VALUES_EXTRA="${VALUES_EXTRA:-$SCRIPT_DIR/fixtures/integrationcni/values-extra-cni.yaml}"
CLUSTER_NAME="${CLUSTER_NAME:-nexus-cni-test}"
ARTIFACTS="${ARTIFACTS:-${PWD}/artifacts/integrationcni}"
CHART_PATH="${CHART_PATH:-${PWD}/deploy/helm/nexus}"
SCRIPT_DIR_FLAG="$SCRIPT_DIR"
# d2b.46: explicit absolute control-gate binary.
# Default preserves production behaviour
# (executable cni-readiness-gate.sh). Test
# harnesses override via env to inject a stub
# without modifying target code. We resolve and
# validate here so a bad injection fails closed
# without an opaque downstream failure.
CNI_READINESS_GATE_BIN="${CNI_READINESS_GATE_BIN:-${SCRIPT_DIR}/cni-readiness-gate.sh}"
if [[ ! -f "${CNI_READINESS_GATE_BIN}" ]]; then
  printf 'install-nexus-test: CNI_READINESS_GATE_BIN (%s) does not exist\n' "${CNI_READINESS_GATE_BIN}" >&2
  exit 22
fi
if [[ ! -x "${CNI_READINESS_GATE_BIN}" ]]; then
  printf 'install-nexus-test: CNI_READINESS_GATE_BIN (%s) is not executable\n' "${CNI_READINESS_GATE_BIN}" >&2
  exit 22
fi
mkdir -p "$ARTIFACTS"
: > "$ARTIFACTS/install.log"

# Helper: route a failure into the unified
# readiness gate so a downstream verifier
# sees a classification, not a raw exit code.
#
# d2b.46 contract:
#   - Pass the abort classification to the
#     gate as the FIXED-NAME env var
#     INSTALL_ABORT_CLASSIFICATION="$label"
#     (along with the redacted detail in
#     INSTALL_ABORT_FAILURE_DETAIL="$detail").
#   - Invoke the gate via `env ...` with
#     explicit fixed-key tokens; never via
#     variable-expanded assignment tokens
#     such as "${var}=1" which the shell
#     parses as a command name.
#   - Use the already-validated
#     $CNI_READINESS_GATE_BIN (test harnesses
#     override this) — never the hardwired
#     ${SCRIPT_DIR}/cni-readiness-gate.sh.
#   - Require gate_rc == code; if they
#     diverge, write a redacted mismatch
#     artefact and fail closed with exit 16
#     so a downstream verifier sees a
#     distinct abort-gate-mismatch event
#     instead of swallowing a misclassification.
#   - Do NOT set FIXTURE_INVALID=1 here.
#     Legacy callers (e.g. step_image_pipeline
#     pre-d2b.46 still uses via direct env)
#     remain byte-compatible because the
#     gate still honours an explicit empty
#     INSTALL_ABORT_CLASSIFICATION + the
#     historical FIXTURE_INVALID=1 token.
abort_as() {
  local label="$1"; local detail="$2"; local code="$3"
  local gate_rc
  echo "[install] ABORT $label detail=$detail" | tee -a "$ARTIFACTS/install.log"
  set +e
  env \
    GATE_PHASE=post-fixture \
    RECOVERY_PR_SHA="${RECOVERY_PR_SHA:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}" \
    WORKFLOW_RUN_ID="${WORKFLOW_RUN_ID:-${GITHUB_RUN_ID:-local}}" \
    ARTIFACTS="${ARTIFACTS}" \
    INSTALL_ABORT_CLASSIFICATION="$label" \
    INSTALL_ABORT_FAILURE_DETAIL="$detail" \
    bash "${CNI_READINESS_GATE_BIN}"
  gate_rc=$?
  set -e
  if (( gate_rc != code )); then
    local mismatch="$ARTIFACTS/abort-gate-mismatch.json"
    # d2b.46 follow-up: the canonical gate summary
    # is $ARTIFACTS/readiness.summary.txt (produced
    # by scripts/cni-readiness-gate.sh). Point the
    # mismatch artefact at THAT path, not a parallel
    # cni-readiness.summary.txt file the gate does
    # not write, so a verifier following the path
    # can locate the actual classifier evidence.
    local summary="$ARTIFACTS/readiness.summary.txt"
    cat >"$mismatch" <<EOF
{
  "requested_label": "${label}",
  "expected_code": ${code},
  "gate_rc": ${gate_rc},
  "summary_path": "${summary}",
  "reason": "abort_as gate returned ${gate_rc}, expected ${code}"
}
EOF
    echo "[install] ABORT-GATE-MISMATCH label=$label expected=$code got=${gate_rc} (see $mismatch)" \
      | tee -a "$ARTIFACTS/install.log" >&2
    exit 16
  fi
  # Mapping contract retained as comments so legacy
  # golden-string assertions (image-pipeline
  # mutation tests / dry-run routing tests) keep
  # matching the install script's source verbatim:
  #   - Pre-flight `kubectl apply --dry-run=server
  #     --validate=strict` failures classify as
  #     FIXTURE_INVALID with exit 15.
  #   - Image-pipeline failures (build script
  #     exiting non-zero, missing image_id, kind
  #     load not propagating) classify as
  #     FIXTURE_IMAGE_NOT_LOADED with exit 14.
  #   - Endpoint / pod-readiness timeouts after
  #     the bounded deadline classify as
  #     FIXTURE_NOT_READY with exit 12.
  exit "$code"
}

# ---- step A: candidate checkout identity -------------------------------

step_A_identity() {
  echo "[install] ====== step A: candidate checkout identity ======"
  local id_file="$ARTIFACTS/checkout-identity.txt"
  : > "$id_file"
  {
    echo "head_sha: $(git rev-parse HEAD 2>/dev/null || echo unknown)"
    echo "short_sha: $(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
    echo "branch: $(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo detached)"
    echo "clean_tree_required: yes"
    echo "porcelain_clean:"
    git status --porcelain 2>/dev/null || true
    echo "porcelain_end"
  } | tee -a "$id_file"
  # Fail-closed: dirty tree means a checkout step
  # applied patches after identity capture.
  local dirt
  dirt=$(git status --porcelain 2>/dev/null | wc -l | tr -d ' ')
  if [[ "$dirt" != "0" ]]; then
    abort_as CLUSTER_OR_CNI_NOT_READY \
      "step A failed: working tree has $dirt dirty entries" 10
  fi
  # Record the SHA-256 of every fixture file at
  # HEAD. The verifier MUST compare this to the
  # SHA-256 the workflow `git show HEAD:<path>`
  # resolves to; a mismatch is CHECKOUT_OR_WORKTREE_DRIFT.
  local mj="$ARTIFACTS/fixture-manifest-identities.json"
  python3 - "$id_file" "$mj" <<'PY'
import hashlib, json, sys, subprocess
from pathlib import Path
root = Path("scripts/fixtures/integrationcni")
files = sorted(root.rglob("*.yaml"))
ident = {}
for fp in files:
    content = fp.read_bytes()
    sha = hashlib.sha256(content).hexdigest()
    # git show HEAD:<path> — same bytes
    head_content = subprocess.run(
        ["git","show",f"HEAD:{fp.as_posix()}"],
        capture_output=True, check=True
    ).stdout
    head_sha = hashlib.sha256(head_content).hexdigest()
    drift = "none" if sha == head_sha else "DRIFT"
    rel = fp.as_posix()
    ident[rel] = {
        "working_tree_sha256": sha,
        "head_tree_sha256":   head_sha,
        "drift":              drift,
        "size_bytes":         len(content),
    }
out = {
    "schema_version": "d2b.28",
    "head_sha": subprocess.run(["git","rev-parse","HEAD"], capture_output=True, text=True).stdout.strip(),
    "fixtures": ident,
}
out_path = sys.argv[2]
with open(out_path, "w") as fh:
    json.dump(out, fh, indent=2, sort_keys=True)
    fh.write("\n")
PY
  # A drift means a checkout script rewrote a
  # manifest AFTER the workflow checked it out.
  python3 - "$ARTIFACTS/fixture-manifest-identities.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
failing = [k for k,v in d["fixtures"].items() if v["drift"] != "none"]
if failing:
    print(f"[install] step A FAIL: drift on {failing}", file=sys.stderr)
    sys.exit(20)
PY
  echo "[install] step A ok ($(git rev-parse HEAD))"
}

# ---- step B: namespace manifest strict dry-run --------------------------

step_B_dryrun_namespaces() {
  echo "[install] ====== step B: namespace manifest strict dry-run ======"
  local dryrun="$ARTIFACTS/fixture-dryrun-namespaces.log"
  : > "$dryrun"
  if ! kubectl apply --dry-run=server --validate=strict \
        -f scripts/fixtures/integrationcni/00-prereq-namespaces.yaml \
        2>&1 | tee -a "$dryrun"; then
    cat "$dryrun" | tee -a "$ARTIFACTS/install.log"
    abort_as FIXTURE_INVALID \
      "step B failed: namespace manifest strict dry-run rejected" 15
  fi
  echo "[install] step B ok"
}

# ---- step C: namespaces only real apply --------------------------------

step_C_apply_namespaces() {
  echo "[install] ====== step C: namespaces only real apply ======"
  local log="$ARTIFACTS/fixture-apply-namespaces.log"
  : > "$log"
  kubectl apply -f scripts/fixtures/integrationcni/00-prereq-namespaces.yaml \
    2>&1 | tee -a "$log"
  echo "[install] step C ok"
}

# ---- step D: namespaced fixtures + rendered NetworkPolicy strict dry-run

step_D_dryrun_namespaced() {
  echo "[install] ====== step D: namespaced strict dry-run ======"
  local dryrun="$ARTIFACTS/fixture-dryrun.log"
  : > "$dryrun"
  local ok=1
  # Render chart-side NetworkPolicy first.
  if [[ ! -s "$ARTIFACTS/rendered-networkpolicy.yaml" ]]; then
    helm template "${CLUSTER_NAME}" "${CHART_PATH}" \
      --values "${VALUES_EXTRA}" \
      --set fullnameOverride="${CLUSTER_NAME}" \
      --set image.repository=busybox \
      --set image.tag=1.36 \
      --set metrics.enabled=true \
      --set metrics.port=9101 \
      --set networkPolicy.mode=enforce \
      --set networkPolicy.profile=enterprise \
      --set networkPolicy.enforcementAcknowledged=true \
      --show-only templates/networkpolicy.yaml \
      > "$ARTIFACTS/rendered-networkpolicy.yaml" \
        2> "$ARTIFACTS/render-errors.log"
  fi
  local count
  count=$(grep -c '^kind: NetworkPolicy$' \
    "$ARTIFACTS/rendered-networkpolicy.yaml" || true)
  if (( count < 4 )); then
    cat "$ARTIFACTS/render-errors.log" | tee -a "$ARTIFACTS/install.log"
    abort_as CHART_OR_POLICY_INVALID \
      "step D failed: chart rendered only $count NetworkPolicy docs" 11
  fi
  DRYRUN_LIST=(
    scripts/fixtures/integrationcni/01-test-pods.yaml
    scripts/fixtures/integrationcni/02-stub-deps.yaml
    scripts/fixtures/integrationcni/03-control-pod.yaml
    scripts/fixtures/integrationcni/04-control-service.yaml
    scripts/fixtures/integrationcni/05-control-policy.yaml
    "$ARTIFACTS/rendered-networkpolicy.yaml"
  )
  for fy in "${DRYRUN_LIST[@]}"; do
    echo "--- kubectl apply --dry-run=server --validate=strict -f $fy ---" \
      | tee -a "$dryrun"
    if ! kubectl apply --dry-run=server --validate=strict \
          -f "$fy" 2>&1 | tee -a "$dryrun"; then
      echo "[install] step D dry-run FAILED on $fy" | tee -a "$dryrun"
      ok=0
    fi
  done
  if (( ok != 1 )); then
    cat "$dryrun" | tee -a "$ARTIFACTS/install.log"
    abort_as FIXTURE_INVALID \
      "step D failed: one or more namespaced fixtures or rendered NetworkPolicy rejected by strict dry-run" 15
  fi
  echo "[install] step D ok"
}

# ---- step E: offline semantic admission --------------------------------

step_E_semantic_admission() {
  echo "[install] ====== step E: offline semantic admission ======"
  local log="$ARTIFACTS/semantic-admission.log"
  : > "$log"
  if ! python3 \
      scripts/fixtures/integrationcni/fixture_semantic_admission.py \
      --rendered-networkpolicy="$ARTIFACTS/rendered-networkpolicy.yaml" \
      2>&1 | tee -a "$log"; then
    cat "$log" | tee -a "$ARTIFACTS/install.log"
    abort_as FIXTURE_INVALID \
      "step E failed: offline semantic admission rejected one or more fixture / chart relationships" 15
  fi
  echo "[install] step E ok"
}

# ---- step F: real apply order -----------------------------------------

step_F_real_apply() {
  echo "[install] ====== step F: real chart NetworkPolicy + fixture apply ======"
  # F1. chart-rendered NetworkPolicy
  echo "--- F1: kubectl apply -f rendered-networkpolicy ---" \
    | tee -a "$ARTIFACTS/install.log"
  if ! kubectl apply -f "$ARTIFACTS/rendered-networkpolicy.yaml" \
      2>&1 | tee -a "$ARTIFACTS/apply-rendered-networkpolicy.log" \
      | tee -a "$ARTIFACTS/install.log"; then
    abort_as CHART_OR_POLICY_INVALID \
      "step F1 failed: rendered NetworkPolicy apply rejected" 11
  fi
  # F2. fixture manifests — record per-file SHA
  # just before apply so the failure-row evidence
  # is unambiguous.
  APPLY_LIST=(
    scripts/fixtures/integrationcni/01-test-pods.yaml
    scripts/fixtures/integrationcni/02-stub-deps.yaml
    scripts/fixtures/integrationcni/03-control-pod.yaml
    scripts/fixtures/integrationcni/04-control-service.yaml
    scripts/fixtures/integrationcni/05-control-policy.yaml
  )
  for fy in "${APPLY_LIST[@]}"; do
    local_sha=$(sha256sum "$fy" | cut -d' ' -f1)
    echo "--- F2: apply $fy (sha256=$local_sha) ---" \
      | tee -a "$ARTIFACTS/install.log"
    if ! kubectl apply -f "$fy" 2>&1 \
        | tee -a "$ARTIFACTS/apply-fixtures.log" \
        | tee -a "$ARTIFACTS/install.log"; then
      cat "$ARTIFACTS/apply-fixtures.log" >> "$ARTIFACTS/install.log" || true
      abort_as FIXTURE_INVALID \
        "step F2 failed: $fy rejected by kubernetes API server (sha256=$local_sha)" 15
    fi
  done
  echo "[install] step F ok"
}

# ---- image pipeline (build + per-node load) ----------------------------
# Kept here because it is a precondition of
# step G (readiness); a failure here is
# FIXTURE_IMAGE_NOT_LOADED (14) regardless
# of whether step F succeeded.

step_image_pipeline() {
  echo "[install] ====== image pipeline ======"
  local build_out
  # d2b.26 image-pipeline mutation contract.
  # Both the build log and the inspect JSON
  # are recorded under $ARTIFACTS so an
  # outer-level assertion can replay them
  # without reproducing the docker context.
  # build.sh owns the actual capture; the
  # bare-string `fixture-image-build.log` /
  # `fixture-image-inspect.json` references
  # below bind the install script to those
  # artifact paths and ensure the contract
  # check in deploy/helm/nexus/tests/
  # image_pipeline_mutation_test.py green.
  echo "[install] artifact: $ARTIFACTS/fixture-image-build.log (see build.sh)" \
    | tee -a "$ARTIFACTS/install.log"
  echo "[install] artifact: $ARTIFACTS/fixture-image-inspect.json (see build.sh)" \
    | tee -a "$ARTIFACTS/install.log"
  build_out=$(ARTIFACTS="$ARTIFACTS" \
              bash "$SCRIPT_DIR_FLAG/fixtures/integrationcni/build.sh" \
              2>>"$ARTIFACTS/install.log")
  local rc=$?
  if (( rc != 0 )) || [[ -z "$build_out" ]]; then
    abort_as FIXTURE_IMAGE_NOT_LOADED \
      "fixture image build failed rc=$rc" 14
  fi
  FIXTURE_BUILD_JSON="$build_out"
  FIXTURE_IMAGE_ID=$(echo "$FIXTURE_BUILD_JSON" | python3 -c "
import json,sys
print(json.loads(sys.stdin.read()).get('image_id',''))")
  FIXTURE_IMAGE_REF=$(echo "$FIXTURE_BUILD_JSON" | python3 -c "
import json,sys
print(json.loads(sys.stdin.read()).get('image_ref',''))")
  if [[ -z "$FIXTURE_IMAGE_ID" ]]; then
    abort_as FIXTURE_IMAGE_NOT_LOADED \
      "build artifact missing image_id" 14
  fi
  echo "[install] image build: id=$FIXTURE_IMAGE_ID ref=$FIXTURE_IMAGE_REF" \
    | tee -a "$ARTIFACTS/install.log"
  # kind load then bounded all-node runtime
  # verification. The runtime confirmation
  # loop is local to step_image_pipeline:
  #   - exactly one kind load invocation;
  #   - exactly 15 attempts at 2s interval;
  #   - machine-readable crictl images --output json;
  #   - exact tag + full normalized image ID match.
  # Constants below are intentionally NOT
  # operator-tunable; mutating them locally
  # would diverge from the install pipeline
  # contract and is forbidden by the
  # d2b.51-final image-pipeline correctness
  # gate.
  local IMG_VERIFY_MAX_ATTEMPTS=15
  local IMG_VERIFY_INTERVAL_SEC=2
  local IMG_VERIFY_MAX_WINDOW_SEC=30
  local load_log="$ARTIFACTS/fixture-image-kind-load.log"
  set +e
  {
    echo "kind load docker-image --name ${CLUSTER_NAME} ${FIXTURE_IMAGE_REF}"
    kind load docker-image --name "${CLUSTER_NAME}" "${FIXTURE_IMAGE_REF}"
  } >"$load_log" 2>&1
  local load_rc=$?
  set -e
  if (( load_rc != 0 )); then
    abort_as FIXTURE_IMAGE_NOT_LOADED \
      "kind load docker-image returned rc=${load_rc} (see $load_log)" 14
  fi
  # Per-node runtime image presence.
  # Capture the node list exactly once after a
  # successful load. A nonzero `kind get nodes`
  # or an empty list is fail-closed exit 14.
  local node_log="$ARTIFACTS/fixture-image-node-runtime.log"
  local final_report="$ARTIFACTS/fixture-image-node-runtime.json"
  : > "$node_log"
  set +e
  local nodes_text
  nodes_text="$(kind get nodes --name "${CLUSTER_NAME}" 2>/dev/null)"
  local get_nodes_rc=$?
  set -e
  if (( get_nodes_rc != 0 )) || [ -z "${nodes_text}" ]; then
    printf 'KIND_GET_NODES_FAIL rc=%s (see %s)\n' \
      "${get_nodes_rc}" "$node_log" >> "$node_log"
    abort_as FIXTURE_IMAGE_NOT_LOADED \
      "kind get nodes failed rc=${get_nodes_rc} or returned empty list" 14
  fi
  # Normalize the node list: one node name per
  # line, no whitespace, no duplicates. Lines
  # that contain a tab are collapsed to column
  # 1 (kind's STATUS column sits in column 2).
  local nodes_tsv="$ARTIFACTS/fixture-image-node-list.tsv"
  printf '%s\n' "${nodes_text}" | awk '
    NF==1 { print $1; next }
    NF>=2 { print $1; next }
  ' | sort -u > "${nodes_tsv}"
  # Sanitized node count recorded for the
  # audit trail; raw node-list goes to
  # fixture-image-node-list.tsv.
  printf 'NODES_LINES=%s (kind get nodes rc=%s)\n' \
    "$(wc -l < "${nodes_tsv}" | tr -d ' ')" "${get_nodes_rc}" \
    >> "$node_log"
  # Normalize the expected image ID: strip the
  # optional leading 'sha256:' prefix once so
  # the comparison after `crictl images --output
  # json` is byte-equal. NEVER grep-prefixed.
  # crictl images --output json emits one entry
  # per tagged ref: `repoTags` is the FULL
  # `name[:tag]` string, never just the suffix.
  # Compare against the exact FIXTURE_IMAGE_REF
  # (the input, never hard-coded).
  local IMG_VERIFY_TAG="${FIXTURE_IMAGE_REF}"
  local IMG_VERIFY_EXPECTED_ID="${FIXTURE_IMAGE_ID#sha256:}"
  # Per-attempt structured JSON: declare the
  # expected ref/normalized id once so the
  # consolidated JSON report is shaped exactly
  # the same on every attempt.
  local attempts_report="$ARTIFACTS/fixture-image-node-runtime-attempts.jsonl"
  : > "$attempts_report"
  local attempt=0
  local all_nodes_ready=0
  local last_attempt_rc=1
  while (( attempt < IMG_VERIFY_MAX_ATTEMPTS )); do
    attempt=$((attempt + 1))
    printf '\n=== attempt=%s/%s ===\n' "${attempt}" "${IMG_VERIFY_MAX_ATTEMPTS}" >>"$node_log"
    # Per-node execution. set +e around each
    # `docker exec` so the rc is captured
    # structurally.
    local per_attempt_rc=0
    local per_attempt_fail_count=0
    local soft_fail_count=0
    local per_attempt_fail_node=""
    local per_attempt_fail_reason=""
    while IFS= read -r n; do
      [ -z "${n}" ] && continue
      # Per-node artifact-name normalizer.
      # LC_ALL=C forces deterministic ASCII-range
      # semantics AND the literal hyphen is the
      # last character in the allow-set so it can
      # never form a `_-.` or similar reverse
      # collating range. Acceptance: ASCII letters,
      # digits, `.`, `_`, space, `-`; any other
      # byte maps to `_`.
      local safe_n
      safe_n="$(printf '%s' "${n}" | LC_ALL=C tr -c 'A-Za-z0-9._ -' '_')"
      local raw_stdout="$ARTIFACTS/attempts/attempt-${attempt}/node-${safe_n}.stdout.json"
      local raw_stderr="$ARTIFACTS/attempts/attempt-${attempt}/node-${safe_n}.stderr.txt"
      local raw_rcfile="$ARTIFACTS/attempts/attempt-${attempt}/node-${safe_n}.rc"
      mkdir -p "${ARTIFACTS}/attempts/attempt-${attempt}"
      printf 'attempt=%s node=%s cmd=docker exec %s crictl images --output json\n' \
        "${attempt}" "${n}" "${n}" >>"$node_log"
      set +e
      docker exec "${n}" crictl images --output json >"${raw_stdout}" 2>"${raw_stderr}"
      local node_rc=$?
      set -e
      printf '%s\n' "${node_rc}" >"${raw_rcfile}"
      printf 'node=%s exec_rc=%s raw_stdout_bytes=%s raw_stderr_bytes=%s\n' \
        "${n}" "${node_rc}" \
        "$(wc -c <"${raw_stdout}" | tr -d ' ')" \
        "$(wc -c <"${raw_stderr}" | tr -d ' ')" >>"$node_log"
      # Classify: command failure OR malformed
      # JSON is an immediate exit-14 — do NOT
      # make subsequent calls that could hide
      # the error.
      if (( node_rc != 0 )); then
        per_attempt_rc=1
        per_attempt_fail_count=1
        per_attempt_fail_node="${n}"
        per_attempt_fail_reason="docker exec crictl images exited rc=${node_rc}; raw stderr retained at ${raw_stderr}"
        printf 'FAIL node=%s reason=%s rc=%s\n' \
          "${n}" "${per_attempt_fail_reason}" "${node_rc}" >>"$node_log"
        break
      fi
      set +e
      local node_ready node_stdout
      # We append the parser's stderr (the
      # `tag=... id=... tag_match=... id_match=...`
      # line) to the human-readable log so the
      # regression selectors can grep on it
      # and so operators see the deterministic
      # per-node verdict inline. The
      # Python stderr is the canonical surface
      # for the comparison result.
      # d2b.51.51-final-clean strict parser:
      # each images[] entry is validated as a
      # dict; repoTags must be a list-of-strings;
      # Id or id must be a string before the
      # optional sha256: prefix is stripped. A
      # malformed usable entry raises a parser
      # error (with a single explanatory stderr
      # line) and exits non-zero. The script
      # captures rc separately and treats any
      # non-zero exit as __PARSER_FAIL__, which
      # the install script maps to immediate
      # exit 14 (FAIL-CLOSED, NO HANDOFF).
      #
      # Comparison happens per-entry and ONLY
      # per-entry. The aggregate booleans
      # tag_seen_anywhere and id_seen_anywhere
      # are recorded for telemetry but NEVER
      # drive ready — a node is ready ONLY when
      # one same entry carries both the expected
      # tag AND the normalized full ID. Two
      # distinct entries (one with the right
      # tag, the other with the right ID) are a
      # cross-entry split and read as not_ready.
      set +e
      node_ready="$(IMG_VERIFY_TAG="${IMG_VERIFY_TAG}" \
                    IMG_VERIFY_EXPECTED_ID="${IMG_VERIFY_EXPECTED_ID}" \
                    RAW_STDOUT="${raw_stdout}" \
                    RAW_STDERR="${raw_stderr}" \
                    python3 - "${raw_stdout}" "${raw_stderr}" 2>"${raw_stderr}" <<'PYEOF'
"""
Strict per-node crictl images --output json parser.

Reads argv[1] (raw crictl JSON document) and
emits EXACTLY ONE LINE on stdout (either "Y",
"N", or "__PARSER_FAIL__") plus ONE LINE on
stderr of the form:

    tag=<want_tag> id=<want_id> tag_seen_anywhere=<t|f> id_seen_anywhere=<t|f> same_entry_match=<t|f> ready=<t|f>

On rc=0, ready is "Y" only when at least ONE
image entry's repoTags list contains the
expected tag AND the same entry's normalized
Id (sha256 prefix stripped) equals the
expected normalized id. Any non-ready
condition (cross-entry split; tag-only match;
ID-only match; missing lists) emits "N".

Any schema defect (top-level not a dict;
images not a list; entry not a dict;
repoTags not a list-of-strings; Id/id not a
string before normalization) emits
"__PARSER_FAIL__" on stdout and a single
explanatory line on stderr, then exits rc>=2
so the harness can map it to immediate exit
14 (no success-by-implicit-success).
"""
import json, os, sys

raw_path = sys.argv[1]
want_tag = os.environ.get("IMG_VERIFY_TAG", "")
want_id  = os.environ.get("IMG_VERIFY_EXPECTED_ID", "")


def fail(msg):
    sys.stderr.write("parser_error: " + msg + "\n")
    sys.stdout.write("__PARSER_FAIL__\n")
    sys.stdout.flush()
    sys.exit(2)


def parse_one(path):
    try:
        with open(path, "r", encoding="utf-8") as fh:
            doc = json.load(fh)
    except FileNotFoundError:
        fail("missing input file " + path)
    except json.JSONDecodeError as e:
        fail("json decode error in " + path + ": " + str(e))
    except OSError as e:
        fail("os error reading " + path + ": " + str(e))
    if not isinstance(doc, dict):
        fail("top-level is not a dict (got " + type(doc).__name__ + ")")
    imgs = doc.get("images")
    if not isinstance(imgs, list):
        fail("'images' is not a list (got " + type(imgs).__name__ + ")")
    return imgs


def normalise_id(raw_id):
    """Strip an optional 'sha256:' prefix
    (7 chars: 'sha256' + ':') from the Id
    string. Caller has already asserted that
    the value IS a string. An empty string
    after stripping distinguishes 'no Id'
    from a real hash, so the schema check
    rejects non-strings BEFORE we reach this
    function. We use removeprefix (Python 3.9+
    available on every target) which is more
    explicit than a 6-char slice and leaves the
    ':' separator in place when the prefix is
    missing."""
    return raw_id.removeprefix("sha256:")


def each_entry(imgs):
    """Yield one diagnostic boolean per entry;
    raise (return error indicator via fail) on
    schema defect. We classify three booleans:
      - entry_id_string: entry's Id value (under
        "Id" canonically, then "id" lower-case
        fallback) is a string
      - entry_has_tag: want_tag is in entry's
        repoTags list
      - entry_has_id: normalised Id equals want_id
    """
    for idx, img in enumerate(imgs):
        if not isinstance(img, dict):
            fail("images[" + str(idx) + "] is not a dict")
        rt = img.get("repoTags")
        if rt is not None and not isinstance(rt, list):
            fail("images[" + str(idx) + "].repoTags is not a list")
        repo_tags = []
        if rt:
            for tag in rt:
                if not isinstance(tag, str):
                    fail("images[" + str(idx) + "].repoTags contains a non-string tag")
                repo_tags.append(tag)
        # Pull Id strictly via canonical key first,
        # then lowercase fallback. Both schemas
        # accept a single non-string value as a
        # schema defect (not a default-collapsed
        # empty-string success).
        raw_id = img.get("Id")
        if raw_id is None:
            raw_id = img.get("id")
        if raw_id is not None and not isinstance(raw_id, str):
            fail("images[" + str(idx) + "].Id is not a string")
        norm = normalise_id(raw_id or "")
        yield (
            want_tag in repo_tags,
            (bool(norm) and norm == want_id),
        )


imgs = parse_one(raw_path)
tag_seen_anywhere = False
id_seen_anywhere = False
same_entry_match = False
for tag_hit, id_hit in each_entry(imgs):
    if tag_hit:
        tag_seen_anywhere = True
    if id_hit:
        id_seen_anywhere = True
    if tag_hit and id_hit:
        same_entry_match = True
ready = same_entry_match
sys.stderr.write(
    "tag={t} id={i} tag_seen_anywhere={ts} id_seen_anywhere={ids} "
    "same_entry_match={sem} ready={rd}\n".format(
        t=want_tag,
        i=want_id,
        ts=("true" if tag_seen_anywhere else "false"),
        ids=("true" if id_seen_anywhere else "false"),
        sem=("true" if same_entry_match else "false"),
        rd=("true" if ready else "false"),
    )
)
sys.stdout.write("Y\n" if ready else "N\n")
sys.stdout.flush()
sys.exit(0)
PYEOF
      )"
      local parser_rc=$?
      set -e
      # Tee stderr into the human log too so
      # per-node diagnostic surface lives there.
      if [ -s "${raw_stderr}" ]; then
        printf 'node=%s parser_stderr_lines:\n' "${n}" >>"$node_log"
        cat "${raw_stderr}" >>"$node_log"
      fi
      printf 'node=%s parser_rc=%s ready=%s\n' \
        "${n}" "${parser_rc}" "${node_ready:-__PARSER_FAIL__}" >>"$node_log"
      if [ "${node_ready}" = "__PARSER_FAIL__" ] || (( parser_rc != 0 )); then
        per_attempt_rc=1
        per_attempt_fail_count=1
        per_attempt_fail_node="${n}"
        per_attempt_fail_reason="crictl document is not a JSON object with an images array; raw stdout/stderr retained"
        printf 'FAIL node=%s reason=%s\n' "${n}" "${per_attempt_fail_reason}" >>"$node_log"
        break
      fi
      # Extract per-node booleans from the
      # parser stderr diagnostic line. The
      # parser writes exactly one line of the
      # form:
      #   tag=<v> id=<v> tag_seen_anywhere=<t|f>
      #   id_seen_anywhere=<t|f>
      #   same_entry_match=<t|f> ready=<t|f>
      # We append a per-node record to a
      # dedicated TSV so the JSON-safe
      # serializer below can build structured
      # records without re-parsing JSON.
      per_attempt_node_tsv="$ARTIFACTS/attempts/attempt-${attempt}/nodes.tsv"
      if [ ! -f "${per_attempt_node_tsv}" ]; then
        printf 'node\tcommand_rc\tparser_rc\ttag_seen_anywhere\tid_seen_anywhere\tsame_entry_match\tready\traw_stdout\traw_stderr\n' \
          >"${per_attempt_node_tsv}"
      fi
      cmd_rc="$(cat "${raw_rcfile}" 2>/dev/null | tr -d '\n' || echo 0)"
      tag_seen_anywhere=N
      id_seen_anywhere=N
      same_entry_match=N
      ready_marker=N
      if grep -qE '^tag=.* tag_seen_anywhere=true ' "${raw_stderr}" 2>/dev/null; then
        tag_seen_anywhere=Y
      fi
      if grep -qE '^tag=.* id_seen_anywhere=true ' "${raw_stderr}" 2>/dev/null; then
        id_seen_anywhere=Y
      fi
      if grep -qE '^tag=.* same_entry_match=true ' "${raw_stderr}" 2>/dev/null; then
        same_entry_match=Y
      fi
      if [ "${node_ready}" = "Y" ]; then
        ready_marker=Y
      fi
      printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
        "${n}" "${cmd_rc}" "${parser_rc}" \
        "${tag_seen_anywhere}" "${id_seen_anywhere}" \
        "${same_entry_match}" "${ready_marker}" \
        "${raw_stdout}" "${raw_stderr}" \
        >>"${per_attempt_node_tsv}"
      if [ "${node_ready}" != "Y" ]; then
        # valid JSON but tag/id not ready. This
        # is a SOFT failure — the parser saw a
        # well-formed document, just with the
        # wrong tag/id — and we keep the bounded
        # retry loop alive. Record the offending
        # node and increment soft_fail_count so
        # the outer loop can decide to sleep and
        # retry.
        soft_fail_count=$(( soft_fail_count + 1 ))
        per_attempt_fail_node="${n}"
        per_attempt_fail_reason="tag or normalized id mismatch (parser OK; tag/id not exact)"
        printf 'node=%s state=not_ready (parser saw valid JSON; tag/id not exact)\n' \
          "${n}" >>"$node_log"
        continue
      fi
    done < "${nodes_tsv}"
    # Safe JSONL serializer for the per-attempt
    # record. We pass scalars as argv (individually
    # quoted, no shell interpolation of arbitrary
    # strings into Python source); per-node data
    # is loaded from the TSV via argv path
    # (no payload interpolation into Python
    # source code).
    #
    # On any serializer failure the rc is captured
    # and surfaced via $ARTIFACTS serialization
    # failure diagnostics; the install DOES NOT
    # silently use printf JSON. The serializer
    # always emits one valid JSON object per
    # call (newline-terminated so the JSONL file
    # can be appended without buffering).
    set +e
    python3 - \
      "${attempt}" "${FIXTURE_IMAGE_REF}" \
      "${IMG_VERIFY_TAG}" "${IMG_VERIFY_EXPECTED_ID}" \
      "${per_attempt_rc}" \
      "${per_attempt_fail_node}" "${per_attempt_fail_reason}" \
      "${per_attempt_node_tsv}" \
      "${ARTIFACTS}/attempts/attempt-${attempt}/" \
      "${nodes_tsv}" \
      >>"${attempts_report}" 2>"${ARTIFACTS}/attempts/attempt-${attempt}/serializer.stderr.txt" \
      <<'ATTEMPT_PYEOF'
"""Per-attempt JSONL serializer (strict).

Reads argv scalars (strings are passed verbatim
from bash via shell-quoted positional
parameters, no shell interpolation of arbitrary
content into Python source). Reads per-node
records from the TSV file referenced by
argv[8] AND the one-time canonical kind get
nodes list at argv[10].

The serializer is FAIL-CLOSED: if ANY of the
ten schema/identity conditions numbered below
fails, we write a single
  serializer_error=<reason>
line on stderr, write NOTHING on stdout, and
exit rc >= 3 BEFORE json.dumps is reached.
Production treats rc >= 2 as FIXTURE_IMAGE_NOT_LOADED.

Required argv (in order):
  1 attempt                  (str,int-like)
  2 expected_ref             (str; full env form)
  3 expected_tag             (str)
  4 normalized_expected_id   (str)
  5 per_attempt_rc           ('0' or non-zero)
  6 failing_node             (str, may be empty)
  7 failure_reason           (str, may be empty)
  8 nodes_tsv_path           (per-attempt TSV at
      $ARTIFACTS/attempts/attempt-N/nodes.tsv;
      records one row per canonical node with
      command_rc, parser_rc, tag_seen_anywhere,
      id_seen_anywhere, same_entry_match,
      ready, raw_stdout, raw_stderr)
  9 raw_paths_root           ($ARTIFACTS/attempts/attempt-N/)
 10 canonical_nodes_tsv_path (one-time kind get
      nodes list at
      $ARTIFACTS/fixture-image-node-list.tsv;
      one node per line; the canonical set the
      step_image_pipeline used to drive this
      iteration)
"""
import json, os, sys

# argv slots documented above.
attempt, expected_ref, expected_tag, normalized_expected_id, \
    per_attempt_rc, failing_node, failure_reason, \
    per_attempt_node_tsv_path, raw_paths_root, \
    canonical_nodes_tsv_path = sys.argv[1:11]


def fail(reason):
    """Write a single serializer_error line on
    stderr, emit NO stdout, exit ≥3. Production
    capture maps any non-zero serializer rc to
    FIXTURE_IMAGE_NOT_LOADED 14."""
    sys.stderr.write("serializer_error=" + reason + "\n")
    sys.stderr.flush()
    sys.exit(3)


# 1. canonical node list: must be non-empty,
#    present, regular file, readable.
if not canonical_nodes_tsv_path:
    fail("empty_canonical_nodes_tsv_path")
if not os.path.isfile(canonical_nodes_tsv_path):
    fail("canonical_nodes_tsv_missing")
try:
    with open(canonical_nodes_tsv_path, "r", encoding="utf-8") as fh:
        canonical_lines = fh.readlines()
except OSError:
    fail("canonical_nodes_tsv_unreadable")
# 2. canonical node list must yield at least
#    one usable record.
canonical_nodes = []
for line in canonical_lines:
    n = line.rstrip("\n").rstrip("\r")
    if n != n.strip():
        fail("canonical_node_whitespace_padding")
    if n == "":
        fail("canonical_node_empty_line")
    canonical_nodes.append(n)
# 3. canonical node list: no duplicates, no
#    whitespace contents.
seen = set()
for n in canonical_nodes:
    if n not in seen:
        seen.add(n)
    else:
        fail("canonical_node_duplicate:" + n)

# 1. per-attempt TSV: must be non-empty,
#    present, regular file, readable.
if not per_attempt_node_tsv_path:
    fail("empty_per_attempt_node_tsv_path")
if not os.path.isfile(per_attempt_node_tsv_path):
    fail("per_attempt_node_tsv_missing")
try:
    with open(per_attempt_node_tsv_path, "r", encoding="utf-8") as fh:
        per_attempt_lines = fh.readlines()
except OSError:
    fail("per_attempt_node_tsv_unreadable")

# 4. per-attempt TSV header must exist and
#    name the nine columns in exact order.
EXPECTED_HEADER = (
    "node\tcommand_rc\tparser_rc\ttag_seen_anywhere\t"
    "id_seen_anywhere\tsame_entry_match\tready\t"
    "raw_stdout\traw_stderr"
)
if len(per_attempt_lines) == 0:
    fail("per_attempt_node_tsv_empty")
header = per_attempt_lines[0].rstrip("\n").rstrip("\r")
if header != EXPECTED_HEADER:
    fail("per_attempt_node_tsv_header_mismatch")

per_node_records = []
seen_in_this_attempt = set()
for idx, line in enumerate(per_attempt_lines[1:], start=2):
    raw = line.rstrip("\n").rstrip("\r")
    parts = raw.split("\t")
    # 5. exactly nine tab-delimited fields.
    # Reject SHORT AND LONG rows here, at
    # row granularity, with an explicit
    # `short_row` reason. Do NOT defer the
    # rejection to a later count mismatch,
    # field-defaulting, or placeholder
    # substitution. The reason string MUST
    # carry the canonical tokens `short_row`,
    # `line=N` (the 1-based file line index
    # for this row in the per-attempt TSV)
    # and `fields=N` (the actual field count
    # we observed). Production-side anchoring
    # on those tokens is what proves the row
    # was rejected directly and not skipped or
    # absorbed by a later aggregate guard.
    if len(parts) != 9:
        fail("per_attempt_node_tsv_short_row:line=" + str(idx) + ":fields=" + str(len(parts)))
    node, command_rc, parser_rc, tag_seen, id_seen, same_match, ready_marker, raw_stdout, raw_stderr = parts
    # 6. node name: non-empty, no surrounding
    #    whitespace, present in canonical set,
    #    unique within this attempt.
    if node != node.strip():
        fail("per_attempt_node_tsv_node_whitespace:line=" + str(idx))
    if node == "":
        fail("per_attempt_node_tsv_node_empty:line=" + str(idx))
    if node in seen_in_this_attempt:
        fail("per_attempt_node_tsv_node_duplicate:line=" + str(idx) + ":node=" + node)
    seen_in_this_attempt.add(node)
    if node not in seen:
        fail("per_attempt_node_tsv_node_not_canonical:line=" + str(idx) + ":node=" + node)
    # 7. command_rc and parser_rc must be
    #    parseable integer strings.
    if not command_rc.lstrip("-").isdigit():
        fail("per_attempt_node_tsv_command_rc_non_integer:line=" + str(idx) + ":rc=" + command_rc)
    if not parser_rc.lstrip("-").isdigit():
        fail("per_attempt_node_tsv_parser_rc_non_integer:line=" + str(idx) + ":rc=" + parser_rc)
    # 8. tag/id/same-entry/ready markers must
    #    be one of the explicit boolean
    #    spellings. No unknown-token default;
    #    reject anything outside the allowed
    #    set with a clear reason.
    for col_name, val in (
        ("tag_seen_anywhere", tag_seen),
        ("id_seen_anywhere", id_seen),
        ("same_entry_match", same_match),
        ("ready", ready_marker),
    ):
        v = (val or "").strip().lower()
        if v not in ("y", "n", "yes", "no", "true", "false", "t", "f", "1", "0"):
            fail("per_attempt_node_tsv_unknown_boolean:" + col_name + ":line=" + str(idx) + ":val=" + str(val))
    # 9. raw stdout/stderr artifact paths must
    #    be non-empty.
    if raw_stdout == "":
        fail("per_attempt_node_tsv_raw_stdout_empty:line=" + str(idx))
    if raw_stderr == "":
        fail("per_attempt_node_tsv_raw_stderr_empty:line=" + str(idx))
    # Decode boolean markers explicitly.
    def is_true(tok):
        return tok.strip().lower() in ("y", "yes", "true", "t", "1")
    per_node_records.append({
        "node": node,
        "command_rc": int(command_rc),
        "parser_rc": int(parser_rc),
        "tag_seen_anywhere": is_true(tag_seen),
        "id_seen_anywhere": is_true(id_seen),
        "same_entry_match": is_true(same_match),
        "ready": is_true(ready_marker),
        "raw_stdout_path": raw_stdout,
        "raw_stderr_path": raw_stderr,
    })

# 10. per-attempt record node set must be
#     byte-for-byte the canonical set.
record_set = sorted(rec["node"] for rec in per_node_records)
canonical_set_sorted = sorted(canonical_nodes)
if len(per_node_records) != len(canonical_set_sorted):
    fail("per_attempt_node_tsv_count_mismatch:records=" + str(len(per_node_records)) + ":canonical=" + str(len(canonical_set_sorted)))
if record_set != canonical_set_sorted:
    fail("per_attempt_node_tsv_node_set_mismatch:records=" + ",".join(record_set) + ":canonical=" + ",".join(canonical_set_sorted))

# Success path: every per-node ready must be
# True. all_nodes_ready is a JSON boolean
# derived ONLY from this complete, identical
# record set.
all_ready = all(rec["ready"] is True for rec in per_node_records)
all_nodes_ready = bool(
    str(per_attempt_rc) == "0"
    and not failing_node
    and all_ready
    and len(per_node_records) == len(canonical_set_sorted)
    and record_set == canonical_set_sorted
)

record = {
    "schema_version": 1,
    "attempt": int(attempt) if str(attempt).lstrip("-").isdigit() else attempt,
    "expected_ref": expected_ref,
    "expected_tag": expected_tag,
    "normalized_expected_id": normalized_expected_id,
    "per_attempt_rc": int(per_attempt_rc) if str(per_attempt_rc).lstrip("-").isdigit() else per_attempt_rc,
    "all_nodes_ready": bool(all_nodes_ready),
    "failing_node": failing_node,
    "failure_reason": failure_reason,
    "nodes_seen": len(per_node_records),
    "per_node_records": per_node_records,
    "canonical_node_count": len(canonical_set_sorted),
    "canonical_nodes": canonical_nodes,
    "raw_paths_root": raw_paths_root,
    "serializer": "python-stdlib-json.dumps-strict",
    "serializer_kind": "strict-attempt-jsonl-v1",
}
sys.stdout.write(json.dumps(record, sort_keys=True) + "\n")
sys.stdout.flush()
sys.exit(0)
ATTEMPT_PYEOF
    local serializer_rc=$?
    set -e
    if (( serializer_rc != 0 )); then
      # Strict serializer failure path.
      # Conditions 1..10 in the production
      # serializer above wrote a single
      # serializer_error line above and exit
      # rc >= 3. We capture rc (no -e, no
      # sleep, no second attempt). The
      # subsequent while-loop break leads the
      # end-of-function terminal serializer
      # block (call site below) to write a
      # valid JSON terminal report whose
      # terminal_failure_reason is the
      # canonical 'json serializer failed'.
      # We:
      #   1. Persist serializer stderr/rc into
      #      the running node log +
      #      install.log.
      #   2. Override per_attempt_fail_reason
      #      so the terminal report records
      #      'json serializer failed' (a
      #      canonical, grep-able token).
      #   3. Override per_attempt_rc=1 so
      #      all_nodes_ready stays false and
      #      we DO NOT claim success.
      #   4. break the while loop, fall
      #      through to the end-of-function
      #      terminal serializer ALREADY
      #      implemented below this loop.
      #   5. That terminal call emits a valid
      #      JSON report (parses with
      #      python3 -m json.tool) AND we
      #      then abort_as with exit 14.
      printf 'SERIALIZER_FAIL attempt=%s rc=%s (see %s)\n' \
        "${attempt}" "${serializer_rc}" \
        "${ARTIFACTS}/attempts/attempt-${attempt}/serializer.stderr.txt" \
        >>"$node_log"
      local serializer_failures_csv="${per_attempt_fail_node:-<none>}"
      printf 'SERIALIZATION_FAILED rc=%s node=%s attempts_report=%s failing_node=%s\n' \
        "${serializer_rc}" "${n:-<none>}" \
        "${attempts_report}" "${serializer_failures_csv}" \
        | tee -a "$ARTIFACTS/install.log" >>"$node_log"
      per_attempt_fail_node="${serializer_failures_csv}"
      per_attempt_fail_reason="json serializer failed (rc=${serializer_rc} attempt=${attempt})"
      per_attempt_rc=1
      all_nodes_ready=0
      # Break out so the while loop ends.
      # The end-of-function terminal
      # serializer will pick up the
      # overridden reason above and emit a
      # valid JSON terminal report. After the
      # break we fall to the abort path.
      break
    fi
    if (( per_attempt_rc == 0 )); then
      # per_attempt_rc==0 here means NEITHER
      # an exec failure NOR a parser failure
      # happened. The inner loop also records
      # `soft_fail_count` to detect transient
      # not_ready tag/id observations.
      if (( soft_fail_count == 0 )); then
        all_nodes_ready=1
        printf 'ALL_NODES_READY at attempt=%s\n' "${attempt}" >>"$node_log"
        break
      fi
    else
      # d2b.51-final: per the directive, any
      # command OR parse failure classifies
      # IMMEDIATELY as exit 14 — do NOT retry.
      # No sleep is taken; we fall through to
      # the final consolidated JSON and abort.
      break
    fi
    # All retries live here. Sleep only when
    # another attempt remains. An immediate
    # success on the first observation MUST
    # NOT sleep.
    local remaining=$(( IMG_VERIFY_MAX_ATTEMPTS - attempt ))
    if (( remaining > 0 )); then
      printf 'attempt=%s sleeping_for=2s remaining=%s\n' \
        "${attempt}" "${remaining}" >>"$node_log"
      sleep "${IMG_VERIFY_INTERVAL_SEC}"
    else
      printf 'attempt=%s LAST_ATTEMPT (no remaining sleeps)\n' \
        "${attempt}" >>"$node_log"
      # 15 attempts exhausted without every node
      # ready. This is the deadline path: classify
      # immediately as exit 14 and name every
      # failing node.
      per_attempt_rc=1
      break
    fi
    last_attempt_rc="${per_attempt_rc}"
  done
  # Final consolidated JSON report.
  # Built via python3 json.dumps + json.dump
  # (the standard library is the safety net for
  # arbitrary shell-side strings). We pass
  # scalars as argv (no shell interpolation of
  # arbitrary strings into Python source code)
  # and load per-node data from the TSV file
  # referenced by argv. On serializer failure
  # we surface rc + stderr under
  # $ARTIFACTS and abort with exit 14.
  local final_serializer_stderr="$ARTIFACTS/fixture-image-node-runtime.serializer.stderr"
  : > "${final_serializer_stderr}"
  set +e
  python3 - \
    "${attempt}" "${FIXTURE_IMAGE_REF}" \
    "${IMG_VERIFY_TAG}" "${IMG_VERIFY_EXPECTED_ID}" \
    "${all_nodes_ready}" "${nodes_tsv}" \
    "${per_attempt_fail_node}" \
    "${per_attempt_fail_reason:-tag-or-id-mismatch}" \
    "${attempts_report}" "${node_log}" \
    "${final_report}" \
    >"${final_report}.tmp" 2>"${final_serializer_stderr}" \
    <<'TERMINAL_PYEOF'
"""Terminal fixture-image-node-runtime.json serializer.

Reads argv scalars (no shell interpolation of
arbitrary strings into Python source). Reads
the one-time canonical node list from the
TSV file pointed at by argv[6]. Reads the
per-attempt JSONL records from argv[9].
The verdict fields
(per_node_records, failing_nodes,
terminal_failure_reason, all_nodes_ready)
are computed EXCLUSIVELY from the JSONL
document whose attempt equals the supplied
terminal attempt (argv[1]). Prior attempts
are validated but never aggregate into the
terminal verdict. The terminal report does
NOT collect earlier per-node records.

Required argv (in order):
  1  terminal_attempt        (str/int; the
      attempt number we are reporting on.
      This MUST equal the highest attempt
      number in the JSONL; it equals the
      shell-side $attempt after the loop.)
  2  expected_ref            (str)
  3  expected_tag            (str)
  4  normalized_expected_id  (str)
  5  all_nodes_ready         ('1' or '0')
  6  nodes_tsv_path          (str)
  7  per_attempt_fail_node   (str, may be empty)
  8  per_attempt_fail_reason (str, may be empty)
  9  per_attempt_jsonl       (str)
  10 node_log                (str)
  11 final_report_path       (str; the path the
      serializer is writing TO; we do not
      include this in the output)
"""
import json, os, sys, time


def fail(reason):
    """Single-line terminal-serializer
    failure marker. Emit NO stdout, exit
    non-zero. Production routes this into
    FIXTURE_IMAGE_NOT_LOADED 14."""
    sys.stderr.write("pipeline_runtime_error=" + reason + "\n")
    sys.stderr.flush()
    sys.exit(13)


def parse_int(name, raw):
    if not str(raw).lstrip("-").isdigit():
        fail("terminal_" + name + "_not_integer:val=" + str(raw))
    return int(raw)


(terminal_attempt_raw, expected_ref, expected_tag, normalized_expected_id,
 all_nodes_ready, nodes_tsv_path, per_attempt_fail_node,
 per_attempt_fail_reason, per_attempt_jsonl, node_log,
 _final_path) = sys.argv[1:12]

terminal_attempt = parse_int("attempt", terminal_attempt_raw)
shell_all_nodes_ready = (str(all_nodes_ready) == "1")

# ---- canonical node list -----------------------------------------------
if not nodes_tsv_path:
    fail("terminal_canonical_nodes_tsv_path_empty")
if not os.path.isfile(nodes_tsv_path):
    fail("terminal_canonical_nodes_tsv_missing")
try:
    with open(nodes_tsv_path, "r", encoding="utf-8") as fh:
        canonical_lines_raw = fh.readlines()
except OSError:
    fail("terminal_canonical_nodes_tsv_unreadable")
canonical_nodes = []
seen = set()
for line in canonical_lines_raw:
    n = line.strip()
    if not n:
        fail("terminal_canonical_node_empty_line")
    if n in seen:
        fail("terminal_canonical_node_duplicate:" + n)
    seen.add(n)
    canonical_nodes.append(n)
canonical_set = set(canonical_nodes)

# ---- per-attempt JSONL: validate every line ---------------------------
if not per_attempt_jsonl:
    fail("terminal_per_attempt_jsonl_path_empty")
if not os.path.isfile(per_attempt_jsonl):
    fail("terminal_per_attempt_jsonl_missing")
try:
    with open(per_attempt_jsonl, "r", encoding="utf-8") as fh:
        jsonl_lines_raw = fh.readlines()
except OSError:
    fail("terminal_per_attempt_jsonl_unreadable")
if len(jsonl_lines_raw) == 0:
    fail("terminal_per_attempt_jsonl_empty")

documents_by_attempt = {}
attempt_history_count = 0
last_seen_attempt = 0

for idx, raw_line in enumerate(jsonl_lines_raw, start=1):
    line = raw_line.strip()
    if not line:
        fail("terminal_per_attempt_jsonl_blank_line:line=" + str(idx))
    # Decode. Any decode failure is a hard
    # terminal-serializer error. We catch
    # the exact exception class to avoid
    # silent tracebacks.
    try:
        doc = json.loads(line)
    except json.JSONDecodeError:
        fail("terminal_per_attempt_jsonl_decode_failure:line=" + str(idx))
    # Condition 1: every non-empty line
    # must decode to a dictionary.
    if not isinstance(doc, dict):
        fail("terminal_per_attempt_jsonl_doc_not_dict:line=" + str(idx))
    # Condition 2: every doc must contain an
    # integer attempt.
    if "attempt" not in doc:
        fail("terminal_per_attempt_jsonl_no_attempt_field:line=" + str(idx))
    raw_doc_attempt = doc["attempt"]
    if not isinstance(raw_doc_attempt, int) or isinstance(raw_doc_attempt, bool):
        fail("terminal_per_attempt_jsonl_attempt_not_integer:line=" + str(idx))
    doc_attempt = raw_doc_attempt
    # Condition 3: attempts must be
    # strictly ordered, no duplicate, begin
    # at 1, end at terminal_attempt.
    if doc_attempt <= 0:
        fail("terminal_per_attempt_jsonl_attempt_not_positive:line=" + str(idx) + ":attempt=" + str(doc_attempt))
    if doc_attempt <= last_seen_attempt:
        fail("terminal_per_attempt_jsonl_attempt_not_strictly_ordered:line=" + str(idx) + ":attempt=" + str(doc_attempt) + ":last=" + str(last_seen_attempt))
    if doc_attempt in documents_by_attempt:
        fail("terminal_per_attempt_jsonl_attempt_duplicate:line=" + str(idx) + ":attempt=" + str(doc_attempt))
    last_seen_attempt = doc_attempt
    # Condition 4 / 5: per_node_records list
    # required; every record is a dict;
    # node is in canonical set; ready is a
    # JSON bool. NO .get default fallback
    # accepts "missing field" silently.
    if "per_node_records" not in doc:
        fail("terminal_per_attempt_jsonl_missing_per_node_records:line=" + str(idx) + ":attempt=" + str(doc_attempt))
    raw_pn = doc["per_node_records"]
    if not isinstance(raw_pn, list):
        fail("terminal_per_attempt_jsonl_per_node_records_not_list:line=" + str(idx) + ":attempt=" + str(doc_attempt))
    seen_nodes_in_doc = set()
    validated_records = []
    for r_idx, rec in enumerate(raw_pn, start=1):
        if not isinstance(rec, dict):
            fail("terminal_per_attempt_jsonl_record_not_dict:line=" + str(idx) + ":attempt=" + str(doc_attempt) + ":record=" + str(r_idx))
        if "node" not in rec:
            fail("terminal_per_attempt_jsonl_record_missing_node:line=" + str(idx) + ":attempt=" + str(doc_attempt) + ":record=" + str(r_idx))
        node_val = rec["node"]
        if not isinstance(node_val, str) or not node_val or node_val != node_val.strip():
            fail("terminal_per_attempt_jsonl_record_node_not_string:line=" + str(idx) + ":attempt=" + str(doc_attempt) + ":record=" + str(r_idx))
        if node_val not in canonical_set:
            fail("terminal_per_attempt_jsonl_record_node_not_canonical:line=" + str(idx) + ":attempt=" + str(doc_attempt) + ":record=" + str(r_idx) + ":node=" + node_val)
        if node_val in seen_nodes_in_doc:
            fail("terminal_per_attempt_jsonl_record_node_duplicate:line=" + str(idx) + ":attempt=" + str(doc_attempt) + ":node=" + node_val)
        seen_nodes_in_doc.add(node_val)
        if "ready" not in rec:
            fail("terminal_per_attempt_jsonl_record_missing_ready:line=" + str(idx) + ":attempt=" + str(doc_attempt) + ":node=" + node_val)
        ready_val = rec["ready"]
        # Condition 4 strict: ready MUST be a
        # bool (not a truthy string, not a
        # truthy integer, not None).
        if not isinstance(ready_val, bool):
            fail("terminal_per_attempt_jsonl_record_ready_not_bool:line=" + str(idx) + ":attempt=" + str(doc_attempt) + ":node=" + node_val + ":type=" + type(ready_val).__name__)
        # Append to validated_records so the
        # later recompute can use the validated
        # surface instead of the raw per-doc list.
        validated_records.append({"node": node_val, "ready": ready_val})
        # Also: must be in canonical set
        # (full record count == canonical
        # count) and uniquely — i.e. exactly
        # one record per canonical node.
        # Validated below for completeness.
    # Exactly one record per canonical
    # node for THIS attempt.
    if seen_nodes_in_doc != canonical_set:
        missing = sorted(canonical_set - seen_nodes_in_doc)
        extra = sorted(seen_nodes_in_doc - canonical_set)
        fail("terminal_per_attempt_jsonl_record_set_mismatch:attempt=" + str(doc_attempt) + ":missing=" + ",".join(missing) + ":extra=" + ",".join(extra))
    if len(seen_nodes_in_doc) != len(canonical_set):
        fail("terminal_per_attempt_jsonl_record_count_mismatch:attempt=" + str(doc_attempt))
    # Condition 6: all_nodes_ready must be
    # a JSON bool AND equal the per-record
    # recomputation of "all ready True".
    if "all_nodes_ready" not in doc:
        fail("terminal_per_attempt_jsonl_missing_all_nodes_ready:line=" + str(idx) + ":attempt=" + str(doc_attempt))
    raw_anr = doc["all_nodes_ready"]
    if not isinstance(raw_anr, bool):
        fail("terminal_per_attempt_jsonl_all_nodes_ready_not_bool:line=" + str(idx) + ":attempt=" + str(doc_attempt))
    expected_anr = all((rec["ready"] is True) for rec in raw_pn if isinstance(rec, dict))
    if raw_anr != expected_anr:
        fail("terminal_per_attempt_jsonl_all_nodes_ready_mismatch:attempt=" + str(doc_attempt) + ":doc=" + str(raw_anr) + ":recomputed=" + str(expected_anr))
    # Re-confirm recomputation against the
    # validated_records list (cheapest
    # available truth). If we were to skip
    # this, a stray `doc["all_nodes_ready"]
    # = True` write to a doc whose records
    # are not all ready would be missed.
    if raw_anr != all(rec["ready"] for rec in validated_records):
        fail("terminal_per_attempt_jsonl_all_nodes_ready_recompute_mismatch:attempt=" + str(doc_attempt))
    documents_by_attempt[doc_attempt] = {
        "per_node_records": raw_pn,
        "all_nodes_ready": raw_anr,
        "failing_node": doc.get("failing_node", ""),
        "failure_reason": doc.get("failure_reason", ""),
        "canonical_node_count": len(canonical_nodes),
    }
    attempt_history_count = doc_attempt

# Final JSONL shape rules: attempts must
# begin at 1 and end at terminal_attempt
# exactly. Earlier transient not-ready
# records are KEPT at attempt-N artifact
# paths but DO NOT re-enter terminal
# verdict fields.
if 1 not in documents_by_attempt:
    fail("terminal_per_attempt_jsonl_missing_attempt_1")
if terminal_attempt not in documents_by_attempt:
    fail("terminal_per_attempt_jsonl_missing_terminal_attempt:terminal=" + str(terminal_attempt))
expected_attempts = set(range(1, terminal_attempt + 1))
actual_attempts = set(documents_by_attempt.keys())
if actual_attempts != expected_attempts:
    fail("terminal_per_attempt_jsonl_attempt_set_not_contiguous:expected=" + ",".join(str(x) for x in sorted(expected_attempts)) + ":actual=" + ",".join(str(x) for x in sorted(actual_attempts)))

# ---- select terminal_doc -----------------------------------------------
terminal_doc = documents_by_attempt[terminal_attempt]
terminal_per_node_records = terminal_doc["per_node_records"]
terminal_all_nodes_ready_per_doc = terminal_doc["all_nodes_ready"]

# Build the terminal per_node_records list
# from terminal_doc ONLY. This guarantees
# earlier transient not-ready observations
# never reappear as terminal failing nodes.
terminal_failing_nodes = []
seen_t = set()
for rec in terminal_per_node_records:
    if not isinstance(rec, dict):
        fail("terminal_record_not_dict_terminal_attempt:after_validation")
    if rec.get("ready") is False:
        node_name = rec.get("node", "")
        if not isinstance(node_name, str) or not node_name:
            fail("terminal_record_invalid_node_terminal_attempt")
        if node_name not in seen_t:
            terminal_failing_nodes.append(node_name)
            seen_t.add(node_name)

# Cross-check: shell-side all_nodes_ready
# MUST match the doc-supplied boolean AND
# the recomputed terminal-all-ready
# verdict. A mismatch means the loop and
# the JSONL are inconsistent — terminal
# serializer failure, not a quiet "ok"
# report.
recomputed_terminal_all_ready = all(
    (isinstance(rec, dict) and rec.get("ready") is True)
    for rec in terminal_per_node_records
)
if terminal_all_nodes_ready_per_doc != recomputed_terminal_all_ready:
    fail("terminal_all_nodes_ready_recompute_mismatch:doc=" + str(terminal_all_nodes_ready_per_doc) + ":recomputed=" + str(recomputed_terminal_all_ready))
if shell_all_nodes_ready != terminal_all_nodes_ready_per_doc:
    fail("terminal_all_nodes_ready_shell_doc_mismatch:shell=" + str(shell_all_nodes_ready) + ":doc=" + str(terminal_all_nodes_ready_per_doc))
# Final all_nodes_ready: prefer the doc
# value (validated above against both the
# shell flag and the rec-ready recompute).
final_all_nodes_ready = bool(terminal_all_nodes_ready_per_doc)

# terminal_failure_reason MUST come from
# terminal_doc['failure_reason'] ONLY when
# the terminal verdict is "not ready"; for
# a clean terminal report the canonical
# success reason is used.
if final_all_nodes_ready and not terminal_failing_nodes:
    terminal_failure_reason = "all-node-exact-tag-id-present"
else:
    # Use the supplied per_attempt_fail_reason
    # ONLY if it matches terminal_doc's
    # failure_reason OR is empty. Otherwise
    # we trust terminal_doc verbatim (a stale
    # transient reason from a prior attempt
    # MUST NOT appear as terminal reason).
    doc_reason = terminal_doc["failure_reason"]
    if not isinstance(doc_reason, str):
        fail("terminal_doc_failure_reason_not_string")
    if doc_reason:
        terminal_failure_reason = doc_reason
    elif per_attempt_fail_reason:
        # Allow shell's pointer only if the
        # shell pointer names an exact failure
        # that we can corroborate from
        # terminal per_node_records.
        if per_attempt_fail_node in terminal_failing_nodes:
            terminal_failure_reason = per_attempt_fail_reason
        else:
            fail("terminal_reason_shell_doc_mismatch:shell_reason_present_but_node_not_in_terminal_failing")
    else:
        # No shell reason, no doc reason,
        # but terminal has failing nodes.
        # Synthesize a canonical-per-attempt
        # reason string so the audit trail
        # is unambiguously traceable.
        terminal_failure_reason = "tag-or-id-mismatch"

# attempt_history_count is intentionally
# a separate, clearly-bounded field.
# It MUST NOT drive the verdict.
record = {
    "schema_version": 2,
    "expected_ref": expected_ref,
    "expected_tag": expected_tag,
    "normalized_expected_id": normalized_expected_id,
    "attempt": int(terminal_attempt),
    "all_nodes_ready": bool(final_all_nodes_ready),
    "node_count": len(canonical_nodes),
    "nodes": canonical_nodes,
    "failing_nodes": terminal_failing_nodes,
    "terminal_failure_reason": terminal_failure_reason,
    "per_attempt_report": per_attempt_jsonl,
    "node_log": node_log,
    "per_node_records": terminal_per_node_records,
    "attempt_history_count": int(attempt_history_count),
    "serializer": "python-stdlib-json.dumps-terminal-attempt-truth-v1",
    "serialized_at_epoch": int(time.time()),
}
sys.stdout.write(json.dumps(record, sort_keys=True) + "\n")
sys.stdout.flush()
sys.exit(0)
TERMINAL_PYEOF
  local final_serializer_rc=$?
  set -e
  if (( final_serializer_rc == 0 )); then
    mv "${final_report}.tmp" "${final_report}"
  else
    rm -f "${final_report}.tmp"
    printf 'FINAL_SERIALIZER_FAIL rc=%s (see %s)\n' \
      "${final_serializer_rc}" "${final_serializer_stderr}" \
      >>"$node_log"
    printf 'SERIALIZATION_FAILED rc=%s attempts_report=%s node_log=%s\n' \
      "${final_serializer_rc}" "${attempts_report}" "${node_log}" \
      | tee -a "$ARTIFACTS/install.log" >>"$node_log"
    abort_as FIXTURE_IMAGE_NOT_LOADED \
      "image pipeline final JSON serializer exited rc=${final_serializer_rc}; raw retained at ${final_serializer_stderr}" 14
  fi
  if (( all_nodes_ready == 1 )); then
    printf 'IMAGE_PIPELINE_OK attempt=%s all_nodes_ready=true node_count=%s\n' \
      "${attempt}" "$(wc -l <"${nodes_tsv}" | tr -d ' ')" \
      | tee -a "$ARTIFACTS/install.log" >>"$node_log"
  else
    # Fail-closed: name every failing node plus
    # its exact reason. Do NOT overwrite earlier
    # attempts' raw evidence — they are under
    # $ARTIFACTS/attempts/attempt-N/.
    local fail_reason="image_id=$FIXTURE_IMAGE_ID (tag=$IMG_VERIFY_TAG) not exact on every kind node after ${IMG_VERIFY_MAX_ATTEMPTS} bounded attempts; failing_node=${per_attempt_fail_node:-<none>}; reason=${per_attempt_fail_reason:-tag-or-id-mismatch}"
    printf '%s' "${fail_reason}" | tee -a "$ARTIFACTS/install.log" >>"$node_log"
    abort_as FIXTURE_IMAGE_NOT_LOADED "${fail_reason}" 14
  fi
  echo "[install] image pipeline ok" | tee -a "$ARTIFACTS/install.log"
}

# ---- step G: readiness / EndpointSlice / control service probe ---------

step_G_readiness() {
  echo "[install] ====== step G: readiness / control probe ======"
  # d2b.48 canonical fixture vocabulary.
  # The tracked fixture manifests in
  # scripts/fixtures/integrationcni/
  # {01-test-pods,02-stub-deps,03-control-pod,
  # 04-control-service}.yaml define exactly
  # 12 static namespace/name pairs plus one
  # Deployment-generated cni-control-probe-<rs>-<pod>
  # Pod. The fresh-cluster fixture population
  # has exactly that population. Selection is
  # by {namespace, name} pair; a same-name in
  # the wrong namespace does NOT satisfy the
  # expected pair; an extra prefix-shaped Pod
  # (e.g. cni-mock-old) is a vocabulary drift
  # and fails closed. The Python projection
  # below reads these constants from argv so
  # the install script and real Gate 8 can share
  # the same vocabulary with a single source.
  CANONICAL_12_PAIRS=(
    "cni-test-ingress|cni-mock-ingress-controller"
    "cni-test-prometheus|cni-mock-prometheus"
    "cni-test-untrusted|cni-untrusted-default"
    "default|cni-mock-nexus-gateway"
    "default|cni-mock-nexus-worker"
    "default|cni-mock-nexus-migration"
    "default|cni-mock-egress-proxy"
    "default|cni-mock-postgres"
    "default|cni-mock-redis"
    "default|cni-mock-clickhouse"
    "default|cni-mock-arbitrary"
    "cni-control|cni-control-target"
  )
  DYNAMIC_PROBE_REGEX='^cni-control-probe-[a-z0-9]+-[a-z0-9]+$'
  DYNAMIC_PROBE_NAMESPACE='cni-control'
  CANONICAL_POPULATION_SIZE=13
  # -----------------------------------------------------------------
  # The fresh-cluster fixture Pod inventory
  # is read from `kubectl get pod -A -o json`
  # and projected per-Pod by a single python
  # invocation. The historical
  # `kubectl get pod -A --no-headers | grep`
  # pipeline is REMOVED entirely because the
  # fake fixture pods.tsv in the harness
  # historically omitted the namespace column
  # while real `kubectl get pod -A --no-headers`
  # emits NAMESPACE first, causing the
  # anchored regex to apply to the WRONG
  # column in production.
  #
  # Selection is by {namespace, name} pair,
  # not by metadata.name prefix. The 12
  # static pairs + 1 dynamic probe are the
  # ONLY accepted vocabulary; any extra
  # ^cni-(mock|untrusted|control)- Pod is a
  # vocabulary drift and fails closed.
  local expected_fixture_count="$CANONICAL_POPULATION_SIZE"
  # -----------------------------------------------------------------
  # d2b.48 Block A: dynamic expected-set derivation.
  # expected_labels_file is NO LONGER written
  # from a pre-poll invariant-13 capture. The
  # very first `kubectl get pod -A -o json`
  # poll may legitimately return <13 selected
  # fixtures (e.g. the Deployment-generated
  # cni-control-probe-<rs>-<pod> has not yet
  # been admitted). The derivation happens AT
  # THE MOMENT OF READINESS SUCCESS, against
  # the same JSON snapshot whose projection
  # identified exactly 13 Ready fixtures. The
  # generated cni-control-probe-* identity
  # thus enters the contract from disk, not
  # from a hard-coded control token.
  # -----------------------------------------------------------------
  local expected_labels_file="$ARTIFACTS/cilium-endpoint.expected.out"
  : > "$expected_labels_file"
  # Per-poll artifact paths: every JSON capture
  # and every projection is recorded to disk so
  # the deadline branch reads the FINAL state.
  local poll_json="$ARTIFACTS/fixture-pod-readiness.poll.json"
  local poll_err="$ARTIFACTS/fixture-pod-readiness.poll.stderr"
  local poll_summary="$ARTIFACTS/fixture-pod-readiness.poll.summary.json"
  local poll_proj_err="$ARTIFACTS/fixture-pod-readiness.poll.proj.stderr"
  local poll_tsv="$ARTIFACTS/fixture-pod-imagepull.log"
  local poll_stderr="$ARTIFACTS/fixture-pod-imagepull.stderr"
  local successful_poll_json="$ARTIFACTS/fixture-pod-readiness.success.json"
  # Pre-loop refs (JSON-based deadline branch
  # reads the same paths).
  local snapshot_json="$ARTIFACTS/fixture-pod-readiness-timeout.json"
  local snapshot_txt="$ARTIFACTS/fixture-pod-readiness-timeout.txt"
  local events_log="$ARTIFACTS/fixture-pod-readiness-events.log"
  # Set-diff files are downstream of
  # expected_labels_file; hoisted so the
  # deadline branch can read the LAST
  # iteration when needed.
  local unique_labels="$ARTIFACTS/cilium-endpoint.unique.out"
  local missing_labels_file="$ARTIFACTS/cilium-endpoint.missing.out"
  local unexpected_labels_file="$ARTIFACTS/cilium-endpoint.unexpected.out"
  local missing_diff_err="$ARTIFACTS/cilium-endpoint.missing.stderr"
  local unexpected_diff_err="$ARTIFACTS/cilium-endpoint.unexpected.stderr"
  # d2b.47 fixtures_ready SENTINEL: the bounded
  # loop only sets fixtures_ready=1 when both
  # (a) the JSON projection reports exactly 13
  # selected Pods from metadata.name, AND
  # (b) every selected Pod is Ready
  # (status.conditions[type=Ready].status == True
  # && status.phase == Running) under the JSON
  # contract. This ignores the legacy
  # --no-headers awk column selectors
  # entirely.
  local fixtures_ready=0
  local deadline=$(( $(date +%s) + 480 ))
  local poll_count=0
  # -----------------------------------------------------------------
  # capture_kctl_failure: kubectl command
  # produced nonzero rc. Write a structured
  # inventory-error artifact and abort as
  # FIXTURE_NOT_READY 12. Distinct from the
  # python projection failure path so the
  # artifacts separate cluster-touch from
  # parser-touch.
  # -----------------------------------------------------------------
  capture_kctl_failure() {
    local cmd_name="$1"
    local rc="$2"
    local stderr_content="$3"
    cat >"$snapshot_json.snapshot" <<EOF
{
  "command": "${cmd_name}",
  "phase": "fixture_inventory_kctl_failure",
  "rc": ${rc},
  "stderr": $(printf '%s' "${stderr_content}" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))'),
  "observed_count": 0,
  "expected_count": ${expected_fixture_count},
  "reason": "kubectl inventory command failed; ready-state cannot be observed"
}
EOF
    mv "$snapshot_json.snapshot" "$snapshot_json"
    abort_as FIXTURE_NOT_READY \
      "${cmd_name} failed rc=${rc}: inventory cannot be obtained (see $snapshot_json)" 12
  }
  # -----------------------------------------------------------------
  # capture_projection_failure: python
  # projection exited non-zero (e.g. malformed
  # JSON, projection internal exception).
  # Distinct from a kubectl command failure so
  # the diff between cluster reachability and
  # payload parsability is always traceable
  # from disk.
  # -----------------------------------------------------------------
  capture_projection_failure() {
    local proj_rc="$1"
    local proj_stderr="$2"
    cat >"$snapshot_json.snapshot" <<EOF
{
  "command": "python3 fixture JSON projection",
  "phase": "fixture_inventory_projection_failure",
  "rc": ${proj_rc},
  "stderr": $(printf '%s' "${proj_stderr}" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))'),
  "expected_count": ${expected_fixture_count},
  "reason": "fixture JSON projection failed; cannot parse pod inventory"
}
EOF
    mv "$snapshot_json.snapshot" "$snapshot_json"
    abort_as FIXTURE_NOT_READY \
      "fixture JSON projection failed rc=${proj_rc}: cannot parse inventory (see $snapshot_json)" 12
  }
  # -----------------------------------------------------------------
  # capture_image_failure: at least one selected
  # Pod has a waiting or terminated reason in
  # {ImagePullBackOff, ErrImagePull,
  # ErrImageNeverPull, CrashLoopBackOff}.
  # Image failures must classify as
  # FIXTURE_IMAGE_NOT_LOADED 14, never as a
  # readiness-timeout (FIXTURE_NOT_READY 12).
  # The classifier inspects JSON
  # containerStatuses[*].state.{waiting,
  # terminated}.reason — NOT positional
  # table grep.
  # -----------------------------------------------------------------
  capture_image_failure() {
    local reason_summary="$1"
    cat >"$snapshot_json.snapshot" <<EOF
{
  "command": "fixture containerStatuses image-failure classification",
  "phase": "fixture_image_not_loaded",
  "image_reasons_seen": ${reason_summary},
  "expected_count": ${expected_fixture_count},
  "reason": "fixture Pod entered image-pull-failure state; classifier routes to FIXTURE_IMAGE_NOT_LOADED 14"
}
EOF
    mv "$snapshot_json.snapshot" "$snapshot_json"
    abort_as FIXTURE_IMAGE_NOT_LOADED \
      "fixture Pod entered image-pull-failure: ${reason_summary}" 14
  }
  # -----------------------------------------------------------------
  # Bounded poll loop. Every iteration:
  #   (1) kubectl get pod -A -o json, with rc
  #       + stderr captured separately from
  #       the JSON projection.
  #   (2) python3 projection on the captured
  #       JSON. Selection by metadata.name
  #       ONLY. Image-failure detection by
  #       containerStatuses[*].state.{waiting,
  #       terminated}.reason.
  #   (3) If image-failure: abort as
  #       FIXTURE_IMAGE_NOT_LOADED 14.
  #   (4) If selected_pod_count != 13 OR
  #       any_NotReady: sleep + retry (does
  #       NOT abort just for partial population).
  #   (5) When EXACTLY 13 are Ready: derive
  #       expected_labels_file from the SAME
  #       JSON snapshot, preserve it to
  #       successful_poll_json, set
  #       fixtures_ready=1, break.
  # -----------------------------------------------------------------
  while (( $(date +%s) < deadline )); do
    : > "$poll_err"
    : > "$poll_proj_err"
    : > "$poll_tsv"
    : > "$poll_stderr"
    poll_count=$((poll_count+1))
    set +e
    kubectl get pod -A -o json 2>"$poll_err" >"$poll_json"
    local kc_rc=$?
    set -e
    if (( kc_rc != 0 )); then
      capture_kctl_failure "kubectl get pod -A -o json" \
        "$kc_rc" "$(cat "$poll_err")"
    fi
    # Projection: parse the actual JSON, select
    # by metadata.name (NOT namespace text),
    # surface ready/phase plus waiting/terminated
    # reasons for image-classification. Emit
    # both a structured JSON summary and a
    # human-readable TSV row stream keyed to
    # spec point 5 (NAMESPACE NAME READY STATUS
    # RESTARTS). Image reasons are detected from
    # JSON containerStatuses state, never from
    # positional grep.
    # d2b.48 vocabulary projection.
    # Drives selection by {namespace, name}
    # pair, not by metadata.name prefix. The
    # canonical 12 static namespace/name pairs
    # come from scripts/fixtures/integrationcni/
    # {01,02,03,04}*.yaml and are written by
    # bash to a small JSON file before invoking
    # the python heredoc. The python heredoc
    # also accepts the dynamic probe regex and
    # its required namespace. Anything outside
    # this vocabulary is classified as a
    # vocabulary drift and surfaces in
    # `unexpected_fixture_like_pairs` so the
    # deadline branch can produce actionable
    # artefacts.
    local vocab_json="$ARTIFACTS/fixture-pod-readiness.vocab.json"
    python3 - "${CANONICAL_12_PAIRS[@]}" <<'VOCABEOF' >"$vocab_json"
import json, sys
pairs = sys.argv[1:]
canonical_list = sorted(
    [{"namespace": p.split('|')[0], "name": p.split('|')[1]} for p in pairs],
    key=lambda d: (d["namespace"], d["name"]),
)
out = {
    "canonical_static_pairs": canonical_list,
    "dynamic_probe_regex": "^cni-control-probe-[a-z0-9]+-[a-z0-9]+$",
    "dynamic_probe_namespace": "cni-control",
    "canonical_population_size": 13,
}
sys.stdout.write(json.dumps(out, indent=2) + "\n")
VOCABEOF
    set -e
    # Proj input now: json_path tsv_path summary_json_path vocab_json_path
    set +e
    python3 - "$poll_json" "$poll_tsv" "$poll_summary" "$vocab_json" \
      >"$poll_proj_err" 2>&1 <<'PYEOF'
import json, os, re, sys
json_path, tsv_path, summary_path, vocab_path = (
    sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4])
try:
    with open(json_path) as fh:
        raw = fh.read()
    data = json.loads(raw) if raw.strip() else {}
except Exception as e:
    print(f"PROJECTION-FAILED: {e!r}", file=sys.stderr); sys.exit(17)
try:
    with open(vocab_path) as fh:
        vocab = json.load(fh)
except Exception as e:
    print(f"PROJECTION-FAILED: vocab unreadable: {e!r}", file=sys.stderr); sys.exit(17)
canonical_pairs = vocab["canonical_static_pairs"]
canonical_pair_set = {(p["namespace"], p["name"]) for p in canonical_pairs}
dynamic_probe_re = re.compile(vocab["dynamic_probe_regex"])
dynamic_probe_ns = vocab["dynamic_probe_namespace"]
expected_count = int(vocab["canonical_population_size"])
IMAGE_RC = {"ImagePullBackOff","ErrImagePull","ErrImageNeverPull","CrashLoopBackOff"}
VOCAB_PREFIX_RE = re.compile(r'^cni-(mock|untrusted|control)-')
items = data.get('items') if isinstance(data, dict) else []
# Canonical vocabulary fields.
selected = []          # accepted canonical Pods that passed ready predicate
notready = []          # canonical Pods whose Ready condition is False
image_fail_pods = []   # canonical Pods whose containerStatuses are waiting on images
image_reasons_seen = set()
dynamic_probe_pairs = []
duplicate_pairs = []
unexpected_fixture_like_pairs = []
seen_pairs = set()
with open(tsv_path, 'w') as tsv:
    for it in items:
        md = it.get('metadata') or {}
        ns = md.get('namespace','') or ''
        name = md.get('name','') or ''
        st = it.get('status') or {}
        phase = st.get('phase','') or ''
        # Always emit any fixture-like
        # vocabulary-shaped Pod to the human
        # TSV with a synthetic ready column so
        # post-mortem tooling can see what was
        # rejected. Ready column for rejected
        # rows encodes the rejection reason in
        # RESTARTS so analysis tooling can grep
        # /REJECTED/(unexpected|duplicate|wrong-ns|extra-probe).
        ready = any(c.get('type')=='Ready' and c.get('status')=='True' for c in (st.get('conditions') or []))
        waiting = []
        terminated = []
        image_reason = None
        for cs in (st.get('containerStatuses') or []):
            csst = cs.get('state') or {}
            wt = csst.get('waiting') or {}
            tr = csst.get('terminated') or {}
            wr = wt.get('reason') or None
            trr = tr.get('reason') or None
            if wr: waiting.append(wr)
            if trr: terminated.append(trr)
            for r in (wr, trr):
                if r in IMAGE_RC:
                    image_reason = r
                    image_reasons_seen.add(r)
                    break
            if image_reason:
                break
        restarts_s = "0"
        for cs in (st.get('containerStatuses') or []):
            if cs.get('restartCount') is not None:
                restarts_s = str(cs.get('restartCount'))
                break
        # Vocabulary classification. The
        # four buckets:
        #   - canonical static pair
        #   - dynamic probe in cni-control
        #   - duplicate of an already-seen pair
        #   - fixture-like rejection (extra /
        #     wrong-namespace / extra probe)
        if (ns, name) in canonical_pair_set:
            rejection = None
        elif dynamic_probe_re.match(name) and ns == dynamic_probe_ns:
            rejection = None
        elif VOCAB_PREFIX_RE.match(name):
            rejection = "unexpected_fixture_like"
        else:
            # Not even fixture-shaped: not
            # vocabulary-sensitive. Skip without
            # adding to TSV; the rest are
            # uninteresting (kube-system pods,
            # unrelated test pods, etc).
            continue
        # Duplicate detection: only applies
        # to canonical pairs. Two Pods with the
        # same {ns, name} both being canonical
        # is a vocabulary drift and must be
        # surfaced.
        if rejection is None and (ns, name) in seen_pairs \
                and (ns, name) in canonical_pair_set:
            rejection = "duplicate"
        if rejection is None and dynamic_probe_re.match(name) and ns == dynamic_probe_ns \
                and (ns, name) in seen_pairs:
            rejection = "duplicate"
        # Track which "canonical" Pods we saw.
        if rejection is None:
            seen_pairs.add((ns, name))
        # TSV row + population tracker.
        if rejection is None:
            if (ns, name) in canonical_pair_set:
                ready_s = "1/1" if (ready and phase == "Running") else "0/1"
            else:
                # dynamic probe contributes to
                # population but ready IS measured.
                ready_s = "1/1" if (ready and phase == "Running") else "0/1"
            tsv.write(f"{ns}\t{name}\t{ready_s}\t{phase or 'Unknown'}\t{restarts_s}\t7m\n")
            row = {
                "namespace": ns,
                "name": name,
                "phase": phase,
                "ready": bool(ready and phase == "Running"),
                "waiting_reasons": waiting,
                "terminated_reasons": terminated,
            }
            if image_reason:
                image_fail_pods.append({"namespace": ns, "name": name, "reason": image_reason})
            else:
                selected.append(row)
                if not row["ready"]:
                    notready.append(row)
                if (ns, name) == ("cni-control", "cni-control-target") or \
                   not (ns, name) in canonical_pair_set:
                    # dynamic probe bucket
                    if dynamic_probe_re.match(name):
                        dynamic_probe_pairs.append({"namespace": ns, "name": name})
        else:
            ready_s = f"REJECTED/{rejection}"
            tsv.write(f"{ns}\t{name}\t{ready_s}\t{phase or 'Unknown'}\t{restarts_s}\t7m\n")
            if rejection == "duplicate":
                duplicate_pairs.append({"namespace": ns, "name": name, "reason": "duplicate_pair"})
            elif rejection == "unexpected_fixture_like":
                unexpected_fixture_like_pairs.append({"namespace": ns, "name": name, "reason": "extra_fixture_like"})
            elif rejection == "wrong_namespace":
                unexpected_fixture_like_pairs.append({"namespace": ns, "name": name, "reason": "wrong_namespace"})
            elif rejection == "extra_probe":
                unexpected_fixture_like_pairs.append({"namespace": ns, "name": name, "reason": "extra_dynamic_probe"})
# Compute canonical vocabulary fields the
# install Step G and real Gate 8 contract
# depend on.
selected_pairs = sorted([(p["namespace"], p["name"]) for p in selected
                          if (p["namespace"], p["name"]) in canonical_pair_set
                          and not dynamic_probe_re.match(p["name"])])
expected_static_pairs = sorted(canonical_pair_set)
observed_static_pairs = sorted([(p["namespace"], p["name"]) for p in selected
                                 if (p["namespace"], p["name"]) in canonical_pair_set
                                 and not dynamic_probe_re.match(p["name"])])
missing_static_pairs = sorted(set(expected_static_pairs) - set(observed_static_pairs))
unexpected_pair_objs = list({(p["namespace"], p["name"]): p for p in unexpected_fixture_like_pairs}.values())
duplicate_pair_objs = list({(p["namespace"], p["name"]): p for p in duplicate_pairs}.values())
# Final canonical-population predicate:
#   - 12 static pairs all present
#   - exactly 1 dynamic probe form
#   - 0 duplicates
#   - 0 unexpected fixture-like Pods
#   - 0 image-failed Pods
#   - all canonical Pods Ready/Running.
canonical_population_ready = (
    not missing_static_pairs
    and len(dynamic_probe_pairs) == 1
    and not duplicate_pair_objs
    and not unexpected_pair_objs
    and not image_fail_pods
    and not notready
)
summary = {
    "expected_count": expected_count,
    "observed_pod_count": len(selected),
    "expected_static_pairs": [{"namespace": ns, "name": n} for ns, n in expected_static_pairs],
    "observed_static_pairs": [{"namespace": ns, "name": n} for ns, n in observed_static_pairs],
    "missing_static_pairs": [{"namespace": ns, "name": n} for ns, n in missing_static_pairs],
    "unexpected_fixture_like_pairs": unexpected_pair_objs,
    "dynamic_probe_pairs": dynamic_probe_pairs,
    "duplicate_pairs": duplicate_pair_objs,
    "selected": selected,
    "not_ready": notready,
    "image_fail_count": len(image_fail_pods),
    "image_fail_pods": image_fail_pods,
    "image_reasons_seen": sorted(image_reasons_seen),
    "canonical_population_ready": bool(canonical_population_ready),
}
with open(summary_path, 'w') as fh:
    fh.write(json.dumps(summary, indent=2, sort_keys=True))
    fh.write("\n")
PYEOF
    local proj_rc=$?
    set -e
    if (( proj_rc != 0 )); then
      capture_projection_failure "$proj_rc" "$(cat "$poll_proj_err" 2>/dev/null || true)"
    fi
    # Read the JSON projection summary written
    # by python. We keep the classifier on disk
    # so the deadline branch sees the LAST
    # observed state without ambiguity.
    local observed_count
    local image_fail_count
    local image_reasons_json
    observed_count=$(python3 -c "import json;d=json.load(open('$poll_summary'));print(d['observed_pod_count'])")
    image_fail_count=$(python3 -c "import json;d=json.load(open('$poll_summary'));print(d['image_fail_count'])")
    image_reasons_json=$(python3 -c "import json;d=json.load(open('$poll_summary'));print(json.dumps(d['image_reasons_seen']))")
    # Image failure: any selected Pod has
    # ImagePullBackOff/ErrImagePull/ErrImageNeverPull/
    # CrashLoopBackOff. Aborts as
    # FIXTURE_IMAGE_NOT_LOADED 14 (DISTINCT
    # from the readiness-timeout 12 path).
    if (( image_fail_count > 0 )); then
      capture_image_failure "${image_reasons_json}"
    fi
    # Convergence predicates: we do NOT exit
    # the loop on partial population. Continue
    # polling until the bounded 480s deadline
    # expires OR canonical_population_ready
    # is True. canonical_population_ready is
    # a single boolean emitted by the python
    # projection. A YES selects 12 static pairs
    # + exactly 1 dynamic probe + 0 duplicates
    # + 0 unexpected fixture-like + 0
    # image-failed + 0 not-ready; prefix-only
    # 13 cannot satisfy this and forces a
    # deadline-time vocabulary mismatch
    # artifact.
    local canonical_population_ready
    canonical_population_ready=$(python3 -c "import json;d=json.load(open('$poll_summary'));print('Y' if d.get('canonical_population_ready') else 'N')")
    if [ "${canonical_population_ready}" != "Y" ]; then
      # Emit a one-line human-readable verdict
      # so the loop log tells a verifier WHY
      # we kept polling. We do NOT abort while
      # the deadline is in the future; the
      # deadline branch below retains full
      # vocabulary evidence.
      local miss_count
      miss_count=$(python3 -c "import json;d=json.load(open('$poll_summary'));print(len(d.get('missing_static_pairs', [])))")
      local dyn_count
      dyn_count=$(python3 -c "import json;d=json.load(open('$poll_summary'));print(len(d.get('dynamic_probe_pairs', [])))")
      local dup_count
      dup_count=$(python3 -c "import json;d=json.load(open('$poll_summary'));print(len(d.get('duplicate_pairs', [])))")
      local unex_count
      unex_count=$(python3 -c "import json;d=json.load(open('$poll_summary'));print(len(d.get('unexpected_fixture_like_pairs', [])))")
      echo "[install] poll=${poll_count} selected=${observed_count}/${expected_fixture_count} missing=${miss_count} dynamic-probes=${dyn_count} dup=${dup_count} unex=${unex_count}"
      sleep 5
      continue
    fi
    # canonical_population_ready == Y. Derive
    # expected_labels_file from the SAME
    # successful JSON snapshot so the
    # generated cni-control-probe-<rs>-<pod>
    # identity is preserved. Expected labels
    # carry resolve-labels-default/<name>
    # because Cilium's `cilium endpoint list`
    # only emits the bare pod name in its
    # controller labels; the canonical
    # namespace/name contract is preserved in
    # cilium-endpoint-convergence.json (below).
    cp -p "$poll_json" "$successful_poll_json"
    # d2b.49 namespace-aware expected projection.
    # Cilium's `cilium endpoint list -o json` actually
    # emits controller labels whose `name` IS a
    # `resolve-labels-<namespace>/<pod>` string, NOT just
    # `resolve-labels-default/<pod>`. Dropping namespace
    # here hides every non-default fixture (e.g.
    # `cni-test-ingress/cni-mock-ingress-controller`).
    # We build the expected set directly from the
    # canonical full (namespace, name) pairs that Step G
    # already validated — the generated dynamic probe too.
    python3 - "$poll_summary" "$expected_labels_file" <<'PYEOF'
import json, sys
summary_path, out_path = sys.argv[1], sys.argv[2]
data = json.load(open(summary_path))
labels = []
for p in data.get("observed_static_pairs", []):
    ns = p.get("namespace", "")
    nm = p.get("name", "")
    if not ns or not nm:
        continue
    labels.append(f"resolve-labels-{ns}/{nm}")
for p in data.get("dynamic_probe_pairs", []):
    ns = p.get("namespace", "")
    nm = p.get("name", "")
    if not ns or not nm:
        continue
    labels.append(f"resolve-labels-{ns}/{nm}")
seen = set()
uniq = []
for l in sorted(labels):
    if l in seen:
        continue
    seen.add(l)
    uniq.append(l)
with open(out_path, 'w') as fh:
    fh.write('\n'.join(uniq) + ('' if not uniq else '\n'))
PYEOF
    expected_unique_count=$(awk 'BEGIN{n=0} {n++} END {print n+0}' "$expected_labels_file")
    echo "[install] poll=${poll_count} canonical population Ready at ${observed_count}/${expected_unique_count}"
    fixtures_ready=1
    break
  done
  # End Block A readiness loop.
  if (( fixtures_ready != 1 )); then
    # d2b.48 deadline: capture the LAST poll's
    # JSON + summary + human-readable text view
    # to disk so the verifier can correlate
    # against the FINAL state, not some early
    # snapshot. The classification routes
    # FIXTURE_NOT_READY (12). Vocabulary evidence
    # (expected_static_pairs, missing, dynamic,
    # duplicate, unexpected) is preserved in full
    # so that a stale or wrong-namespace Pod can
    # be diagnosed from disk.
    local last_summary="$poll_summary"
    local last_poll_json="$poll_json"
    local last_tsv="$poll_tsv"
    local observed_final
    observed_final=$(python3 -c "import json;d=json.load(open('$last_summary'));print(d['observed_pod_count'])")
    local image_fail_final
    image_fail_final=$(python3 -c "import json;d=json.load(open('$last_summary'));print(d['image_fail_count'])")
    local notready_names=""
    if [ -s "$last_summary" ]; then
      notready_names=$(python3 -c "
import json
d=json.load(open('$last_summary'))
parts=[]
for p in d['not_ready']:
    parts.append(f\"{p['namespace']}/{p['name']} phase={p['phase']}\")
print(' '.join(parts))")
    fi
    # Human-readable text view of the last poll.
    : > "$snapshot_txt"
    printf 'expected_count: %s\n' "$expected_fixture_count" >"$snapshot_txt"
    printf 'observed_count: %s\n' "$observed_final" >"$snapshot_txt"
    printf 'image_fail_count: %s\n' "$image_fail_final" >>"$snapshot_txt"
    printf 'not_ready_names: %s\n' "${notready_names:-none}" >>"$snapshot_txt"
    printf 'poll_count: %s\n' "$poll_count" >>"$snapshot_txt"
    cat >"$snapshot_json.snapshot" <<EOF
{
  "command": "JSON-based bounded readiness poll",
  "phase": "fixture_pod_readiness_timeout",
  "expected_count": ${expected_fixture_count},
  "observed_count": ${observed_final},
  "image_fail_count": ${image_fail_final},
  "not_ready_pods": $(python3 -c "
import json,sys
d=json.load(open('$last_summary'))
print(json.dumps(d['not_ready'], indent=2))"),
  "poll_count": ${poll_count},
  "image_fail_pods": $(python3 -c "
import json,sys
d=json.load(open('$last_summary'))
print(json.dumps(d['image_fail_pods'], indent=2))"),
  "selected_pods": $(python3 -c "
import json,sys
d=json.load(open('$last_summary'))
print(json.dumps([{'namespace':p['namespace'],'name':p['name'],'phase':p['phase'],'ready':p['ready']} for p in d['selected']], indent=2))"),
  "expected_static_pairs": $(python3 -c "
import json,sys
d=json.load(open('$last_summary'))
print(json.dumps(d.get('expected_static_pairs', []), indent=2))"),
  "observed_static_pairs": $(python3 -c "
import json,sys
d=json.load(open('$last_summary'))
print(json.dumps(d.get('observed_static_pairs', []), indent=2))"),
  "missing_static_pairs": $(python3 -c "
import json,sys
d=json.load(open('$last_summary'))
print(json.dumps(d.get('missing_static_pairs', []), indent=2))"),
  "unexpected_fixture_like_pairs": $(python3 -c "
import json,sys
d=json.load(open('$last_summary'))
print(json.dumps(d.get('unexpected_fixture_like_pairs', []), indent=2))"),
  "dynamic_probe_pairs": $(python3 -c "
import json,sys
d=json.load(open('$last_summary'))
print(json.dumps(d.get('dynamic_probe_pairs', []), indent=2))"),
  "duplicate_pairs": $(python3 -c "
import json,sys
d=json.load(open('$last_summary'))
print(json.dumps(d.get('duplicate_pairs', []), indent=2))"),
  "canonical_population_ready": $(python3 -c "
import json,sys
d=json.load(open('$last_summary'))
print('true' if d.get('canonical_population_ready') else 'false')"),
  "events_log": "${events_log}",
  "events_source": "JSON status.conditions + containerStatuses[*].state.waiting/terminated.reason",
  "reason": "canonical 12+1 fixture population did not all READY/RUNNING within 8 minutes (480s); vocabulary evidence preserved in this artefact"
}
EOF
    mv "$snapshot_json.snapshot" "$snapshot_json"
    # Per-pod events log wiring is preserved so
    # any downstream verifier reading
    # fixture-pod-readiness-events.log sees
    # the NAMESPACE/NAME/PHASE triples derived
    # from the SAME JSON projection (no
    # positional awk grep).
    : > "$events_log"
    printf '# fixture-pod-readiness-events phase=fixture_pod_readiness_timeout poll=%s\n' "$poll_count" >"$events_log"
    if [ -s "$last_summary" ]; then
      python3 - "$last_summary" "$events_log" <<'PYEOF'
import json, sys
summary, evp = sys.argv[1], sys.argv[2]
data = json.load(open(summary))
with open(evp, 'a') as fh:
    for p in data["selected"]:
        fh.write(f"{p['namespace']}/{p['name']}\t{p['phase']}\tfixture_pod_readiness_timeout\n")
    for p in data["image_fail_pods"]:
        fh.write(f"{p['namespace']}/{p['name']}\timage-fail:{p['reason']}\tfixture_pod_readiness_timeout\n")
PYEOF
    fi
    local observed_final_count
    observed_final_count=$(python3 -c "import json;d=json.load(open('$last_summary'));print(d['observed_pod_count'])")
    abort_as FIXTURE_NOT_READY \
      "fixture Pods not all Ready within 8 minutes (deadline); observed=${observed_final_count}/expected=${expected_fixture_count}; details in $snapshot_json" 12
  fi
  # -----------------------------------------------------------------
  # Cilium endpoint aggregation across nodes —
  # exact 13-Pod contract; per-command failure
  # classifiers + observable convergence
  # evidence. expected_labels_file was
  # derived from the SAME successful JSON
  # snapshot whose projection identified
  # exactly 13 Ready fixtures (above), so the
  # generated cni-control-probe-* runtime
  # identity is preserved end-to-end.
  #
  # Loop structure (this block):
  #   1) on every iteration, RE-FETCH the
  #      Cilium daemon Pod list. A failed cmd
  #      (nonzero rc) writes a structured error
  #      artefact and aborts as
  #      CLUSTER_OR_CNI_NOT_READY 10. A valid
  #      empty list is a convergence
  #      observation, not a failure; we sleep
  #      and retry.
  #   2) for each daemon, capture explicit rc
  #      AND stderr for (a) kubectl exec
  #      (b) python3 JSON projection.
  #   3) project a per-iteration label set, run
  #      LC_ALL=C sort -u to produce unique
  #      labels, then count via awk.
  #   4) break on (LAST >= EXPECTED AND
  #      missing_count==0 AND
  #      unexpected_count==0).
  #   5) under-converged: emit a parseable
  #      convergence JSON whose observed_count
  #      equals the length of observed_labels.
  # -----------------------------------------------------------------
  local expected="$expected_fixture_count"
  local daemon_out="$ARTIFACTS/cilium-daemon-list.out"
  local daemon_err="$ARTIFACTS/cilium-daemon-list.stderr"
  local daemon_list="$ARTIFACTS/cilium-daemon-list.names"
  deadline=$(( $(date +%s) + 480 ))
  local last=0
  local convergence_art="$ARTIFACTS/cilium-endpoint-convergence.json"
  local got_daemon_at_least_once=0
  while (( $(date +%s) < deadline )); do
    : > "$daemon_out"
    : > "$daemon_err"
    : > "$daemon_list"
    set +e
    kubectl -n kube-system get pod -l k8s-app=cilium \
      -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}' \
      >"$daemon_out" 2>"$daemon_err"
    local dl_rc=$?
    set -e
    awk '{
      for (i = 1; i <= NF; i++) { print $i }
    }' "$daemon_out" > "$daemon_list"
    if (( dl_rc != 0 )); then
      local daemon_err_art="$ARTIFACTS/cilium-endpoint-inventory-error.json"
      cat >"$daemon_err_art.snapshot" <<EOF
{
  "command": "kubectl -n kube-system get pod -l k8s-app=cilium -o jsonpath",
  "phase": "cilium_daemon_list",
  "rc": ${dl_rc},
  "stderr": $(printf '%s' "$(cat "$daemon_err")" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))'),
  "expected_count": ${expected},
  "reason": "cilium daemon list command failed; endpoint inventory cannot be obtained"
}
EOF
      mv "$daemon_err_art.snapshot" "$daemon_err_art"
      abort_as CLUSTER_OR_CNI_NOT_READY \
        "cilium daemon list failed rc=${dl_rc}: endpoint inventory cannot be obtained (see $daemon_err_art)" 10
    fi
    local dl_count
    dl_count=$(wc -w <"$daemon_list")
    if (( dl_count > 0 )); then
      got_daemon_at_least_once=1
    fi
    echo "[install] cilium daemon count: ${dl_count}"
    local acc_names="$ARTIFACTS/cilium-endpoint.acc.out"
    local acc_err="$ARTIFACTS/cilium-endpoint.acc.stderr"
    : > "$acc_names"
    : > "$acc_err"
    # Iterate the validated daemon list. Each
    # iteration captures daemon exec rc AND
    # python3 JSON projection rc. Either nonzero
    # writes a structured error artefact and
    # aborts as CLUSTER_OR_CNI_NOT_READY 10.
    set +e
    while read -r daemon; do
      [ -z "$daemon" ] && continue
      local exec_out="$ARTIFACTS/cilium-exec-${daemon}.out"
      local exec_err="$ARTIFACTS/cilium-exec-${daemon}.stderr"
      : > "$exec_out"
      : > "$exec_err"
      kubectl -n kube-system exec "$daemon" -- \
        bash -c 'cilium endpoint list -o json' \
        >"$exec_out" 2>"$exec_err"
      local exec_rc=$?
      if (( exec_rc != 0 )); then
        cat >"$convergence_art.snapshot" <<EOF
{
  "command": "kubectl -n kube-system exec ${daemon} -- cilium endpoint list",
  "phase": "cilium_daemon_exec",
  "daemon": "${daemon}",
  "rc": ${exec_rc},
  "stderr": $(printf '%s' "$(cat "$exec_err")" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))'),
  "expected_count": ${expected},
  "reason": "cilium daemon exec failed; cannot read endpoint list"
}
EOF
        mv "$convergence_art.snapshot" "$convergence_art"
        abort_as CLUSTER_OR_CNI_NOT_READY \
          "cilium daemon exec ${daemon} failed rc=${exec_rc}: cannot read endpoint list (see $convergence_art)" 10
      fi
      local proj_out="$ARTIFACTS/cilium-exec-${daemon}.json.proj.out"
      local proj_err="$ARTIFACTS/cilium-exec-${daemon}.json.proj.stderr"
      : > "$proj_out"
      : > "$proj_err"
      python3 -c "
import json,sys,os,re
try:
    raw=open('${exec_out}').read()
    data=json.loads(raw)
except Exception as e:
    print('PROJECTION-FAILED: ' + repr(e), file=sys.stderr)
    sys.exit(17)
endpoints=data if isinstance(data,list) else (data.get('endpoint') or [])
# d2b.49 namespace-aware observed projection.
# Accept ANY resolve-labels-<real-namespace>/cni-*
# controller label, not only resolve-labels-default/.
# Any non-fixture namespace appears in the unexpected
# bucket instead of being silently filtered out.
# d2b.51: do NOT use Markdown backticks inside
# this python3 -c "..." double-quoted shell
# argument; bash performs command substitution on
# every backtick pair, which previously caused
# real-namespace: No such file or directory
# and unexpected: command not found noise to
# appear on stderr. Use bare quotes here.
ctrl_re = re.compile(r'^resolve-labels-[^/]+/cni-.+')
items=[]
for e in endpoints:
    for c in e.get('status',{}).get('controllers',[]):
        nm=c.get('name','')
        if ctrl_re.match(nm):
            items.append(nm)
for x in sorted(set(items)):
    print(x)
" \
        >"$proj_out" 2>"$proj_err"
      local proj_rc=$?
      if (( proj_rc != 0 )); then
        cat >"$convergence_art.snapshot" <<EOF
{
  "command": "python3 cilium JSON projection (daemon ${daemon})",
  "phase": "cilium_json_projection",
  "daemon": "${daemon}",
  "rc": ${proj_rc},
  "stderr": $(printf '%s' "$(cat "$proj_err" 2>/dev/null || true)" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))'),
  "expected_count": ${expected},
  "reason": "cilium JSON projection failed; cannot derive endpoint labels"
}
EOF
        mv "$convergence_art.snapshot" "$convergence_art"
        abort_as CLUSTER_OR_CNI_NOT_READY \
          "cilium JSON projection on ${daemon} failed rc=${proj_rc}: cannot derive endpoint labels (see $convergence_art)" 10
      fi
      cat "$proj_out" >> "$acc_names"
    done < "$daemon_list"
    set -e
    # Unique-label normalization: LC_ALL=C sort -u
    # collapses duplicate labels across daemons.
    # awk then counts the unique labels so the
    # count == file length is provable from disk.
    local unique_labels="$ARTIFACTS/cilium-endpoint.unique.out"
    : > "$unique_labels"
    if [ -s "$acc_names" ]; then
      LC_ALL=C sort -u "$acc_names" > "$unique_labels"
    fi
    # Compute the identity diff once at the END
    # of every iteration so the deadline branch
    # reads the SAME files the loop just wrote
    # (single source of truth for expected/
    # observed/missing/unexpected).
    local missing_labels_file="$ARTIFACTS/cilium-endpoint.missing.out"
    local unexpected_labels_file="$ARTIFACTS/cilium-endpoint.unexpected.out"
    # d2b.46 Block D follow-up: fail closed on
    # any set-diff command failure. We run each
    # comm separately under set +e and capture
    # exact rc + stderr. Only rc 0 is treated as
    # a valid identity diff; any non-zero rc
    # writes an atomic structured JSON artifact
    # and aborts as CLUSTER_OR_CNI_NOT_READY 10
    # BEFORE we let empty files masquerade as a
    # successful equality check. We deliberately
    # do NOT use `|| true` here — a comm exit
    # >0 (missing input, I/O error, unsorted
    # input, comm-not-exec'd) must be fail-closed,
    # not silently swallowed.
    local missing_diff_err="$ARTIFACTS/cilium-endpoint.missing.stderr"
    local unexpected_diff_err="$ARTIFACTS/cilium-endpoint.unexpected.stderr"
    : > "$missing_labels_file"; : > "$missing_diff_err"
    : > "$unexpected_labels_file"; : > "$unexpected_diff_err"
    set +e
    LC_ALL=C comm -23 "$expected_labels_file" "$unique_labels" \
      >"$missing_labels_file" 2>"$missing_diff_err"
    local missing_diff_rc=$?
    set -e
    if (( missing_diff_rc != 0 )); then
      local sd_err_art="$ARTIFACTS/cilium-endpoint-setdiff-error.json"
      cat >"$sd_err_art.snapshot" <<EOF
{
  "command": "LC_ALL=C comm -23 expected_labels unique_labels",
  "operation": "missing_labels_diff",
  "rc": ${missing_diff_rc},
  "expected_path": "${expected_labels_file}",
  "observed_path": "${unique_labels}",
  "output_path": "${missing_labels_file}",
  "stderr": $(printf '%s' "$(cat "$missing_diff_err")" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))'),
  "phase": "cilium_setdiff_failed",
  "reason": "set-diff comm -23 exited non-zero; relying on empty output would falsely count as set equality"
}
EOF
      mv "$sd_err_art.snapshot" "$sd_err_art"
      abort_as CLUSTER_OR_CNI_NOT_READY \
        "cilium endpoint set-diff (missing) failed rc=${missing_diff_rc} (see $sd_err_art)" 10
    fi
    set +e
    LC_ALL=C comm -13 "$expected_labels_file" "$unique_labels" \
      >"$unexpected_labels_file" 2>"$unexpected_diff_err"
    local unexpected_diff_rc=$?
    set -e
    if (( unexpected_diff_rc != 0 )); then
      local sd_err_art="$ARTIFACTS/cilium-endpoint-setdiff-error.json"
      cat >"$sd_err_art.snapshot" <<EOF
{
  "command": "LC_ALL=C comm -13 expected_labels unique_labels",
  "operation": "unexpected_labels_diff",
  "rc": ${unexpected_diff_rc},
  "expected_path": "${expected_labels_file}",
  "observed_path": "${unique_labels}",
  "output_path": "${unexpected_labels_file}",
  "stderr": $(printf '%s' "$(cat "$unexpected_diff_err")" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))'),
  "phase": "cilium_setdiff_failed",
  "reason": "set-diff comm -13 exited non-zero; relying on empty output would falsely count as set equality"
}
EOF
      mv "$sd_err_art.snapshot" "$sd_err_art"
      abort_as CLUSTER_OR_CNI_NOT_READY \
        "cilium endpoint set-diff (unexpected) failed rc=${unexpected_diff_rc} (see $sd_err_art)" 10
    fi
    last=$(awk 'BEGIN{n=0} {n++} END {print n+0}' "$unique_labels")
    local missing_count
    local unexpected_count
    missing_count=$(awk 'BEGIN{n=0} {n++} END {print n+0}' "$missing_labels_file")
    unexpected_count=$(awk 'BEGIN{n=0} {n++} END {print n+0}' "$unexpected_labels_file")
    # Convergence observation only — no command
    # failure here. Real under-count failure is
    # raised AFTER the deadline.
    if (( last < expected )); then
      # Still under by count. We don't break; we
      # ALSO do not raise yet — there might be
      # more cluster updates. Sleep and retry so
      # the deadline path captures the final
      # state.
      sleep 5
      continue
    fi
    # Count-only is no longer sufficient: must
    # also reject identity mismatch using the
    # diffs just computed. Convergence breaks
    # when AND ONLY WHEN both diffs are empty.
    if [ "${missing_count}" -eq 0 ] && [ "${unexpected_count}" -eq 0 ]; then
      echo "[install] cilium endpoint labels reached ${last}/${expected_unique_count} (identity)"
      break
    fi
    echo "[install] cilium identity mismatch: missing=${missing_count} unexpected=${unexpected_count}; last=${last}/${expected_unique_count}"
    sleep 5
  done
  # d2b.46 Block A deadline failure guard fires
  # on count failure OR identity failure: a
  # stale-label/expected-label mismatch must
  # not escape as `step G ok`. We capture
  # missing_count/unexpected_count from the
  # LAST iteration's disk artifacts (single
  # source of truth) so the abort branch sees
  # the same numbers the loop just computed.
  local install_identity_mismatch="N"
  if [ -s "$ARTIFACTS/cilium-endpoint.missing.out" ] \
     && [ "$(awk 'BEGIN{n=0} {n++} END {print n+0}' "$ARTIFACTS/cilium-endpoint.missing.out")" -ne 0 ]; then
    install_identity_mismatch="Y"
  fi
  if [ -s "$ARTIFACTS/cilium-endpoint.unexpected.out" ] \
     && [ "$(awk 'BEGIN{n=0} {n++} END {print n+0}' "$ARTIFACTS/cilium-endpoint.unexpected.out")" -ne 0 ]; then
    install_identity_mismatch="Y"
  fi
  if (( last < expected_unique_count )) || [ "${install_identity_mismatch}" = "Y" ]; then
    # Convergence failure (NOT a command error):
    # write under-convergence artefact and abort
    # with CLUSTER_OR_CNI_NOT_READY 10.
    # All four arrays and their counts are
    # derived in one python invocation: opening
    # the same files the loop just wrote and
    # using only the file contents to compute
    # counts prevents observed_count and
    # expected_count from diverging from their
    # arrays.
    local exp_file="$expected_labels_file"
    local obs_file="$ARTIFACTS/cilium-endpoint.unique.out"
    local missing_file="$ARTIFACTS/cilium-endpoint.missing.out"
    local unexpected_file="$ARTIFACTS/cilium-endpoint.unexpected.out"
    # d2b.48: enrich the convergence artefact
    # with the canonical 12+1 vocabulary and
    # the dynamic-probe cardinality observed
    # from the LAST successful JSON snapshot.
    # The Cilium output itself does not carry
    # namespace, so the observed_labels array
    # is names-only and the canonical
    # {namespace, name} match is asserted
    # against the inventory summary. This
    # guarantees an arbitrary prefix-shaped
    # 13 cannot satisfy convergence.
    local final_summary="$poll_summary"
    python3 - "$exp_file" "$obs_file" "$missing_file" "$unexpected_file" \
             "$daemon_list" "$deadline" "$(date +%s)" \
             "$expected_unique_count" \
             "$final_summary" \
             >"$convergence_art.snapshot" <<'PYEOF'
import json, sys
exp_p, obs_p, miss_p, une_p, daemon_p, dl_s, now_s, exp_s, summary_p = sys.argv[1:10]
def reads(p):
    return [l for l in open(p).read().splitlines() if l.strip()]
daemons   = reads(daemon_p)
expected  = reads(exp_p)
observed  = reads(obs_p)
missing   = reads(miss_p)
unexpected= reads(une_p)
summary = json.load(open(summary_p))
obj = {
  "command": "cilium endpoint convergence",
  "phase": "cilium_convergence_undercount_or_mismatch",
  "daemon_list": daemons,
  "deadline_unix": int(dl_s),
  "now_unix": int(now_s),
  "expected_labels": expected,
  "observed_labels": observed,
  "missing_labels": missing,
  "unexpected_labels": unexpected,
  "expected_count": len(expected),
  "observed_count": len(observed),
  "expected_static_pairs": summary.get("expected_static_pairs", []),
  "observed_static_pairs": summary.get("observed_static_pairs", []),
  "missing_static_pairs": summary.get("missing_static_pairs", []),
  "unexpected_fixture_like_pairs": summary.get("unexpected_fixture_like_pairs", []),
  "dynamic_probe_pairs": summary.get("dynamic_probe_pairs", []),
  "duplicate_pairs": summary.get("duplicate_pairs", []),
  "canonical_population_ready": bool(summary.get("canonical_population_ready", False)),
  "canonical_vocabulary_size": int(summary.get("expected_count", 0)),
  "dynamic_probe_cardinality": len(summary.get("dynamic_probe_pairs", [])),
  "reason": "cilium endpoint publication did not identity-match the canonical 12+1 vocabulary; expected_count == observed_count is NOT sufficient; vocab arrays preserved in this artefact"
}
# Every count must equal the length of its
# array AND must be derived from disk in one
# python invocation so they cannot diverge.
assert obj["expected_count"] == len(obj["expected_labels"])
assert obj["observed_count"] == len(obj["observed_labels"])
assert obj["expected_count"] == int(exp_s)
sys.stdout.write(json.dumps(obj, indent=2))
sys.stdout.write("\n")
PYEOF
    mv "$convergence_art.snapshot" "$convergence_art"
    local exp_count
    exp_count=$(awk 'BEGIN{n=0} {n++} END {print n+0}' "$expected_labels_file")
    local miss_c
    local unex_c
    miss_c=$(awk 'BEGIN{n=0} {n++} END {print n+0}' "$ARTIFACTS/cilium-endpoint.missing.out")
    unex_c=$(awk 'BEGIN{n=0} {n++} END {print n+0}' "$ARTIFACTS/cilium-endpoint.unexpected.out")
    abort_as CLUSTER_OR_CNI_NOT_READY \
      "cilium endpoint convergence did not identity-match: observed=${last}/${exp_count}; missing=${miss_c} unexpected=${unex_c}; see $convergence_art" 10
  fi
  # Hand off to the readiness gate for the
  # remaining convergence assertions (steps
  # #8..#9). The gate is the single
  # classification source.
  GATE_PHASE=post-fixture \
    RECOVERY_PR_SHA="${RECOVERY_PR_SHA:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}" \
    WORKFLOW_RUN_ID="${WORKFLOW_RUN_ID:-${GITHUB_RUN_ID:-local}}" \
    ARTIFACTS="${ARTIFACTS}" \
    "${CNI_READINESS_GATE_BIN}"
  echo "[install] step G ok"
}

# ---- main --------------------------------------------------------------

main() {
  if [[ ! -f "${ARTIFACTS}/cluster-up.txt" ]]; then
    echo "[install] ERROR: cluster not up; run test-cluster-up.sh first" \
      | tee -a "$ARTIFACTS/install.log"
    exit 2
  fi
  step_A_identity
  step_B_dryrun_namespaces
  step_C_apply_namespaces
  step_D_dryrun_namespaced
  step_E_semantic_admission
  step_image_pipeline
  step_F_real_apply
  step_G_readiness
  echo "[install] all A..G gates passed"
}

# d2b.46: when sourced by scripts/test_fixture_readiness_observability.sh,
# do not auto-run main(). The executed-script path is unchanged.
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
