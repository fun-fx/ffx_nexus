#!/usr/bin/env bash
# scripts/test-cluster-down.sh
#
# Phase D-2b.11: tear down the kind cluster
# created by test-cluster-up.sh. Removes the CNI
# DaemonSet and drops the cluster in a way that
# prevents stale policy state from leaking into
# the next run.

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-nexus-cni-test}"
ARTIFACTS="${ARTIFACTS:-${PWD}/artifacts/integrationcni}"

if [[ -f "${ARTIFACTS}/cluster-up.txt" ]]; then
  echo "[teardown] removing cluster ${CLUSTER_NAME}"
  kind delete cluster --name "${CLUSTER_NAME}" || true
  rm -f "${ARTIFACTS}/cluster-up.txt"
  echo "[teardown] cilium DaemonSet cleaned up"
else
  echo "[teardown] no cluster sentinel at ${ARTIFACTS}/cluster-up.txt — nothing to do"
fi

# We intentionally do NOT remove the kubeconfig:
# another test might still want to inspect it.
# Removing the cluster itself is enough to drop
# all pods, services, policies.
