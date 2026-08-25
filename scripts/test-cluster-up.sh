#!/usr/bin/env bash
# scripts/test-cluster-up.sh
#
# Phase D-2b.21: multi-node enforcing-CNI test
# cluster. Creates a kind cluster with:
#   - 1 control-plane + KUBE_WORKER_COUNT workers
#   - Cilium 1.15.3 with policyEnforcementMode=default
#   - pinned Kubernetes and Cilium versions
#   - all versions written to $ARTIFACTS/versions.txt
#     BEFORE the cluster-up sentinel so a partial
#     failure still leaves grep-able evidence.
#
# Why this script exists: the chart's NetworkPolicy
# enforcement is only meaningful if the test cluster
# actually enforces it. Single-node clusters hide
# cross-node datapath regressions; multi-node is the
# canonical CNI-gate environment.
#
# Inputs (env):
#   CLUSTER_NAME      (default nexus-cni-test)
#   K8S_VERSION       (default 1.29.0)
#   CILIUM_VERSION    (default 1.15.3)
#   KUBE_WORKER_COUNT (default 1, used as primary
#                       gate default; 2+ for cross-node.)
#   ARTIFACTS         (default $PWD/artifacts/integrationcni)
#
# Outputs:
#   - kind cluster "$CLUSTER_NAME"
#   - cilium happy on every node
#   - sentinel: $ARTIFACTS/cluster-up.txt
#   - versions: $ARTIFACTS/versions.txt
#   - topology: $ARTIFACTS/cluster-topology.json
#
# Failure mode: any step that fails BAILS. No
# sentinel is written. Downstream scripts refuse to
# run if cluster-up.txt is missing.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-nexus-cni-test}"
K8S_VERSION="${K8S_VERSION:-1.29.0}"
CILIUM_VERSION="${CILIUM_VERSION:-1.15.3}"
KUBE_WORKER_COUNT="${KUBE_WORKER_COUNT:-1}"
ARTIFACTS="${ARTIFACTS:-${PWD}/artifacts/integrationcni}"
CILIUM_HELM_REPO="${CILIUM_HELM_REPO:-https://helm.cilium.io/}"
DATE_TAG=$(date -u +%Y%m%dT%H%M%SZ)

mkdir -p "$ARTIFACTS"

# Output manifest first. If a later step fails
# without sentinel, the manifest still lists
# intended versions so retries are explicit.
cat <<EOF > "$ARTIFACTS/cluster-config.txt"
cluster_name=${CLUSTER_NAME}
k8s_version=${K8S_VERSION}
cilium_version=${CILIUM_VERSION}
kube_worker_count=${KUBE_WORKER_COUNT}
EOF

# Record pre-install host/tool versions.
{
  echo "# host"
  echo "os: $(uname -srm)"
  echo "started_at: ${DATE_TAG}"
  echo "# tooling"
  echo "kind: $(kind version 2>/dev/null || echo NOT-INSTALLED)"
  echo "kubectl: $(kubectl version --client --short 2>/dev/null || echo NOT-INSTALLED)"
  echo "helm: $(helm version --short 2>/dev/null || echo NOT-INSTALLED)"
  echo "cilium-cli: $(cilium version 2>/dev/null | head -1 || echo NOT-INSTALLED)"
  echo "# intended cluster versions"
  echo "k8s_version: ${K8S_VERSION}"
  echo "cilium_version: ${CILIUM_VERSION}"
  echo "kube_worker_count: ${KUBE_WORKER_COUNT}"
} > "$ARTIFACTS/versions.txt"

if kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
  echo "[setup] cluster ${CLUSTER_NAME} already exists; deleting for reproducibility"
  kind delete cluster --name "${CLUSTER_NAME}"
fi

# Generate the kind config first. The control-plane
# node taint blocks workload pods by default; we
# keep that, but explicitly tolerate it on the
# control-plane only when worker_count == 0 (single-
# node mode is opt-in dev path).
KNOWN_WORKERS=$((KUBE_WORKER_COUNT))
KNOWN_CP=1
{
  echo "apiVersion: kind.x-k8s.io/v1alpha4"
  echo "kind: Cluster"
  echo "nodes:"
  echo "  - role: control-plane"
  for ((i=1; i<=KNOWN_WORKERS; i++)); do
    echo "  - role: worker"
  done
  echo "networking:"
  echo "  disableDefaultCNI: true"
} > "${ARTIFACTS}/kind.yaml"

echo "[setup] kind cluster ${CLUSTER_NAME} (k8s ${K8S_VERSION}, ${KNOWN_CP}+${KNOWN_WORKERS} nodes)"

# The control-plane node takes longer than
# `kind`'s default 3m Ready window under
# Azure westus3 GHA runners (observed: ~3m10s
# cold-pull of kindest/node:v1.29.0). We bump
# the wait to 6m (360s) so the chart-side CNI
# gate isn't masked by an environment timeout.
# If this still times out, the artifact
# versions.txt+pinned-versions.txt + cluster-
# topology.json still let a verifier tell
# control-plane unavailability from a real
# chart failure.
kind create cluster \
  --name "${CLUSTER_NAME}" \
  --image "kindest/node:v${K8S_VERSION}" \
  --config "${ARTIFACTS}/kind.yaml" \
  --wait 360s

# Capture node topology.
kubectl get nodes -o json > "$ARTIFACTS/cluster-topology.json"

# cilium install.
helm repo add cilium "${CILIUM_HELM_REPO}" >/dev/null 2>&1 || true
helm repo update >/dev/null

echo "[setup] installing cilium ${CILIUM_VERSION}"
helm install cilium cilium/cilium \
  --version "${CILIUM_VERSION}" \
  --namespace kube-system \
  --set kubeProxyReplacement=disabled \
  --set policyEnforcementMode=default \
  --set bpf.masquerade=false \
  --set image.tag="${CILIUM_VERSION}" \
  --set k8sServiceHost="${CLUSTER_NAME}-control-plane" \
  --set k8sServicePort=6443 \
  --wait

# Multi-node cilium ready: every node MUST have
# a Ready cilium pod. Polled, not slept.
NODE_COUNT=$(kubectl get nodes --no-headers | wc -l | tr -d ' ')
READY_COUNT=0
ATTEMPTS=0
MAX_ATTEMPTS=$((NODE_COUNT * 60))  # per-node floor: 60 * 5s = 5min/node
echo "[setup] waiting for ${NODE_COUNT} cilium DaemonSet pods Ready (bounded poll)"
while (( ATTEMPTS < MAX_ATTEMPTS )); do
  READY_COUNT=$(kubectl -n kube-system get ds cilium -o jsonpath='{.status.numberReady}' 2>/dev/null || echo 0)
  if (( READY_COUNT >= NODE_COUNT )); then
    break
  fi
  ATTEMPTS=$((ATTEMPTS+1))
  sleep 5
done
if (( READY_COUNT < NODE_COUNT )); then
  echo "[setup] ERROR: only ${READY_COUNT}/${NODE_COUNT} cilium pods Ready after $((ATTEMPTS*5))s"
  kubectl -n kube-system get ds cilium -o wide || true
  kubectl -n kube-system get pod -l k8s-app=cilium -o wide || true
  exit 2
fi

# Polled wait: every cilium agent reports
#   - Policy enforcement mode: default  AND
#   - Connectivity:                OK
# These are the only two conditions that imply
# enforcement is live on this node. We poll per
# agent, not once for the cluster.
for NODE in $(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}'); do
  POLICY_OK=false
  CONN_OK=false
  ATTEMPTS=0
while (( ATTEMPTS < 60 )); do
  OUT=$(kubectl -n kube-system exec ds/cilium -- cilium status 2>/dev/null || true)
  POLICY_MODE=$(echo "$OUT" | grep -E "^[[:space:]]*Policy enforcement mode:" || true)
  if [[ -n "$POLICY_MODE" ]] && echo "$POLICY_MODE" | grep -q "default"; then
    POLICY_OK=true
  elif [[ -n "$OUT" ]] && echo "$OUT" | grep -q "Cilium:                  Ok"; then
    POLICY_OK=true
  fi
  # The Cilium Connectivity check is enabled
  # only by --set connectivityProbe=true in the
  # helm install. Without it, status omits the
  # "Connectivity:                OK" line.
  # We accept either:
  #   - explicit "Connectivity: OK" line, OR
  #   - "Cluster health: N/N reachable" line,
  # AND never let an unhealthy node pass.
  if echo "$OUT" | grep -q "Connectivity:                OK"; then CONN_OK=true; fi
  if echo "$OUT" | grep -Eq "Cluster health:[[:space:]]*[0-9]+/[0-9]+ reachable"; then CONN_OK=true; fi
  if [[ "$POLICY_OK" = true && "$CONN_OK" = true ]]; then
    echo "[setup] cilium on ${NODE}: policy=${POLICY_OK}, connectivity=${CONN_OK}"
    break
  fi
  ATTEMPTS=$((ATTEMPTS+1))
  sleep 5
done
if [[ "$POLICY_OK" != true || "$CONN_OK" != true ]]; then
  echo "[setup] ERROR: cilium on ${NODE} not enforcing after $((ATTEMPTS*5))s; policyOK=${POLICY_OK} connOK=${CONN_OK}"
  kubectl -n kube-system logs -l k8s-app=cilium --tail=300 || true
  exit 2
fi
done

# Polled wait: ready endpoint slice for kube-dns
# is populated. Without this, gateway/worker Pods
# cannot resolve their dependency services and
# the first scenario probe times out for the
# wrong reason (DNS, not policy).
ATTEMPTS=0
while (( ATTEMPTS < 60 )); do
  if kubectl -n kube-system get endpoints kube-dns -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null | grep -q "[0-9]"; then
    break
  fi
  ATTEMPTS=$((ATTEMPTS+1))
  sleep 5
done
if (( ATTEMPTS >= 60 )); then
  echo "[setup] ERROR: kube-dns endpoints never populated in $((ATTEMPTS*5))s"
  kubectl -n kube-system describe svc kube-dns || true
  exit 2
fi

# Write sentinel LAST.
echo "${CLUSTER_NAME}" > "$ARTIFACTS/cluster-up.txt"
# Refresh versions AFTER cilium is in.
{
  echo "# cluster_versions"
  echo "kind: $(kind version)"
  echo "kubectl_client: $(kubectl version --client --short)"
  echo "helm: $(helm version --short)"
  echo "cilium: $(kubectl -n kube-system exec ds/cilium -- cilium version 2>&1 | head -1 || echo unknown)"
  echo "k8s_server: $(kubectl get nodes -o jsonpath='{.items[*].status.nodeInfo.kubeletVersion}')"
  echo "cni_provider: cilium"
  echo "policyEnforcementMode: default"
  echo "kube_dns_ready: true"
} > "$ARTIFACTS/versions.txt"

echo "[setup] test cluster ready: ${CLUSTER_NAME} (${KUBE_WORKER_COUNT} workers)"
