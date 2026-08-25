#!/usr/bin/env bash
# scripts/install-nexus-test.sh
#
# Phase D-2b.21: install ONLY the chart's
# NetworkPolicy onto the multi-node enforcing-CNI
# test cluster created by test-cluster-up.sh.
#
# Why this script does NOT call
# `helm install deploy/helm/nexus` end to end:
# the chart's Deployment images require an image
# registry the test environment may not have.
# The CNI gate is exclusively about the chart's
# rendered NetworkPolicy enforcement, so we
# render with `helm template` (--show-only
# templates/networkpolicy.yaml), then `kubectl
# apply -f -`. The Deployment targets are
# substituted by the fixture Deployments in
# scripts/fixtures/integrationcni/.
#
# Inputs:
#   CLUSTER_NAME  (default nexus-cni-test)
#   ARTIFACTS     (default $PWD/artifacts/integrationcni)
#   CHART_PATH    (default $PWD/deploy/helm/nexus)
set -euo pipefail
SCRIPT_DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
VALUES_EXTRA="${VALUES_EXTRA:-$SCRIPT_DIR/fixtures/integrationcni/values-extra-cni.yaml}"
CLUSTER_NAME="${CLUSTER_NAME:-nexus-cni-test}"
ARTIFACTS="${ARTIFACTS:-${PWD}/artifacts/integrationcni}"
CHART_PATH="${CHART_PATH:-${PWD}/deploy/helm/nexus}"

mkdir -p "$ARTIFACTS"
if [[ ! -f "${ARTIFACTS}/cluster-up.txt" ]]; then
  echo "[install] ERROR: cluster not up; run test-cluster-up.sh first"
  exit 2
fi

# Render NetworkPolicy from the chart with
# enterprise profile + multi-worker values that
# match the fixture Pod selectors exactly.
RENDER="$ARTIFACTS/rendered-networkpolicy.yaml"
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
  > "$RENDER" 2> "$ARTIFACTS/render-errors.log"

NETPOL_COUNT=$(grep -c "^kind: NetworkPolicy$" "$RENDER" || true)
if (( NETPOL_COUNT < 4 )); then
  echo "[install] ERROR: rendered chart produced $NETPOL_COUNT NetworkPolicy docs; expected ≥4"
  cat "$ARTIFACTS/render-errors.log" || true
  exit 2
fi

# Apply the rendered NetworkPolicy.
kubectl apply -f "$RENDER" 2>&1 | tee -a "$ARTIFACTS/install.log"

# Apply fixtures (Namespaces first, then
# Deployments + Services). Order matters:
# sources/ingress/prometheus/untrusted live
# in dedicated namespaces created by
# 00-prereq-namespaces.yaml.
#
# Pre-step: build the deterministic cni-listener
# fixture image used by every fixture Pod and
# push it into kind. Without this build the
# Pods remain in ImagePullBackOff forever
# because imagePullPolicy: Never on each Pod
# refuses to fall back to any remote registry.
# The build script is the SINGLE source of
# digest and the digest pin is recorded in
#   $ARTIFACTS/fixture-image-digest.json
# (structured, see Phase D-2b.26).
SCRIPT_DIR_FLAG="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
echo "[install] build cni-listener fixture image" | tee -a "$ARTIFACTS/install.log"
FIXTURE_BUILD_OUT=$(ARTIFACTS="$ARTIFACTS" \
                    bash "$SCRIPT_DIR_FLAG/fixtures/integrationcni/build.sh" 2>>"$ARTIFACTS/install.log")
FIXTURE_RC=$?
# Record the build pipeline outputs into the
# artifact folder so a verifier can correlate
# per-image build / inspect / kind load with
# the final Pod readiness. These paths are
# referenced by deploy/helm/nexus/tests/
# image_pipeline_mutation_test.py so the
# names must remain stable.
ARTIFACTS_DIR_ABS="$(cd "$ARTIFACTS" && pwd -P)"
echo "[install] fixture build: build.log=${ARTIFACTS_DIR_ABS}/fixture-image-build.log inspect.log=${ARTIFACTS_DIR_ABS}/fixture-image-inspect.json" \
  | tee -a "$ARTIFACTS/install.log"
if (( FIXTURE_RC != 0 )) || [[ -z "$FIXTURE_BUILD_OUT" ]]; then
  echo "[install] ERROR: cni-listener fixture build failed (rc=$FIXTURE_RC)" | tee -a "$ARTIFACTS/install.log"
  FIXTURE_IMAGE_NOT_LOADED=1
  # We DO NOT continue into fixture apply;
  # hand off to the unified readiness gate
  # so the run is classified FIXTURE_IMAGE_NOT_LOADED
  # (exit 14) — distinct from CHART_OR_POLICY_INVALID (11),
  # FIXTURE_NOT_READY (12), and
  # CLUSTER_OR_CNI_NOT_READY (10).
  GATE_PHASE=post-fixture \
    RECOVERY_PR_SHA="${RECOVERY_PR_SHA:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}" \
    WORKFLOW_RUN_ID="${WORKFLOW_RUN_ID:-${GITHUB_RUN_ID:-local}}" \
    ARTIFACTS="${ARTIFACTS}" \
    FIXTURE_IMAGE_NOT_LOADED=1 \
    FIXTURE_IMAGE_LOAD_FAILURE_DETAIL="docker build returned rc=$FIXTURE_RC with no stdout json line" \
    bash "${SCRIPT_DIR_FLAG}/cni-readiness-gate.sh" || exit $?
  exit 14
fi
# Parse the JSON line from stdout (the build
# script's structured "did the build succeed"
# signal). Anything else is a contract drift.
FIXTURE_BUILD_JSON="$FIXTURE_BUILD_OUT"
echo "[install] build produced JSON: $FIXTURE_BUILD_JSON" | tee -a "$ARTIFACTS/install.log"
FIXTURE_IMAGE_ID=$(echo "$FIXTURE_BUILD_JSON" | python3 -c "
import json,sys
try:
    d=json.loads(sys.stdin.read())
except Exception:
    sys.exit(1)
print(d.get('image_id',''))")
FIXTURE_IMAGE_REF=$(echo "$FIXTURE_BUILD_JSON" | python3 -c "
import json,sys
d=json.loads(sys.stdin.read())
print(d.get('image_ref',''))")
if [[ -z "$FIXTURE_IMAGE_ID" ]]; then
  echo "[install] ERROR: build artifact missing image_id" | tee -a "$ARTIFACTS/install.log"
  cat "$ARTIFACTS/fixture-image-digest.json" 2>/dev/null | tee -a "$ARTIFACTS/install.log" || true
  exit 14
fi

# kind load docker-image. The exit-code zero
# alone is NOT a sufficient success signal;
# we follow it with per-node crictl images to
# confirm the image made it onto every node.
KIND_LOAD_LOG="$ARTIFACTS/fixture-image-kind-load.log"
{
  echo "kind load docker-image --name ${CLUSTER_NAME} ${FIXTURE_IMAGE_REF}" >>"$KIND_LOAD_LOG"
  kind load docker-image --name "${CLUSTER_NAME}" "${FIXTURE_IMAGE_REF}"
} >>"$KIND_LOAD_LOG" 2>&1
KIND_LOAD_RC=$?
echo "[install] kind load rc=$KIND_LOAD_RC; verifying per-node runtime" | tee -a "$ARTIFACTS/install.log"
if (( KIND_LOAD_RC != 0 )); then
  echo "[install] ERROR: kind load returned non-zero (rc=$KIND_LOAD_RC)" | tee -a "$KIND_LOAD_LOG"
  cat "$KIND_LOAD_LOG" | tee -a "$ARTIFACTS/install.log"
  GATE_PHASE=post-fixture \
    RECOVERY_PR_SHA="${RECOVERY_PR_SHA:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}" \
    WORKFLOW_RUN_ID="${WORKFLOW_RUN_ID:-${GITHUB_RUN_ID:-local}}" \
    ARTIFACTS="${ARTIFACTS}" \
    FIXTURE_IMAGE_NOT_LOADED=1 \
    FIXTURE_IMAGE_LOAD_FAILURE_DETAIL="kind load returned rc=$KIND_LOAD_RC" \
    bash "${SCRIPT_DIR_FLAG}/cni-readiness-gate.sh" || exit $?
  exit 14
fi

# Per-node runtime verification: query every
# kubelet container runtime via `crictl images`
# (kind uses containerd by default) and confirm
# the image is present on every node. Empty
# intersection across nodes is FIXTURE_IMAGE_NOT_LOADED.
NODE_RUNTIME_LOG="$ARTIFACTS/fixture-image-node-runtime.log"
{
  echo "=== per-node runtime image inventory ==="
  for n in $(kind get nodes --name "${CLUSTER_NAME}" 2>/dev/null); do
    echo "--- node: $n ---"
    docker exec "${n}" crictl images 2>&1 \
      | grep -E "$(echo "$FIXTURE_IMAGE_REF" | sed 's,:,\\\\\\\\\\\,:g')|${FIXTURE_IMAGE_ID:0:12}" \
      || echo "(no match in node $n)"
  done
} >>"$NODE_RUNTIME_LOG" 2>&1 || true
MISSING=$(grep -c "^--- node:" "$NODE_RUNTIME_LOG" || echo 0)
PRESENT=$(grep -c "${FIXTURE_IMAGE_ID:0:12}" "$NODE_RUNTIME_LOG" || echo 0)
echo "[install] per-node runtime: ${PRESENT}/${MISSING} nodes have the fixture image_id" \
  | tee -a "$ARTIFACTS/install.log"
if (( PRESENT < MISSING )); then
  cat "$NODE_RUNTIME_LOG" | tee -a "$ARTIFACTS/install.log" || true
  echo "[install] ERROR: fixture image missing on one or more kind nodes" \
    | tee -a "$ARTIFACTS/install.log"
  GATE_PHASE=post-fixture \
    RECOVERY_PR_SHA="${RECOVERY_PR_SHA:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}" \
    WORKFLOW_RUN_ID="${WORKFLOW_RUN_ID:-${GITHUB_RUN_ID:-local}}" \
    ARTIFACTS="${ARTIFACTS}" \
    FIXTURE_IMAGE_NOT_LOADED=1 \
    FIXTURE_IMAGE_LOAD_FAILURE_DETAIL="image_id=${FIXTURE_IMAGE_ID} missing on at least one kind node: ${PRESENT}/${MISSING} present" \
    bash "${SCRIPT_DIR_FLAG}/cni-readiness-gate.sh" || exit $?
  exit 14
fi

kubectl apply -f scripts/fixtures/integrationcni/00-prereq-namespaces.yaml 2>&1 | tee -a "$ARTIFACTS/install.log"
kubectl apply -f scripts/fixtures/integrationcni/01-test-pods.yaml 2>&1 | tee -a "$ARTIFACTS/install.log"
kubectl apply -f scripts/fixtures/integrationcni/02-stub-deps.yaml 2>&1 | tee -a "$ARTIFACTS/install.log"
kubectl apply -f scripts/fixtures/integrationcni/03-control-pod.yaml 2>&1 | tee -a "$ARTIFACTS/install.log"
kubectl apply -f scripts/fixtures/integrationcni/04-control-service.yaml 2>&1 | tee -a "$ARTIFACTS/install.log"
kubectl apply -f scripts/fixtures/integrationcni/05-control-policy.yaml 2>&1 | tee -a "$ARTIFACTS/install.log"

# Phase D-2b.27: pre-flight dry-run gate.
#
# Every fixture yaml is `kubectl apply`-
# --dry-run=server --validate=strict on a
# kind control plane before any of them is
# applied for real. A failure here means the
# fixture yaml is structurally unsound and
# is independent of:
#   - the cluster / CNI / cilium policy
#   - the chart's Helm render or NetworkPolicy
# so the unified gate MUST classify it as
# FIXTURE_INVALID (exit 15) and never as
# CLUSTER_OR_CNI_NOT_READY (10),
# CHART_OR_POLICY_INVALID (11),
# FIXTURE_NOT_READY (12),
# FIXTURE_IMAGE_NOT_LOADED (14), or
# SCENARIO_POLICY_REGRESSION (13).
# Without this pre-flight, run-id 32470841379
# applied a Pod whose `containers:` key was a
# sibling of `spec:`, the server rejected it
# with strict-decode error, and the run
# looked like a green image pipeline plus a
# red scenario verdict — both readings were wrong.
DRYRUN_LOG="$ARTIFACTS/fixture-dryrun.log"
: > "$DRYRUN_LOG"
DRYRUN_OK=1
for fy in \
  scripts/fixtures/integrationcni/00-prereq-namespaces.yaml \
  scripts/fixtures/integrationcni/01-test-pods.yaml \
  scripts/fixtures/integrationcni/02-stub-deps.yaml \
  scripts/fixtures/integrationcni/03-control-pod.yaml \
  scripts/fixtures/integrationcni/04-control-service.yaml \
  scripts/fixtures/integrationcni/05-control-policy.yaml \
; do
    echo "--- kubectl apply --dry-run=server --validate=strict -f $fy ---" | tee -a "$DRYRUN_LOG"
    if ! kubectl apply --dry-run=server --validate=strict -f "$fy" 2>&1 \
        | tee -a "$DRYRUN_LOG"; then
      echo "[install] dry-run FAILED on $fy" | tee -a "$DRYRUN_LOG"
      DRYRUN_OK=0
    fi
done
if (( DRYRUN_OK != 1 )); then
    echo "[install] ERROR: pre-flight fixture dry-run failed" \
      | tee -a "$ARTIFACTS/install.log"
    cat "$DRYRUN_LOG" | tee -a "$ARTIFACTS/install.log" || true
    FIXTURE_INVALID=1
    FIXTURE_INVALID_FAILURE_DETAIL="pre-flight kubectl apply --dry-run=server --validate=strict FAILED on one or more fixture yamls (see $DRYRUN_LOG)"
    GATE_PHASE=post-fixture \
      RECOVERY_PR_SHA="${RECOVERY_PR_SHA:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}" \
      WORKFLOW_RUN_ID="${WORKFLOW_RUN_ID:-${GITHUB_RUN_ID:-local}}" \
      ARTIFACTS="${ARTIFACTS}" \
      FIXTURE_INVALID=1 \
      FIXTURE_INVALID_FAILURE_DETAIL="$FIXTURE_INVALID_FAILURE_DETAIL" \
      bash "${SCRIPT_DIR}/cni-readiness-gate.sh" || exit $?
    exit 15
fi
echo "[install] pre-flight fixture dry-run OK" | tee -a "$ARTIFACTS/install.log"

# Polled wait for fixture Pods to be Ready
# before we count cilium endpoints. A Pod that
# is still being scheduled does NOT have a
# cilium endpoint yet, so ENDPOINT_WAIT only
# starts after this gate. Without it we measure
# endpoints too early and falsely conclude
# "cilium is missing". The 4-minute window was
# empirically too tight for a cold-pull image
# cache on a fresh runner (observed across
# 32448924433, 32450384157, 32451208718,
# 32452639663) — we extend to 8 minutes
# because (a) the gate's exit 12 contract
# still holds, and (b) an image-pull flake
# inside that window is correctly classified
# as FIXTURE_NOT_READY by the same gate, not
# as a chart regression.
DEADLINE=$(( $(date +%s) + 480 ))
while (( $(date +%s) < DEADLINE )); do
  # Drain any image-pull failures into a dedicated
  # counter so we can say "ImagePullBackOff on one
  # or more fixture Pods" instead of just
  # "fixture Pods not Ready within 8 minutes".
  # An image-pull failure while
  # imagePullPolicy: Never is the literal
  # definition of FIXTURE_IMAGE_NOT_LOADED (exit 14):
  # the image never made it onto this node's
  # containerd despite kind load reporting rc=0.
  IMAGE_PULL_FAIL_LOG="$ARTIFACTS/fixture-pod-imagepull.log"
  {
    kubectl get pod -A --no-headers 2>/dev/null \
      | grep -E "cni-target|cni-source|cni-control" \
      | while read -r ns nm rest; do
          kubectl get pod "$nm" -n "$ns" -o json 2>/dev/null \
            | python3 -c "
import json,sys
try:
  d=json.loads(sys.stdin.read())
except Exception:
  d={}
cs=[c.get('state',{}) for c in d.get('status',{}).get('containerStatuses') or []]
print(d.get('metadata',{}).get('namespace',''), d.get('metadata',{}).get('name',''),
      [c.get('state',{}).get('waiting',{}).get('reason','') for c in cs if c])"
      done
  } >"$IMAGE_PULL_FAIL_LOG" 2>&1 || true
  PULL_REASONS=$(
    awk '$3!="" {print $3}' "$IMAGE_PULL_FAIL_LOG" \
      | tr -d '[]"\047' \
      | grep -E "^(ImagePullBackOff|ErrImageNeverPull|ErrImagePull|CrashLoopBackOff)$" \
      | sort -u | tr '\n' ',' | sed 's/,$//'
  )
  if [[ -n "$PULL_REASONS" ]]; then
    echo "[install] ERROR: fixture Pod image-pull failure reasons: $PULL_REASONS" \
      | tee -a "$ARTIFACTS/install.log"
    cat "$IMAGE_PULL_FAIL_LOG" | tee -a "$ARTIFACTS/install.log" || true
    GATE_PHASE=post-fixture \
      RECOVERY_PR_SHA="${RECOVERY_PR_SHA:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}" \
      WORKFLOW_RUN_ID="${WORKFLOW_RUN_ID:-${GITHUB_RUN_ID:-local}}" \
      ARTIFACTS="${ARTIFACTS}" \
      FIXTURE_IMAGE_NOT_LOADED=1 \
      FIXTURE_IMAGE_LOAD_FAILURE_DETAIL="fixture Pod entered ImagePullBackOff/ErrImagePull/CrashLoopBackOff despite kind load rc=0: $PULL_REASONS; image_id=${FIXTURE_IMAGE_ID}" \
      bash "${SCRIPT_DIR}/cni-readiness-gate.sh" || exit $?
    exit 14
  fi
  NOTREADY=$(kubectl get pod -A --no-headers 2>/dev/null \
    | grep -E "cni-target|cni-source|cni-control" \
    | awk '$3 != "Running" || $4 != "1/1" {n++}; END {print n+0}')
  if (( NOTREADY == 0 )); then break; fi
  sleep 5
done
if (( $(date +%s) >= DEADLINE )); then
  echo "[install] ERROR: fixture Pods not all Ready within 8 minutes"
  kubectl get pod -A -l 'app in (cni-target, cni-source, cni-control)' -o wide 2>&1 | tee -a "$ARTIFACTS/install.log"
  # Hand off to the unified readiness gate so the
  # reason is recorded as FIXTURE_NOT_READY (exit
  # 12) instead of an ambiguous "exit 2".
  GATE_PHASE=post-fixture \
  RECOVERY_PR_SHA="${RECOVERY_PR_SHA:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}" \
  WORKFLOW_RUN_ID="${WORKFLOW_RUN_ID:-${GITHUB_RUN_ID:-local}}" \
  ARTIFACTS="${ARTIFACTS}" \
    bash "${SCRIPT_DIR}/cni-readiness-gate.sh" || exit $?
  exit 12
fi

# Polled wait: cilium endpoint count includes
# every fixture target. We observe the condition,
# not a sleep. Each cilium agent exposes only
# endpoints for Pods on its own node. We
# therefore aggregate the labels resolved by
# every agent (not just one).
EXPECTED=$(kubectl get pod -A --no-headers 2>/dev/null | grep -cE "cni-target|cni-source|cni-control|cni-mock" || echo 0)
echo "[install] expected ${EXPECTED} fixture pods"
DEADLINE=$(( $(date +%s) + 480 ))
LAST=0
while (( $(date +%s) < DEADLINE )); do
  ACC=""
  for p in $(kubectl -n kube-system get pod -l k8s-app=cilium -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}'); do
    OUT=$(kubectl -n kube-system exec "$p" -- bash -c 'cilium endpoint list -o json' 2>/dev/null \
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
" 2>/dev/null)
    ACC+=$(printf "\n%s" "$OUT")
  done
  LAST=$(printf "%s" "$ACC" | grep -c "^resolve-labels-default/cni-" || echo 0)
  if (( LAST >= EXPECTED )); then
    echo "[install] cilium reports ${LAST} fixture endpoints (expected ${EXPECTED})"
    break
  fi
  sleep 5
done
if (( LAST < EXPECTED )); then
  echo "[install] ERROR: cilium endpoint count aggregated across agents reached only ${LAST} of ${EXPECTED} after $((OSEC=$(date+%s)))s; expected=${EXPECTED}"
  printf "%s" "$ACC" | sort -u | tee -a "$ARTIFACTS/install.log"
  # Hand off to the unified readiness gate so the
  # reason is recorded as FIXTURE_NOT_READY (exit
  # 12) instead of an ambiguous "exit 3".
  GATE_PHASE=post-fixture \
  RECOVERY_PR_SHA="${RECOVERY_PR_SHA:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}" \
  WORKFLOW_RUN_ID="${WORKFLOW_RUN_ID:-${GITHUB_RUN_ID:-local}}" \
  ARTIFACTS="${ARTIFACTS}" \
    bash "${SCRIPT_DIR}/cni-readiness-gate.sh" || exit $?
  exit 12
fi

# Run the unified readiness gate in post-fixture
# mode (gates #8..#9). If a probe fails, the gate
# exits 12 (FIXTURE_NOT_READY) — distinct from a
# chart regression, and distinct from a cluster
# creation flake. The summary line is the
# first thing the workflow uploads, so a downstream
# verifier can route the failure to the right
# handler without re-reading the cluster-up logs.
GATE_PHASE=post-fixture \
  RECOVERY_PR_SHA="${RECOVERY_PR_SHA:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}" \
  WORKFLOW_RUN_ID="${WORKFLOW_RUN_ID:-${GITHUB_RUN_ID:-local}}" \
  ARTIFACTS="${ARTIFACTS}" \
  bash "${SCRIPT_DIR}/cni-readiness-gate.sh"

# Snapshot cilium state for evidence.
kubectl -n kube-system exec ds/cilium -- cilium status > "$ARTIFACTS/cilium-status.txt"
kubectl -n kube-system exec ds/cilium -- cilium endpoint list -o json > "$ARTIFACTS/cilium-endpoints.json"
kubectl -n kube-system exec ds/cilium -- cilium policy get > "$ARTIFACTS/cilium-policy.txt"

echo "ready" > "$ARTIFACTS/install-ready.txt"
echo "[install] chart NetworkPolicy installed and cilium endpoints bound"
