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
