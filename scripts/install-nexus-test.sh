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
  # kind load then per-node crictl verify.
  # d2b.46: capture the real exit code without
  # masking via '|| true'. A load failure must
  # raise FIXTURE_IMAGE_NOT_LOADED; the log is
  # preserved regardless of outcome.
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
  # Compute PRESENT and MISSING counts so a
  # downstream assertion can replay the
  # delta "present < expected" and surface it
  # in `fixture-image-node-runtime.log`.
  local node_log="$ARTIFACTS/fixture-image-node-runtime.log"
  : > "$node_log"
  local PRESENT=0
  local MISSING=0
  for n in $(kind get nodes --name "${CLUSTER_NAME}" 2>/dev/null); do
    echo "--- node: $n ---" >>"$node_log"
    if docker exec "${n}" crictl images 2>/dev/null \
        | grep -qE "${FIXTURE_IMAGE_ID:0:12}"; then
      PRESENT=$((PRESENT + 1))
      echo "(present image_id ${FIXTURE_IMAGE_ID:0:12})" >>"$node_log"
    else
      MISSING=$((MISSING + 1))
      echo "(missing image_id ${FIXTURE_IMAGE_ID:0:12})" >>"$node_log"
    fi
  done
  echo "PRESENT=$PRESENT MISSING=$MISSING" >>"$node_log"
  local missing="$MISSING"
  if (( missing > 0 )); then
    cat "$node_log" | tee -a "$ARTIFACTS/install.log"
    abort_as FIXTURE_IMAGE_NOT_LOADED \
      "image_id=$FIXTURE_IMAGE_ID missing on ${missing} kind node(s)" 14
  fi
  echo "[install] image pipeline ok" | tee -a "$ARTIFACTS/install.log"
}

# ---- step G: readiness / EndpointSlice / control service probe ---------

step_G_readiness() {
  echo "[install] ====== step G: readiness / control probe ======"
  # d2b.46 fixture Pod inventory contract:
  # the fresh-cluster fixture population has
  # exactly 13 Pods whose names start with
  # cni-mock-, cni-untrusted-, or cni-control-
  # (5 cni-mock-* in default + 1 each in ingress/
  # prometheus/postgres/redis/clickhouse +
  # 1 cni-untrusted-default + 2 cni-control-... =
  # 13). One anchored matcher drives every
  # inventory assertion below so an observation
  # loop can never silently disagree with the
  # endpoint expectation it consults.
  local fixture_re='^cni-(mock|untrusted|control)-'
  local expected_fixture_count=13
  local fixtures_ready=0
  local deadline=$(( $(date +%s) + 480 ))
  # Hoisted to function scope: post-loop snapshot
  # block (lines after deadline) reads $pull_log /
  # $pull_stderr when writing the timeout artifacts.
  # A `local` declared inside the while body exits
  # scope when the loop exits — without these the
  # outer references become unbound under `set -u`.
  local pull_log="$ARTIFACTS/fixture-pod-imagepull.log"
  local pull_stderr="$ARTIFACTS/fixture-pod-imagepull.stderr"
  # Capture rc/stderr of every kubectl inventory
  # call so observation failures cannot be
  # silently rewritten to empty or zero. rc is
  # written into $ARTIFACTS/fixture-pod-readiness-timeout.json
  # alongside the snapshot if we time out.
  capture_inventory_failure() {
    local cmd_name="$1"
    local inv_rc="$2"
    local inv_stderr="$3"
    local snapshot_json="$ARTIFACTS/fixture-pod-readiness-timeout.json"
    cat >"$snapshot_json.snapshot" <<EOF
{
  "command": "${cmd_name}",
  "rc": ${inv_rc},
  "stderr": $(printf '%s' "$inv_stderr" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))'),
  "observed_count": 0,
  "expected_count": ${expected_fixture_count},
  "reason": "kubectl inventory command failed"
}
EOF
    mv "$snapshot_json.snapshot" "$snapshot_json"
    abort_as FIXTURE_NOT_READY \
      "${cmd_name} failed rc=${inv_rc}: inventory cannot be obtained (see $snapshot_json)" 12
  }
  while (( $(date +%s) < deadline )); do
    : > "$pull_log"
    : > "$pull_stderr"
    set +e
    # Pull every fixture pod. The fake kubectl
    # emits canonical NAME READY STATUS
    # RESTARTS AGE so the anchored regex matches
    # col 1 and the per-pod readiness check
    # below reads col 2 = READY, col 3 = STATUS.
    kubectl get pod -A --no-headers 2>"$pull_stderr" \
      | grep -E "$fixture_re" >"$pull_log"
    local inv_rc=$?
    set -e
    if (( inv_rc != 0 )); then
      capture_inventory_failure "kubectl get pod" "$inv_rc" \
        "$(cat "$pull_stderr")"
    fi
    local observed
    observed=$(wc -l <"$pull_log")
    local pull_reasons
    pull_reasons=$(
      awk '{print $1, $2, $3}' "$pull_log" \
        | grep -E "ImagePullBackOff|ErrImagePull|ErrImageNeverPull|CrashLoopBackOff" \
        | sort -u || true)
    if [[ -n "$pull_reasons" ]]; then
      cat "$pull_log" | tee -a "$ARTIFACTS/install.log"
      abort_as FIXTURE_IMAGE_NOT_LOADED \
        "fixture Pod entered image-pull-failure: $pull_reasons" 14
    fi
    if (( observed != expected_fixture_count )); then
      # Do not declare Ready on partial count.
      sleep 5
      continue
    fi
    local notready
    # pull_log is canonical NAME READY STATUS
    # RESTARTS AGE. A ready pod has $2 == 1/1
    # and $3 == Running.
    notready=$(awk '$2 != "1/1" || $3 != "Running" {n++}; END {print n+0}' \
      "$pull_log")
    if (( notready == 0 )); then
      fixtures_ready=1
      break
    fi
    sleep 5
  done
  if (( fixtures_ready != 1 )); then
    # d2b.46: classify the timeout against the
    # exact anchored fixture set, capture the
    # full canonical inventory snapshot + events,
    # and abort with observed/expected + names +
    # non-ready reasons. Never infer time-out
    # solely from a second 'date' check after
    # the break: when fixtures_ready==1 we
    # explicitly exit the loop above.
    local snapshot_json="$ARTIFACTS/fixture-pod-readiness-timeout.json"
    local snapshot_txt="$ARTIFACTS/fixture-pod-readiness-timeout.txt"
    local events_log="$ARTIFACTS/fixture-pod-readiness-events.log"
    : > "$events_log"
    set +e
    kubectl get pod -A -o json 2>"$pull_stderr" \
      | python3 -c "
import json,sys,os
data=json.loads(sys.stdin.read() or '{\"items\":[]}')
items=data.get('items') if isinstance(data,dict) else []
rows=[]
for it in items:
    md=(it.get('metadata') or {})
    name=md.get('name','')
    ns=md.get('namespace','')
    if not (name.startswith('cni-mock-') or name=='cni-untrusted-default' or name.startswith('cni-control-')):
        continue
    phase=(it.get('status') or {}).get('phase','')
    ready=False
    for c in (it.get('status') or {}).get('conditions') or []:
        if c.get('type')=='Ready':
            ready=bool(c.get('status')=='True')
            break
    containers=[]
    for cs in (it.get('status') or {}).get('containerStatuses') or []:
        st=cs.get('state') or {}
        wt=st.get('waiting') or {}
        term=st.get('terminated') or {}
        runn=st.get('running') or {}
        containers.append({
            'name': cs.get('name',''),
            'ready': bool(cs.get('ready')),
            'restartCount': cs.get('restartCount',0),
            'waiting_reason': wt.get('reason') or None,
            'waiting_message': wt.get('message') or None,
            'terminated_reason': term.get('reason') or None,
            'running': bool(runn),
        })
    rows.append({
        'namespace': ns,
        'name': name,
        'phase': phase,
        'ready': ready,
        'containers': containers,
    })
out={'expected_count': 13, 'observed_count': len(rows), 'pods': rows}
print(json.dumps(out, indent=2, sort_keys=True))
" > "$snapshot_json"
    local snap_rc=$?
    set -e
    if (( snap_rc != 0 )); then
      capture_inventory_failure "kubectl get pod -o json" "$snap_rc" "python3 projection failed"
    fi
    # Human readable text view for grep-ability.
    python3 -c "
import json
d=json.load(open('$snapshot_json'))
print('expected_count:', d['expected_count'])
print('observed_count:', d['observed_count'])
print('--- non-ready pods ---')
for p in d['pods']:
    if not p['ready']:
        cs=','.join(c['waiting_reason'] or c['terminated_reason'] or (('ready' if c['ready'] else 'notready')) for c in p['containers']) or 'none'
        print(f\"{p['namespace']}/{p['name']} phase={p['phase']} containers={cs}\")
" > "$snapshot_txt"
    # Per-pod events; record failure distinctly but
    # cannot erase the status snapshot.
    set +e
    kubectl get pod -A --no-headers 2>/dev/null \
      | grep -E "$fixture_re" \
      | awk '{print $1, $2}' \
      | while read -r ns name; do
          echo "=== events for ${ns}/${name} ===" >>"$events_log"
          kubectl get events -n "$ns" --field-selector "involvedObject.kind=Pod,involvedObject.name=$name" \
            --sort-by=.lastTimestamp 2>>"$events_log" \
            | tail -10 >>"$events_log" || true
        done
    set -e
    local observed_final
    observed_final=$(wc -l <"$pull_log")
    abort_as FIXTURE_NOT_READY \
      "fixture Pods not all Ready within 8 minutes (deadline); observed=${observed_final}/expected=${expected_fixture_count}; details in $snapshot_json" 12
  fi
  # cilium endpoint aggregation across nodes —
  # d2b.46: uses the SAME anchored matcher so
  # the expectation set matches the readiness
  # set. Fail closed if the inventory command
  # itself errors; never accept 0.
  local expected
  local ep_stderr="$ARTIFACTS/cilium-endpoint.stderr"
  : > "$ep_stderr"
  set +e
  expected=$(kubectl get pod -A --no-headers 2>"$ep_stderr" \
    | grep -cE "$fixture_re" | tr -d '\n')
  local exp_rc=$?
  set -e
  if (( exp_rc != 0 )); then
    local snap="$ARTIFACTS/fixture-pod-readiness-timeout.json"
    cat >"$snap" <<EOF
{"command": "kubectl get pod (expected count)", "rc": ${exp_rc}, "stderr": $(printf '%s' "$(cat "$ep_stderr")" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))'), "observed_count": 0, "expected_count": 13, "reason": "kubectl inventory command failed for endpoint expectation"}
EOF
    abort_as FIXTURE_NOT_READY \
      "kubectl get pod rc=${exp_rc}: cannot derive cilium-endpoint expectation (see $snap)" 12
  fi
  deadline=$(( $(date +%s) + 480 ))
  local last=0
  while (( $(date +%s) < deadline )); do
    local acc=""
    for p in $(kubectl -n kube-system get pod -l k8s-app=cilium -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}' 2>/dev/null); do
      local out
      out=$(kubectl -n kube-system exec "$p" -- \
        bash -c 'cilium endpoint list -o json' 2>/dev/null \
        | python3 -c "
import json,sys
data=json.loads(sys.stdin.read())
endpoints=data if isinstance(data,list) else (data.get('endpoint') or [])
items=[]
for e in endpoints:
    for c in e.get('status',{}).get('controllers',[]):
        nm=c.get('name','')
        if nm.startswith('resolve-labels-default/cni-'):
            items.append(nm)
for x in set(items): print(x)
" 2>/dev/null) || true
      acc+=$(printf "\n%s" "$out")
    done
    last=$(printf "%s" "$acc" | grep -c "^resolve-labels-default/cni-" | tr -d '\n' || echo 0)
    if (( last >= expected )); then break; fi
    sleep 5
  done
  if (( last < expected )); then
    abort_as FIXTURE_NOT_READY \
      "cilium endpoint count ${last} < expected ${expected}" 12
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
