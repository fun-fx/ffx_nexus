#!/usr/bin/env bash
# One-shot Cozystack full-stack install for Nexus.
#
#   ./deploy/cozystack/install-full.sh
#
# Optional env:
#   NS=tenant-nexus          target namespace
#   SKIP_TENANT=1            skip 00-tenant.yaml (namespace already exists)
#   SKIP_EVAL=1              skip Ollama + eval-service
#   HELM_VALUES=...          override values file (default: values-full.yaml)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
NS="${NS:-tenant-nexus}"
VALUES="${HELM_VALUES:-$ROOT/deploy/cozystack/values-full.yaml}"

echo "== Nexus full-stack install (namespace: $NS) =="

if [[ "${SKIP_TENANT:-}" != "1" ]]; then
  kubectl -n tenant-root apply -f "$ROOT/deploy/cozystack/00-tenant.yaml"
  echo "Waiting for namespace $NS..."
  kubectl get ns "$NS" >/dev/null 2>&1 || kubectl wait --for=jsonpath='{.status.phase}'=Active "namespace/$NS" --timeout=300s 2>/dev/null || true
fi

echo "Provisioning backing services..."
kubectl -n "$NS" apply -f "$ROOT/deploy/cozystack/01-clickhouse.yaml"
kubectl -n "$NS" apply -f "$ROOT/deploy/cozystack/02-postgres.yaml"
kubectl -n "$NS" apply -f "$ROOT/deploy/cozystack/03-redis.yaml"

echo "Bootstrapping Nexus Secret..."
kubectl -n "$NS" delete job nexus-bootstrap --ignore-not-found
kubectl -n "$NS" apply -f "$ROOT/deploy/cozystack/06-bootstrap-secret.yaml"
kubectl -n "$NS" wait --for=condition=complete job/nexus-bootstrap --timeout=300s
kubectl -n "$NS" logs job/nexus-bootstrap

if [[ "${SKIP_EVAL:-}" != "1" ]]; then
  echo "Deploying eval stack (Ollama + eval-service)..."
  kubectl -n "$NS" apply -f "$ROOT/deploy/cozystack/04-ollama.yaml"
  kubectl -n "$NS" delete job ollama-pull-models --ignore-not-found
  kubectl -n "$NS" apply -f "$ROOT/deploy/cozystack/04-ollama-models-job.yaml"
  kubectl -n "$NS" apply -f "$ROOT/deploy/cozystack/05-eval-service.yaml"
fi

echo "Installing Nexus via Helm..."
helm upgrade --install nexus "$ROOT/deploy/helm/nexus" -n "$NS" -f "$VALUES"

echo "Done. Verify with:"
echo "  kubectl -n $NS get pods"
echo "  ./scripts/test_prod_smoke.sh   # when Tailscale ingress is configured"
