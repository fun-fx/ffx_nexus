# Installing self-hosted Nexus

Two paths, and picking the wrong one wastes an afternoon.

**Trying it out on a laptop** — use the docker-compose installer in §1. One
command, no Kubernetes, no secrets to create.

**Running it for real** — use the Helm chart, §2 onward. Bring your own
Postgres, ClickHouse and Redis; the chart does not deploy databases.

---

## 1. Local: `scripts/install.sh`

```bash
curl -fsSL install.nexus.ffx.ai | bash
```

Needs `git`, `docker` (with Compose v2), `curl` and `go` on `PATH`. It clones
into `~/.nexus/src`, starts Postgres, Redis, ClickHouse and Ollama with
docker-compose, builds the binary, and runs it on `:8090` (gateway) and
`:8091` (console).

It generates a `NEXUS_MASTER_KEY` for you and sets `NEXUS_ALLOW_SIGNUP=true`,
so first login is "Create account" in the console. Then add a provider key,
mint a virtual key, and point your client at it:

```bash
export OPENAI_BASE_URL=http://localhost:8090/v1
export OPENAI_API_KEY=nxs_live_...
```

Logs at `~/.nexus/nexus.log`, PID at `~/.nexus/nexus.pid`. Exit codes are
specific enough to act on: `10` docker missing or daemon down, `20` git
missing, `30` a dependency never got healthy, `40` the Go build failed, `50`
the gateway never answered `/healthz`.

This is a development stack. It signs you up without an invite, runs
everything on one host, and is not what §2 describes.

---

## 2. Kubernetes: what to have ready

### Cluster

- **A CNI that enforces NetworkPolicy** if you keep the chart's default
  profile — which refuses to install until you confirm you have one. Cilium,
  Calico, Antrea, or a managed policy add-on. Flannel alone does not enforce.
  Read [`network-policy-prerequisites.md`](network-policy-prerequisites.md);
  it is the single most common reason a first install fails.
- No minimum Kubernetes version is declared by the chart.
- No persistent volumes. The pod mounts an `emptyDir` for `/tmp` and nothing
  else; all state lives in your databases.

### Datastores

The chart connects to external or managed instances. Have ready, for each of
Postgres, ClickHouse and Redis:

- a **connection url** (goes in a Secret), and
- the **host, port and namespace** (go in values).

Both halves are required and they are not interchangeable. The url carries
credentials and is what the process dials. The host, port and namespace are
what NetworkPolicy builds its egress peer from, and they are **not** derived
from the url, because Helm cannot read a Secret while rendering the chart.

A dependency enabled with a url and no host produces a pod allowed to resolve
DNS and reach nothing — the release installs and the connection times out.
The chart refuses that combination now, but it is worth understanding rather
than working around.

Postgres is required for the control plane: organizations, users, virtual
keys, BYOK credentials. ClickHouse holds traces and benchmark history and its
absence degrades analytics without stopping request handling. Redis backs
rate limiting and the semantic cache.

### A route off-cluster

Under enforcement, there is **no rule for TCP 443** unless you enable the
egress proxy. Without one the gateway starts, passes readiness, reports
healthy, and fails every request that has to reach a provider. Have a forward
proxy — its host, port, namespace and pod labels — or plan to run without
policy enforcement.

### The master key

`NEXUS_MASTER_KEY` encrypts stored provider credentials. It must decode from
base64 or hex to **exactly 32 bytes**:

```bash
openssl rand -hex 32
```

Store it where you store your other break-glass secrets. **If you lose it,
every stored provider credential is permanently undecryptable** — a database
restore without the matching key gives you rows nobody can read. If it is
missing or malformed the gateway still starts, logs `invalid
NEXUS_MASTER_KEY; credential encryption disabled`, and refuses to store
provider keys.

---

## 3. Create the Secret

Out-of-band, so credentials never enter a values file. Reference it with
`existingSecret` and the chart will not render one of its own.

```bash
kubectl create namespace nexus

kubectl -n nexus create secret generic nexus-secrets \
  --from-literal=NEXUS_POSTGRES_URL='postgres://nexus:...@postgres.database.svc.cluster.local:5432/nexus?sslmode=require' \
  --from-literal=NEXUS_CLICKHOUSE_URL='clickhouse://nexus:...@clickhouse.analytics.svc.cluster.local:9000/nexus' \
  --from-literal=NEXUS_REDIS_URL='redis://:...@redis-master.cache.svc.cluster.local:6379/0' \
  --from-literal=NEXUS_MASTER_KEY="$(openssl rand -hex 32)" \
  --from-literal=NEXUS_ADMIN_EMAIL='admin@example.com' \
  --from-literal=NEXUS_ADMIN_PASSWORD='<at least 8 characters>'
```

Recognised keys, all optional unless the matching feature is on:

| Key | For |
| --- | --- |
| `NEXUS_POSTGRES_URL` | control plane |
| `NEXUS_CLICKHOUSE_URL` | traces, benchmarks |
| `NEXUS_REDIS_URL` | rate limiting, semantic cache |
| `NEXUS_MASTER_KEY` | credential encryption |
| `NEXUS_ADMIN_EMAIL`, `NEXUS_ADMIN_PASSWORD` | first login (§5) |
| `NEXUS_SSO_CLIENT_SECRET` | SSO |
| `NEXUS_SMTP_PASSWORD` / `NEXUS_RESEND_API_KEY` | email |
| `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `GROQ_API_KEY`, `MISTRAL_API_KEY` | shared provider keys |
| `NEXUS_JUDGE_API_KEY`, `NEXUS_EMBEDDINGS_API_KEY` | eval judge, embeddings |

With `config.keyMode: strict_byok` — what both examples use — tenants supply
their own provider keys and the shared ones above are unnecessary.

If you use External Secrets Operator or Vault Agent, project into this same
shape; the chart only cares about the key names.

---

## 4. Install

Start from an example rather than assembling values yourself. Both are kept
installable, and both are rendered in CI on every pull request:

- `deploy/helm/nexus/values-production.example.yaml`
- `deploy/helm/nexus/values-staging.example.yaml`

Copy one, replace every `<PLACEHOLDER>`, then:

```bash
helm install nexus oci://ghcr.io/fun-fx/charts/nexus \
  --version <CHART_VERSION> \
  --namespace nexus --create-namespace \
  -f values-production.yaml \
  --wait --timeout 10m
```

Pin `--version` and `image.tag`. Unpinned, the image follows the chart's
`appVersion` and moves under you on the next chart bump. See
[`customer-self-hosted-upgrade-rollback.md`](customer-self-hosted-upgrade-rollback.md).

Check the render before you install anything — it costs nothing and catches
every fail-closed refusal:

```bash
helm template nexus oci://ghcr.io/fun-fx/charts/nexus \
  --version <CHART_VERSION> -f values-production.yaml > /dev/null
```

### Refusals you may hit, and what each means

The chart fails at render rather than installing something that cannot work.
Each message names the value to change.

| Message contains | Meaning |
| --- | --- |
| `requires networkPolicy.enforcementAcknowledged=true` | Default profile is `enterprise`. Confirm your CNI enforces policy, then set it — or use `profile=development`. |
| `profile=enterprise requires networkPolicy.mode=enforce` | `enterprise` plus `mode=disabled` claims enforcement and renders none. Use `profile=development` if you want no policy. |
| `requires either networkPolicy.postgres.selector.enabled ... OR ... cidr.enabled` | Pick exactly one Postgres target mode. |
| `allows only one Postgres target mode` | You set both. |
| `selector mode requires networkPolicy.postgres.selector.namespace` | Empty would allow cluster-wide egress to matching pods. |
| `no egress peer to reach them` | A datastore is enabled with no `host`, or with no target mode. The full message names each one. |
| `refuses external features without an egress proxy` | SSO or Resend is on with no way out. Enable the proxy or declare `serviceTargets.<feature>.namespace`. |
| `config.grafana.enabled requires ... instanceSelector.matchLabels` | An empty selector would match every visible Grafana. |

One failure mode is *not* a render error: `metrics.serviceMonitor.enabled:
true` without the Prometheus Operator CRDs installed fails at apply time with
`no matches for kind "ServiceMonitor"`.

### What happens during install

A migration Job runs first, as a `pre-install` hook, using the same image and
Secret as the Deployment. Helm waits for it, so a failed migration is a failed
install with logs you can read rather than pods running against a schema they
do not match.

```bash
kubectl -n nexus get jobs -l app.kubernetes.io/component=migration
kubectl -n nexus logs job/nexus-migrate-1
```

---

## 5. First login

Three ways in. Pick one before installing, because the first two need values
in place.

**Bootstrap admin (simplest).** Put `NEXUS_ADMIN_EMAIL` and
`NEXUS_ADMIN_PASSWORD` in the Secret. On first boot, if the org has no users,
an admin is created. It is a no-op afterwards, so it is safe to leave set.
Password minimum is 8 characters.

**SSO.** Configure `config.sso.*` with `NEXUS_SSO_CLIENT_SECRET`. Users are
provisioned on first login as `member`. Under enforcement your issuer needs an
egress path — see §2.

**Invites.** An existing admin creates one from the console; the invite URL
works whether or not email is configured.

`config.allowSignup` is `false` by default and should stay that way in a
cluster. `install.sh` sets it true because a laptop is not a tenant boundary.

Then:

```bash
kubectl -n nexus port-forward svc/nexus 8081:8081
open http://localhost:8081
```

---

## 6. Verify

```bash
# Liveness — process is up. No dependency checks.
curl -s https://<your-gateway>/healthz          # -> ok

# Readiness — dependencies and schema.
curl -s https://<your-gateway>/readyz | jq
```

`/readyz` returns `200` when every required check passes and `503` otherwise,
with a per-check breakdown:

```json
{
  "ready": true,
  "checks": [
    { "name": "postgres_schema",   "ok": true, "required": true,  "detail": "" },
    { "name": "clickhouse_schema", "ok": true, "required": false, "detail": "" }
  ]
}
```

`postgres_schema` is required — outstanding Postgres migrations mean no
traffic is served. `clickhouse_schema` is not; it degrades analytics while the
gateway keeps serving.

Then send one real request, because nothing above covers provider egress:

```bash
curl https://<your-gateway>/v1/chat/completions \
  -H "Authorization: Bearer nxs_live_..." \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"ping"}]}'
```

A gateway that answers `/readyz` but times out here is almost always a missing
443 egress path. `/readyz` has no check for it.

---

## 7. Things worth knowing before you commit to a layout

**Ports.** Gateway `8080`, console `8081`, both from one container. Metrics on
its own port when `metrics.enabled`.

**Traces do not include message bodies by default.** Prompts and completions
are stripped before durable storage unless you set
`config.captureTraceContent: true`. In-process evaluators still see full
bodies. See `docs/d2c-design-inventory.md`.

**`deploymentMode: split` and the `gateway.*` / `worker.*` / `connections.*`
blocks in `values.yaml` are not implemented.** The chart renders a single
Deployment. Those keys are described but not wired to anything, so setting
them has no effect — do not plan capacity around them.

**Same for `serviceTargets.postgres`, `.redis`, `.clickhouse` and
`.egressProxy`.** Only `serviceTargets.sso.namespace` and
`serviceTargets.resend.namespace` are read by any template. Configure
datastore peers under `dependencies.*` and `networkPolicy.*`.

**The README's version number lags.** Trust `Chart.yaml`.

---

## Related

- [`network-policy-prerequisites.md`](network-policy-prerequisites.md) — read
  before enabling enforcement
- [`customer-self-hosted-upgrade-rollback.md`](customer-self-hosted-upgrade-rollback.md)
  — version pinning, migrations, rollback
- [`customer-self-hosted-security.md`](customer-self-hosted-security.md) —
  origin policy, cookies, CSRF, tenant boundary
- [`customer-self-hosted-integrations.md`](customer-self-hosted-integrations.md)
  — email transport
