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

**First install** — build the Secret from the operator credentials, then install
with `existingSecret` so later `helm upgrade -f values-prod.yaml` never overwrites
live DSNs with placeholders:

```bash
# 1. Create the out-of-band Secret (once; adjust passwords from step 3)
kubectl -n tenant-nexus create secret generic nexus \
  --from-literal=NEXUS_POSTGRES_URL="postgres://nexus:<pw>@postgres-nexus-rw:5432/nexus?sslmode=require" \
  --from-literal=NEXUS_CLICKHOUSE_URL="clickhouse://nexus:<pw>@chendpoint-clickhouse-nexus:9000/nexus" \
  --from-literal=NEXUS_REDIS_URL="redis://:<pw>@rfrm-redis-nexus:6379/0" \
  --from-literal=NEXUS_MASTER_KEY="$(openssl rand -hex 32)" \
  --from-literal=GEMINI_API_KEY="<your-key>" \
  --from-literal=NEXUS_ADMIN_EMAIL="admin@example.com" \
  --from-literal=NEXUS_ADMIN_PASSWORD="<bootstrap-password>"

# 2. Install/upgrade (values-prod.yaml sets existingSecret: nexus)
helm upgrade --install nexus ../helm/nexus \
  -n tenant-nexus \
  -f values-prod.yaml
```

Subsequent upgrades only need `-f values-prod.yaml`; the Secret is left untouched.

Generate a master key for credential encryption if you don't have one:

```bash
openssl rand -hex 32
```

## 5. Verify

```bash
# health (external, via Tailscale ingress)
curl -s https://nexus.<tailnet>.ts.net/healthz

# console dashboard (Tailscale — separate Ingress per host)
curl -s https://console.<tailnet>.ts.net/healthz

# create a virtual key (log in to console first, or use /api/me/keys with session)
curl -s https://console.<tailnet>.ts.net/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"<admin>","password":"<pass>"}' -c /tmp/cj

curl -s https://console.<tailnet>.ts.net/api/me/keys -b /tmp/cj \
  -H 'Content-Type: application/json' \
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
- **Gateway and console** are exposed on separate Tailscale Ingresses
  (`nexus.<tailnet>.ts.net` and `console.<tailnet>.ts.net`). Tailscale registers
  one MagicDNS name per Ingress resource.
- **`existingSecret: nexus`** in `values-prod.yaml` keeps Helm from overwriting
  live DSNs/API keys on upgrade. Create the Secret out-of-band before first install.
- After a backing-pod restart the gateway may need a rollout restart
  (`kubectl -n tenant-nexus rollout restart deploy/nexus`) to re-establish
  ClickHouse connections if Cilium reassigns the pod identity.
