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
#   $ARTIFACTS/fixture-image-digest.txt
# for downstream verifiers.
SCRIPT_DIR_FLAG="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
echo "[install] build cni-listener fixture image" | tee -a "$ARTIFACTS/install.log"
FIXTURE_DIGEST=$(bash "$SCRIPT_DIR_FLAG/fixtures/integrationcni/build.sh" 2>>"$ARTIFACTS/install.log")
FIXTURE_RC=$?
if (( FIXTURE_RC != 0 )) || [[ -z "$FIXTURE_DIGEST" ]]; then
  echo "[install] ERROR: cni-listener fixture build failed (rc=$FIXTURE_RC)" | tee -a "$ARTIFACTS/install.log"
  exit 12
fi
printf 'fixture_image_digest=%s\nimage_ref=cni-listener:local\n' \
  "$FIXTURE_DIGEST" > "$ARTIFACTS/fixture-image-digest.txt"
echo "[install] fixture image digest: $FIXTURE_DIGEST" | tee -a "$ARTIFACTS/install.log"
echo "[install] kind load docker-image cni-listener:local" | tee -a "$ARTIFACTS/install.log"
kind load docker-image --name "${CLUSTER_NAME}" cni-listener:local \
  2>&1 | tee -a "$ARTIFACTS/install.log"

kubectl apply -f scripts/fixtures/integrationcni/00-prereq-namespaces.yaml 2>&1 | tee -a "$ARTIFACTS/install.log"
kubectl apply -f scripts/fixtures/integrationcni/01-test-pods.yaml 2>&1 | tee -a "$ARTIFACTS/install.log"
kubectl apply -f scripts/fixtures/integrationcni/02-stub-deps.yaml 2>&1 | tee -a "$ARTIFACTS/install.log"
kubectl apply -f scripts/fixtures/integrationcni/03-control-pod.yaml 2>&1 | tee -a "$ARTIFACTS/install.log"
kubectl apply -f scripts/fixtures/integrationcni/04-control-service.yaml 2>&1 | tee -a "$ARTIFACTS/install.log"
kubectl apply -f scripts/fixtures/integrationcni/05-control-policy.yaml 2>&1 | tee -a "$ARTIFACTS/install.log"

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
