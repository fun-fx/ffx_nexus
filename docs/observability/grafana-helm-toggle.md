# Grafana Helm toggle (V5)

The Nexus Helm chart ships a plugin-style toggle for the Grafana observability
stack. Operators enable dashboards and alerting through Helm values, the same
way `config.metabase.url`, `config.otlp.endpoint`, or
`spec.service.type` is selected for eval plugins.

## What `config.grafana.enabled: true` does

1. Renders three bundled reference `GrafanaDashboard` CRs under names
   `nexus-01-overview`, `nexus-02-llm-spend`, `nexus-03-eval-quality`.
   The dashboard JSON files live in `deploy/helm/nexus/files/grafana-dashboards/`
   and are mounted into ConfigMaps by the chart's
   `templates/grafana-dashboards.yaml`.
2. The Grafana operator (the `grafana.integreatly.org/v1beta1` CRD managed by
   Cozystack's `cozy-grafana-operator`) reconciles the CRs into the cluster's
   Grafana instance. Each instance whose Pod has the label
   `dashboards: grafana` will pick them up.
3. If `config.grafana.alertsEnabled: true` AND `config.grafana.folderUID` is
   set, the chart also renders a `GrafanaContactPoint` plus a
   `GrafanaAlertRuleGroup` for the OTLP exporter 4xx sustained rule.

## Helm values reference

```yaml
config:
  grafana:
    enabled: false             # master toggle (off by default — zero-dep)
    dashboardsEnabled: true    # the 3 GrafanaDashboard CRs
    alertsEnabled: false       # GrafanaContactPoint + AlertRuleGroup
    folder: Nexus              # display name in Grafana's sidebar
    folderUID: ""              # required for alertsEnabled: true; discover
                               # via `curl -u admin:$PASS http://<grafana>/api/folders | jq`
    instanceSelector:
      matchLabels:
        dashboards: grafana    # override if your Grafana CR uses a different label
    publicUrl: ""              # surfaced on the console's Spend / Quality pages
    alertsWebhook: ""          # defaults to http://failover-echo.tenant-nexus.svc:8080/alert
    extraDashboards: {}        # map of filename -> JSON body for additional dashboards
```

## Enable Grafana dashboards only (no alerts)

```sh
helm upgrade nexus deploy/helm/nexus -n tenant-nexus \
  --set config.grafana.enabled=true \
  --reuse-values
```

The Three bundled references (overview / spend / eval-quality) appear in
Grafana's `Nexus` folder within a couple of `resyncPeriod` cycles (5m).

## Enable dashboards + alerts

```sh
# 1. Discover the existing folder UID from the running Grafana instance.
#    (Skip if you've never applied the grafana dashboard CR before — Grafana
#    creates the folder on first reconcile.)
GRAFANA=$(tailscale ip -4 grafana)
curl -s -u admin:$PASS "http://$GRAFANA:3000/api/folders" \
  | jq '.[] | select(.title == "Nexus") | .uid'

# 2. Set the folder UID and alerts webhook in your override file.
cat > overlays/grafana.yaml <<EOF
config:
  grafana:
    enabled: true
    dashboardsEnabled: true
    alertsEnabled: true
    folderUID:          # paste the UID from step 1
    alertsWebhook: https://hooks.slack.com/services/...
EOF

helm upgrade nexus deploy/helm/nexus -n tenant-nexus -f overlays/grafana.yaml
```

## When NOT to use this toggle

- The chart assumes a `grafana.integreatly.org/v1beta1` CRD is installed in
  the cluster. Cozystack ships this by default; vanilla Kubernetes does not.
  In that case, keep `config.grafana.enabled: false` and apply the dashboards
  by hand from `deploy/cozystack/09-grafana-dashboards.yaml` instead.
- The chart does NOT deploy Grafana itself. Grafana is a shared service owned
  by the platform / ops team (the same Grafana instance serves Cozystack
  itself). If you want a private Grafana, deploy one out-of-band and point
  `config.grafana.instanceSelector.matchLabels` at its selector.

## Why this is a Helm toggle and not a runtime env

Compare with `internal/evalplugin/types.go`:

| Toggle type | Where it lives | Why |
|---|---|---|
| Rule-based | `config.otlp.endpoint`, `config.metabase.url` | Connects to an external sink; needs URL + creds in cluster |
| Plugin manifest | `spec.service.type: langfuse \| langsmith \| braintrust \| …` | Vendor adapter ships in the binary; per-tenant secret |
| **Kubernetes CRs** | `config.grafana.*` (this doc) | Resource lives in another operator's domain (the Grafana CRD); we just generate manifests |

The toggle exists so the chart is the single source of truth for "which
dashboards ship with a given release of Nexus". The dependencies are still
explicit: a `grafana-operator` must be present, and the underlying Grafana
must be running. The toggle controls *visibility*, not *lifecycle*.

## Bringing Grafana back up after a 1GB → 5GB PVC resize

If your `grafana-db-*` CloudNativePG instance has been crash-looping on
`Detected low-disk-space condition` because its PVC was sized at 1Gi (the
default), the toggle here cannot help — that's about *Grafana's storage*
sitting underneath the CRs we render. Patch the underlying StatefulSet in
the ops repo `deploy/cozystack/00-tenant.yaml`:

```yaml
# CNPG pools — full override for grafana-db-*
spec:
  instances: 2
  storageConfiguration:
    size: 5Gi
```

Apply with `kubectl apply -f 00-tenant.yaml` and CloudNativePG will roll
the new PVC, the second instance will start, and the existing Grafana
Deployment will reattach. See `deploy/observability/otlp-no-traffic-runbook.md`
for the wider "Grafana is up but no data" troubleshooting.

## Onboarding scenarios for a fresh Helm install

A new tenancy cloning this chart for the first time has three
realistic Grafana setups. Pick the one that matches your cluster
and apply the values accordingly. The chart will *not* auto-detect
which case you're in — that decision is yours.

### Scenario A — I have a Cozystack-managed Grafana already

Default Cozystack tenants ship with `cozy-grafana-operator` already
installed. The chart works out of the box:

```yaml
config:
  grafana:
    enabled: true
    dashboardsEnabled: true
    alertsEnabled: false   # until you discover folderUID
    instanceSelector:
      matchLabels:
        dashboards: grafana   # cozy's label
  publicGrafanaUrl: "https://grafana.<your-tenant-host>"
```

After `helm upgrade`, three dashboards appear in Grafana under the
`Nexus` folder within ~5m. The Sidebar's "Open in Grafana" link is
live.

### Scenario B — I have Grafana but it is not managed by the grafana-operator

The chart's CRs require the `grafana.integreatly.org/v1beta1` CRD.
If your Grafana was installed via Helm (bitnami/grafana, grafana/grafana)
without the operator, the CRs will be created but never reconciled
into the Grafana instance. Two options:

1. **Use the operator-aware path**: install `grafana-operator`
   (https://grafana-operator.github.io/grafana-operator/), point it
   at your Grafana, then enable the toggle as in Scenario A.
2. **Apply dashboards by hand**: keep `config.grafana.enabled: false`
   and use the stand-alone Grafana Dashboard provisioning system
   via `deploy/cozystack/09-grafana-dashboards.yaml` (apply
   out-of-band; the file is in this same repo and works against
   vanilla Grafana's `/etc/grafana/provisioning/dashboards/` mount).

The Sidebar's "Open in Grafana" link is still safe — it points at
your `publicGrafanaUrl`. Whether the dashboards inside light up
depends on which of the two paths you took.

### Scenario C — I do not have Grafana at all

Keep `config.grafana.enabled: false` *and* leave
`config.publicGrafanaUrl: ""`. The Sidebar omits the "Open in
Grafana" link entirely (the `ui-observability-link` component
returns nothing when the URL is empty). The chart stays dep-free.

If you later stand up Grafana in the cluster, follow Scenario A or B
as appropriate; no upgrade-unsafe state needs cleaning up.

### Deciding which scenario you're in

```sh
# Is the grafana-operator CRD installed?
kubectl get crd | grep grafana.integreatly.org
# -> Scenario A or B (depends on point 1 / 2 below).

# Is there a Grafana instance selected by the default label?
kubectl get grafanadashboard -A 2>&1 | grep -E 'NAME|nexus-'
kubectl get pod -A -l dashboards=grafana
# -> A if both commands return matches; B if only the first returns.

# Nothing matched either -> Scenario C.
```

The chart's `NOTES.txt` emits a banner with the same diagnostic
after every `helm install`/`helm upgrade` so the operator can
correlate Helm output against the cluster state without greppping.

## Optional Basic-Auth proxy for the *Open in Grafana* link

When Grafana ships with `auth.anonymous.enabled=false` (the Cozystack
default), the console's *Open in Grafana* deep-link bounces the
operator through Grafana's login page — or, if the operator never logs
in, lands on the Grafana root and looks like *404 Not Found*. Setting
`config.grafana.authProxy.enabled: true` deploys a tiny Caddy sidecar
in `tenant-nexus` that:

1. Reads `user`/`password` from the platform Secret (`grafana-admin-password`
   by default — same Secret the Grafana operator already mounts inside
   its pod).
2. Exposes a Tailscale Ingress with the operator's MagicDNS name (e.g.
   `nexus-grafana.tail-nexus.ts.net`).
3. On every request, attaches `Authorization: Basic <b64>` so Grafana
   sees a logged-in `admin`.

The operator's browser transparently reaches the dashboard with no
interaction — they never type the admin password. The trust boundary
is identical to the upstream `grafana-ts` Ingress: anyone who can
resolve the new MagicDNS name on the Tailscale tailnet can read
dashboards, because the proxy authenticates as `admin`. We do NOT
enable Grafana's `auth.anonymous` because that would expose
dashboards publicly (anyone with the URL).

```yaml
config:
  grafana:
    ...
    authProxy:
      enabled: true                     # master switch
      host:     nexus-grafana           # MagicDNS label, becomes
                                        # nexus-grafana.<tailnet-suffix>
      upstreamService: grafana-service.tenant-root.svc.cluster.local:3000
      adminSecret: grafana-admin-password
      image:
        repository: caddy
        tag:        2-alpine
      runAsUser:    0
      resources:
        requests: { cpu: 10m,  memory: 32Mi }
        limits:   { cpu: 100m, memory: 64Mi }
```

Pair this with `config.publicGrafanaUrl: "https://<host>.<tailnet-suffix>"`
so the console's Sidebar link points at the proxied hostname. The
console's `/api/ui/observability` reads `NEXUS_PUBLIC_GRAFANA_URL`
verbatim — the deep-link `/d/nexus-01-overview/nexus-01-overview`
appended to the URL surfaces the same dashboard regardless of whether
Grafana is reached directly (legacy) or via the proxy.

### Why a sidecar proxy instead of `auth.anonymous`

- **Tailscale stays the trust boundary.** Anonymous access would expose
  every dashboard to anyone with the URL; the proxy only admits Tailscale
  members and authenticates them as our `admin` (read-only by intent:
  the proxy doesn't expose a way to mutate Grafana through it).
- **No password typed by humans.** Operators never see the admin
  password. Helm + Secret pulls it from the cluster Secret.
- **Same code path for everyone.** Other Nexus deployments reuse the
  same chart and the same proxy; the only knobs an operator needs
  are `host` (the MagicDNS label) and `upstreamService` (their
  in-cluster Grafana location).

### Diagnostic post-`helm upgrade`

```sh
# Did the proxy get rolled?
kubectl -n tenant-nexus get deploy,svc,ingress -l app.kubernetes.io/component=grafana-auth-proxy

# Is the proxy pod ready?
kubectl -n tenant-nexus logs -l app.kubernetes.io/component=grafana-auth-proxy --tail=50

# Does the Ingress resolve on Tailscale?
nslookup nexus-grafana.tail-<your-tailnet>.ts.net
# or `tailscale status | grep nexus-grafana` if you have the cli.

# Does the deep-link load?
curl -v "https://nexus-grafana.tail-<tailnet>.ts.net/d/nexus-01-overview/nexus-01-overview"
# -> 200 OK with the dashboard HTML. If you see Grafana's login page,
#    the proxy's Authorization header isn't reaching Grafana — verify
#    the upstreamService FQDN resolves from the proxy pod's namespace.
```
