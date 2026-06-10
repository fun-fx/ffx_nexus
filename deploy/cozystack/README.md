# Deploying Nexus on Talos + Cozystack

This directory captures the **infrastructure-as-code** for running Nexus on a
[Cozystack](https://cozystack.io) (Talos OS) cluster: the managed backing
services (ClickHouse, Postgres, Redis) as Cozystack Custom Resources, plus a
Helm values override for the Nexus chart in [`../helm/nexus`](../helm/nexus).

## Files

| File | Purpose |
| --- | --- |
| `00-tenant.yaml` | Cozystack `Tenant` → provisions the `tenant-nexus` namespace |
| `01-clickhouse.yaml` | Managed ClickHouse (trace + eval persistence). Keeper disabled (single-node) |
| `02-postgres.yaml` | Managed Postgres (virtual keys + encrypted credentials) |
| `03-redis.yaml` | Managed Redis (rate limits/budgets + semantic cache) |
| `values-prod.yaml` | Helm override: image, dependency DSNs, secrets, Tailscale ingress |

## Prerequisites

- `kubectl` context pointing at the Cozystack cluster.
- A container registry reachable from the Talos nodes. This setup uses the
  in-cluster Harbor at `harbor.192.168.0.101.nip.io` (Talos containerd cannot
  resolve Tailscale MagicDNS, so use the `nip.io` host, not `*.ts.net`).
- An image pull secret named `harbor` in `tenant-nexus` (created in step 2).

## 1. Build and push the image

```bash
docker build -t harbor.192.168.0.101.nip.io/ffx/nexus:0.1.0 .
docker push harbor.192.168.0.101.nip.io/ffx/nexus:0.1.0
```

> If pushing large layers fails with `413 Request Entity Too Large`, raise
> `client_max_body_size` on the Harbor ingress/proxy. If it fails with
> `no space left on device`, expand the `harbor-registry` PVC.

## 2. Create the tenant and pull secret

```bash
kubectl -n tenant-root apply -f 00-tenant.yaml
# wait for the tenant-nexus namespace to appear
kubectl get ns tenant-nexus -w

kubectl -n tenant-nexus create secret docker-registry harbor \
  --docker-server=harbor.192.168.0.101.nip.io \
  --docker-username='<harbor-user>' \
  --docker-password='<harbor-pass>'
```

## 3. Provision the managed services

Edit the `password:` placeholders first (or manage them out-of-band), then:

```bash
kubectl -n tenant-nexus apply -f 01-clickhouse.yaml
kubectl -n tenant-nexus apply -f 02-postgres.yaml
kubectl -n tenant-nexus apply -f 03-redis.yaml
```

The operators publish credentials into Kubernetes secrets:

| Service | Secret | Key | Client service:port |
| --- | --- | --- | --- |
| ClickHouse | `clickhouse-nexus-credentials` | `nexus` | `chendpoint-clickhouse-nexus:9000` |
| Postgres | `postgres-nexus-credentials` | `nexus` | `postgres-nexus-rw:5432` |
| Redis | `redis-nexus-auth` | `password` | `rfrm-redis-nexus:6379` |

Build the DSNs for `values-prod.yaml`:

```
clickhouse://nexus:<pw>@chendpoint-clickhouse-nexus:9000/nexus
postgres://nexus:<pw>@postgres-nexus-rw:5432/nexus?sslmode=require
redis://:<pw>@rfrm-redis-nexus:6379/0
```

## 4. Deploy Nexus

Fill in `values-prod.yaml` (or pass secrets via `--set-string`), then:

```bash
helm upgrade --install nexus ../helm/nexus \
  -n tenant-nexus \
  -f values-prod.yaml
```

Generate a master key for credential encryption if you don't have one:

```bash
openssl rand -hex 32
```

## 5. Verify

```bash
# health (external, via Tailscale ingress)
curl -s https://nexus.<tailnet>.ts.net/healthz

# create a virtual key (console is internal-only; port-forward to reach it)
kubectl -n tenant-nexus port-forward svc/nexus 8081:8081 &
curl -s localhost:8081/api/keys \
  -d '{"name":"demo","allowed_models":["gemini-2.5-flash"]}'

# authed chat through the gateway
curl -s https://nexus.<tailnet>.ts.net/v1/chat/completions \
  -H "Authorization: Bearer <vk-secret>" \
  -d '{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}'
```

## Notes

- **ClickHouse Keeper is disabled** because Nexus uses only non-replicated
  `MergeTree` tables. On a single node a 3-replica Keeper cannot reach quorum and
  crash-loops. Enable it only on multi-node clusters needing `ReplicatedMergeTree`.
- The **console (`:8081`) is intentionally not exposed** through the ingress;
  reach it via `kubectl port-forward`. Only the gateway (`:8080`) is public.
- After a backing-pod restart the gateway may need a rollout restart
  (`kubectl -n tenant-nexus rollout restart deploy/nexus`) to re-establish
  ClickHouse connections if Cilium reassigns the pod identity.
