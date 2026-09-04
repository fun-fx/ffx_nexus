# NetworkPolicy prerequisites

Read this before setting `networkPolicy.mode=enforce`, and before your first
install if you leave the chart's defaults alone — because the default profile
is `enterprise`, and it will refuse to install until this page is satisfied.

The short version: enforcement renders a **default-deny** policy over the
gateway, worker and migration pods. After that, the only traffic those pods
can send is traffic you named. If you do not name your database, they cannot
reach your database.

## 1. Your CNI has to actually enforce policy

Kubernetes accepts `NetworkPolicy` objects whether or not anything implements
them. On a cluster whose CNI ignores policy, the objects are stored, `kubectl
get networkpolicy` lists them, and every packet still flows. The install
looks identical to a correctly enforcing one.

The chart **cannot detect this**. `networkPolicy.enforcementAcknowledged` is
your statement that you have checked, not a probe result. Enforcing CNIs
include Cilium, Calico, Antrea, and the managed policy add-ons on the major
clouds. Flannel alone does not enforce.

To confirm on a cluster you did not build, apply a deny-all policy to a
throwaway namespace and check that a pod in it can no longer reach anything.
If it still can, enabling enforcement here buys you nothing and you should
know that before you rely on it.

## 2. Two profiles, and the one you want is probably not the default

| | `development` | `enterprise` |
| --- | --- | --- |
| `mode=disabled` allowed | yes | **no — refused at render** |
| Requires `enforcementAcknowledged` | no | yes |
| Requires a Postgres target mode | no | yes |
| Refuses external features with no egress path | no | yes |

`profile: enterprise` with `mode: enforce` is the chart default. It is the
right default for a cluster serving real traffic and the wrong one for a first
look, so a trial install should say so explicitly:

```bash
--set networkPolicy.profile=development \
--set networkPolicy.mode=disabled
```

Note that `enterprise` **refuses** `mode=disabled` rather than honouring it.
The combination claims the strictest posture while rendering no policy at all,
which is the one outcome worse than either half. Pick `development` if you
want no enforcement; do not reach for `mode=disabled` to get there.

## 3. Every peer you need, named

Under enforcement the pods get exactly these egress rules, and each one is
conditional on values you supply:

| Destination | Rendered when | Applies to |
| --- | --- | --- |
| Cluster DNS | always | gateway, worker, migration |
| Postgres (selector) | `dependencies.postgres.host` **and** `networkPolicy.postgres.selector.enabled` | gateway, worker, migration |
| Postgres (CIDR) | `dependencies.postgres.host` **and** `networkPolicy.postgres.cidr.enabled` | gateway, worker, migration |
| ClickHouse (in-cluster) | `dependencies.clickhouse.host` **and** `dependencies.clickhouse.namespace` | gateway, worker, migration |
| ClickHouse (CIDR) | `dependencies.clickhouse.host` **and** `dependencies.clickhouse.cidr.enabled` | gateway, worker, migration |
| Redis | `features.rateLimitRedis` **and** `dependencies.redis.host` **and** `dependencies.redis.namespace` | gateway |
| Egress proxy | `networkPolicy.egress.proxy.enabled` | gateway, worker |
| Prometheus scrape (ingress) | `networkPolicy.prometheus.namespaces` | gateway, worker |
| Ingress controller (ingress) | `networkPolicy.ingressController.namespaces` | gateway |

### `host` is not optional, even though your url is in a Secret

This is the part that surprises people. `dependencies.<dep>.url` carries the
credentials and is what the process dials. `host`, `port` and `namespace` are
what NetworkPolicy builds its peer from — and they are **not** parsed out of
the url, because the url lives in a Secret that Helm cannot read while
rendering the chart.

So a dependency enabled with a url and no host is a pod allowed to resolve DNS
and reach nothing. The release installs, all five policies render, and the
connection times out at runtime with a default-deny policy as the only
evidence.

The chart now refuses this at render time, per dependency:

```
networkPolicy: mode=enforce renders a default-deny policy, and these
dependencies are enabled with no egress peer to reach them, so the release
would install and then fail at runtime: dependencies.postgres.enabled=true
but dependencies.postgres.host is empty (...)
```

If you see that, the fix is in the message. Set the host, and set either
`namespace` (in-cluster) or the `cidr` block (managed endpoint).

### Your provider egress needs a route too

The rule most easily missed, because nothing fails until a user sends a
request. There is **no rule for TCP 443** unless you enable the egress proxy:

```yaml
networkPolicy:
  egress:
    proxy:
      enabled: true
      host: squid.egress.svc.cluster.local
      port: 3128
      namespace: egress
      podSelector:
        app.kubernetes.io/name: squid
```

Without it the gateway starts, passes readiness against its databases, reports
healthy, and fails every request that has to reach `api.openai.com`. The
worker gets the same rule because external eval plugins — Langfuse, LangSmith,
Datadog, Confident AI — are its job and all of them are off-cluster.

If you serve only in-cluster models and genuinely need no route out, use
`profile=development` with `mode=disabled` rather than leaving the pods with no
egress path.

## 4. Postgres: pick exactly one target mode

`enterprise` requires one and refuses both or neither.

**Selector mode** — Postgres runs in your cluster:

```yaml
networkPolicy:
  postgres:
    selector:
      enabled: true
      namespace: database          # where the Service actually lives
      matchLabels:
        app.kubernetes.io/name: postgres
dependencies:
  postgres:
    enabled: true
    host: postgres.database.svc.cluster.local
    port: 5432
    namespace: database            # keep equal to the selector namespace
```

`namespace` may not be empty. An empty namespaceSelector matches *every*
namespace, so the rule would degrade into a cluster-wide allow to anything
carrying the matched pod labels. The chart refuses it for that reason.

`database` is the chart default, not a detected value. Confirm where your
Postgres Service lives before your first install — a wrong namespace here is
a gateway that starts and then fails readiness on its database connection.

**CIDR mode** — managed Postgres (RDS, Cloud SQL) outside the cluster:

```yaml
networkPolicy:
  postgres:
    selector:
      enabled: false
    cidr:
      enabled: true
      cidrs: ["10.44.0.0/16"]
      port: 5432
dependencies:
  postgres:
    enabled: true
    host: mydb.abc123.eu-west-1.rds.amazonaws.com
    port: 5432
```

CIDR mode is pure IP and port and does not depend on cluster DNS resolving
your endpoint to something inside a namespace you can select.

## 5. SSO and email need a declared path

Under `enterprise`, enabling a feature that talks to something off-cluster
without giving it a way out is refused:

```
D-2b networkPolicy: profile=enterprise refuses external features without an
egress proxy unless each declares its own egress target, and these have
neither: features.sso (declare serviceTargets.sso.namespace)
```

Two complete answers: enable the egress proxy, or declare
`serviceTargets.sso.namespace` / `serviceTargets.resend.namespace` for the
namespace that terminates that traffic. The rule is "no egress path at all",
not "no proxy" — routing egress yourself is fine.

Left unset, SSO redirects the operator to an issuer the pod cannot reach and
login fails with a timeout that reads like an outage rather than a
misconfiguration.

## 6. Verify after installing

```bash
# Policies exist and carry the peers you expect
kubectl -n nexus get networkpolicy
kubectl -n nexus describe networkpolicy nexus-gateway

# The gateway agrees it can reach its dependencies
kubectl -n nexus port-forward svc/nexus 8080:8080
curl -s localhost:8080/readyz | jq
```

`/readyz` returning `"ready": true` means Postgres is reachable and migrated.
It does **not** tell you provider egress works — that has no readiness check,
and a missing 443 route shows up only as failing requests. Send one real
completion through the gateway before calling the install done.

To see the rendered policy set without installing anything:

```bash
helm template nexus deploy/helm/nexus -f your-values.yaml \
  | yq 'select(.kind == "NetworkPolicy")'
```

## 7. Rehearse the change on a running cluster

Turning enforcement on for an existing release is the change most likely to
cause an outage, because a peer list that is merely incomplete looks fine
until traffic hits the gap.

Upgrade with `--atomic` so a release that fails its readiness gate rolls back
instead of leaving you half-enforced:

```bash
helm upgrade nexus deploy/helm/nexus -f your-values.yaml \
  --atomic --wait --timeout 10m
```

Do it in staging first, with the same topology. `values-staging.example.yaml`
keeps the same policy shape as production for exactly this reason — a
rehearsal against a different peer set does not tell you anything about
production.

Note the limit of `--atomic`: it reacts to a failed rollout, not to a wrong
peer list that happens to pass readiness. A policy that blocks provider
egress but not Postgres will install, pass, and fail user requests.
See [`customer-self-hosted-upgrade-rollback.md`](customer-self-hosted-upgrade-rollback.md).
