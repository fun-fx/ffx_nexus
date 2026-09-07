# Kubernetes (Helm)

This is the short version. For a production install — datastores,
NetworkPolicy, secrets, first login — read
[`customer-self-hosted-install.md`](customer-self-hosted-install.md).

A first-party Helm chart lives in `deploy/helm/nexus`. It deploys the gateway
(`:8080`) and console (`:8081`) from a single container, with liveness/readiness
probes on `/healthz`, a non-root hardened pod, and optional Ingress / HPA /
PodDisruptionBudget. The chart does **not** run databases itself — it connects
to external/managed Postgres, ClickHouse, and Redis (toggle each on).

The chart defaults to `networkPolicy.profile=enterprise`, which renders a
default-deny NetworkPolicy and refuses to install until you have named every
peer the pod is allowed to reach. That is the right default for a cluster
serving real traffic and the wrong one for a first look, so the trial command
below turns it off explicitly. See
[`docs/network-policy-prerequisites.md`](network-policy-prerequisites.md)
before you turn it on.

```bash
# Zero-dependency: just the gateway + console (point a provider key at it).
helm install nexus deploy/helm/nexus \
  --namespace nexus --create-namespace \
  --set secrets.openaiApiKey=sk-... \
  --set networkPolicy.profile=development \
  --set networkPolicy.mode=disabled

# Port-forward and try it
kubectl -n nexus port-forward svc/nexus 8080:8080 8081:8081
curl -s localhost:8080/healthz
```

Enable the control plane, persistence, and cache by wiring external datastores.
Each one needs a `host` and `namespace` as well as `enabled`: the connection
url lives in a Secret, and NetworkPolicy cannot read a Secret while rendering,
so it builds its egress peer from these fields instead. Enabling a datastore
without them used to install a release whose pods could resolve DNS and reach
nothing; the chart now refuses it.

```bash
helm install nexus deploy/helm/nexus -n nexus --create-namespace \
  --set existingSecret=nexus-secrets \
  --set networkPolicy.enforcementAcknowledged=true \
  --set dependencies.postgres.enabled=true \
  --set dependencies.postgres.host=postgres.database.svc.cluster.local \
  --set dependencies.postgres.namespace=database \
  --set dependencies.clickhouse.enabled=true \
  --set dependencies.clickhouse.host=clickhouse.analytics.svc.cluster.local \
  --set dependencies.clickhouse.namespace=analytics \
  --set dependencies.redis.enabled=true \
  --set dependencies.redis.host=redis-master.cache.svc.cluster.local \
  --set dependencies.redis.namespace=cache
```

That command leaves the pod with no route off-cluster, so provider calls will
fail until you also point `networkPolicy.egress.proxy` at your forward proxy.
`deploy/helm/nexus/values-production.example.yaml` shows the whole shape, and
[`docs/customer-self-hosted-install.md`](customer-self-hosted-install.md)
walks through it.

For production, create a Secret out-of-band and reference it with
`existingSecret` (keys: `OPENAI_API_KEY`, `NEXUS_MASTER_KEY`,
`NEXUS_POSTGRES_URL`, `NEXUS_CLICKHOUSE_URL`, `NEXUS_REDIS_URL`, …) instead of
putting secrets in `values.yaml`. All non-secret settings map to `config.*` in
`values.yaml` (routing, guardrails, semantic cache, self-correction).

Container images are published to `ghcr.io/fun-fx/ffx_nexus` on every `v*` tag.
