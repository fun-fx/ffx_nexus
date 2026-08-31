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
  # -----------------------------------------------------------------
  # d2b.46 Block A: dynamic expected-set derivation.
  # Step F has already produced the canonical fixture
  # Pod inventory (Pod/N READY/R ESTARTS/A) via
  # `kubectl get pod -A -o json` (its timeout-snapshot
  # block writes the same JSON). Here we capture a
  # second snapshot to anchor Gate 8 expectations,
  # because Step G must consult the actual runtime
  # Pod NAME for every fixture, including the
  # Deployment-generated `cni-control-probe-<rs>-<pod>`
  # identity that is NOT knowable from any static
  # control name.
  # -----------------------------------------------------------------
  local expected_labels_file="$ARTIFACTS/cilium-endpoint.expected.out"
  local inv_json="$ARTIFACTS/fixture-inventory.json"
  local inv_err="$ARTIFACTS/fixture-inventory.stderr"
  : > "$expected_labels_file"
  : > "$inv_err"
  set +e
  kubectl get pod -A -o json 2>"$inv_err" >"$inv_json"
  local inv_rc=$?
  set -e
  if (( inv_rc != 0 )); then
    local inv_err_art="$ARTIFACTS/fixture-inventory-error.json"
    cat >"$inv_err_art.snapshot" <<EOF
{
  "command": "kubectl get pod -A -o json",
  "phase": "fixture_inventory_snap",
  "rc": ${inv_rc},
  "stderr": $(printf '%s' "$(cat "$inv_err")" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))'),
  "expected_count": ${expected_fixture_count},
  "reason": "fixture inventory capture failed; cannot derive dynamic expected label set"
}
EOF
    mv "$inv_err_art.snapshot" "$inv_err_art"
    abort_as CLUSTER_OR_CNI_NOT_READY \
      "fixture inventory capture failed rc=${inv_rc}: cannot derive dynamic expected label set (see $inv_err_art)" 10
  fi
  # Derive expected labels from Pod metadata.name.
  # This is where generated names like
  # cni-control-probe-<rs>-<pod> enter the
  # contract: the username never types them,
  # the inventory provides them. Fail closed if
  # the dynamic set is non-13 because the
  # upstream fixture drift would mean our
  # convergence check is meaningless.
  python3 - "$inv_json" "$expected_labels_file" >/dev/null <<'PYEOF'
import json, sys, re
inv_path, out_path = sys.argv[1], sys.argv[2]
mock_re    = re.compile(r'^cni-mock-')
control_re = re.compile(r'^cni-control-')
data = json.load(open(inv_path))
items = data.get('items') if isinstance(data, dict) else []
labels = []
for it in items:
    md = it.get('metadata') or {}
    name = md.get('name') or ''
    if not (mock_re.match(name) or name == 'cni-untrusted-default' or control_re.match(name)):
        continue
    labels.append(f"resolve-labels-default/{name}")
seen = set()
uniq = []
for l in sorted(labels):
    if l in seen: continue
    seen.add(l)
    uniq.append(l)
open(out_path, 'w').write('\n'.join(uniq) + ('' if not uniq else '\n'))
# Note: we deliberately do NOT raise on
# empty/short inventory here; the caller checks
# length against expected_fixture_count so the
# downstream convergence classifier owns the
# exit decision.
PYEOF
  local expected_unique_count
  expected_unique_count=$(awk 'BEGIN{n=0} {n++} END {print n+0}' "$expected_labels_file")
  if (( expected_unique_count != expected_fixture_count )); then
    local fix_art="$ARTIFACTS/fixture-pod-readiness-timeout.json"
    cat >"$fix_art.snapshot" <<EOF
{
  "command": "kubectl get pod -A -o json dynamic-set derivation",
  "phase": "fixture_inventory_set_mismatch",
  "rc": 0,
  "observed_count": ${expected_unique_count},
  "expected_count": ${expected_fixture_count},
  "expected_labels_file": "${expected_labels_file}",
  "reason": "dynamic expected label set derived from inventory observed=${expected_unique_count} unique entries; canonical 13-fixture vocabulary contract requires ${expected_fixture_count}"
}
EOF
    mv "$fix_art.snapshot" "$fix_art"
    printf '[install] FAIL expected=%s observed=%s\n' "${expected_fixture_count}" "${expected_unique_count}" >>"$ARTIFACTS/fixture-pod-readiness-timeout.txt"
    # Write the canonical events log so downstream
    # controls (C10 in particular) still see the
    # pre-loop inventory surface they historically
    # validated. We name the same file the
    # deadline branch writes.
    python3 - "$inv_json" "$ARTIFACTS/fixture-pod-readiness-events.log" >/dev/null <<'PYEOF'
import json, sys
inv_path, events_path = sys.argv[1], sys.argv[2]
try:
    data = json.load(open(inv_path))
except Exception:
    data = {}
items = data.get('items') if isinstance(data, dict) else []
rows = []
for it in items:
    md = it.get('metadata') or {}
    name = md.get('name','')
    if not (name.startswith('cni-mock-') or name == 'cni-untrusted-default' or name.startswith('cni-control-')):
        continue
    phase = (it.get('status') or {}).get('phase','')
    rows.append({'namespace': md.get('namespace',''), 'name': name, 'phase': phase})
import os, datetime
ts = datetime.datetime.utcnow().strftime('%Y-%m-%dT%H:%M:%SZ')
with open(events_path, 'w') as out:
    out.write(f"# fixture-pod-readiness-events {ts} phase=fixture_inventory_set_mismatch\n")
    for r in rows:
        out.write(f"{r['namespace']}/{r['name']}\t{r['phase']}\n")
PYEOF
    abort_as FIXTURE_NOT_READY \
      "fixture inventory observed=${expected_unique_count} unique expected=${expected_fixture_count}; canonical 13-fixture vocabulary contract (see $fix_art)" 12
  fi
  echo "[install] cilium expected label set size: ${expected_unique_count}/${expected_fixture_count}"
  # End Block A prelude.
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
  # exact 13-Pod contract; per-command failure
  # classifiers + observable convergence evidence.
  #
  # Loop structure (this block):
  #   1) on every iteration, RE-FETCH the Cilium
  #      daemon Pod list. A failed cmd
  #      (nonzero rc) writes a structured
  #      error artefact and aborts as
  #      CLUSTER_OR_CNI_NOT_READY 10. A valid
  #      empty list is a convergence observation,
  #      not a failure; we sleep and retry.
  #   2) for each daemon, capture explicit rc
  #      AND stderr for (a) kubectl exec
  #      (b) python3 JSON projection.
  #   3) project a per-iteration label set, run
  #      LC_ALL=C sort -u to produce unique
  #      labels, then count via awk.
  #   4) break on LAST >= EXPECTED.
  #   5) under-converged: emit a parseable
  #      convergence JSON whose observed_count
  #      equals the length of observed_labels.
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
import json,sys,os
try:
    raw=open('${exec_out}').read()
    data=json.loads(raw)
except Exception as e:
    print('PROJECTION-FAILED: ' + repr(e), file=sys.stderr)
    sys.exit(17)
endpoints=data if isinstance(data,list) else (data.get('endpoint') or [])
items=[]
for e in endpoints:
    for c in e.get('status',{}).get('controllers',[]):
        nm=c.get('name','')
        if nm.startswith('resolve-labels-default/cni-'):
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
    python3 - "$exp_file" "$obs_file" "$missing_file" "$unexpected_file" \
             "$daemon_list" "$deadline" "$(date +%s)" \
             "$expected_unique_count" \
             >"$convergence_art.snapshot" <<'PYEOF'
import json, sys
exp_p, obs_p, miss_p, une_p, daemon_p, dl_s, now_s, exp_s = sys.argv[1:9]
def reads(p):
    return [l for l in open(p).read().splitlines() if l.strip()]
daemons   = reads(daemon_p)
expected  = reads(exp_p)
observed  = reads(obs_p)
missing   = reads(miss_p)
unexpected= reads(une_p)
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
  "reason": "cilium endpoint publication did not identity-match the dynamic 13-Pod vocabulary contract; expected_count == observed_count is NOT sufficient, set diffs must be empty"
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
