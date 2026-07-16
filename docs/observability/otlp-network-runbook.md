# OTLP network runbook — `Nexus OTLP exporter — collector unreachable`

This runbook covers the alert:

```
nex-otlp-network-flapping
  expr: max(nexus_otlp_export_failures_total{reason="network"}) > 0
  for: 3m
  severity: warning
```

## Symptom

Nexus is failing OTLP HTTP POSTs with **network-class** errors:
DNS resolution failures, dial refused, TLS handshake resets, or
connection reset by peer. The receiver-side never sees these.

The Nexus gateway pod log shows, **without** any 4xx/5xx body prefix:

```
WARN msg="otlp export failed"
  err="Post \"http://otel-collector:4318/v1/traces\": dial tcp 10.244.x.x:4318: connect: connection refused"
  reason=network
```

## Common root causes (in order)

### 1. OTLP collector pod is missing or crash-looping

```bash
kubectl -n tenant-nexus get pod -l app.kubernetes.io/name=otel-collector
kubectl -n tenant-nexus logs -l app.kubernetes.io/name=otel-collector --tail=20
```

Look for OOM kills, panic stack traces, `loglevel: error` lines.

**Fix**: scale Deployment back to >=1 replicas. If the failure is a
chronic crash on startup, check the manifest:

```
deploy/cozystack/07-otel-collector.yaml
```

The Debug exporter (we use this in our cluster) is loud but not the
source of crashes. If we accidentally ship a misconfigured
`service::pipelines::traces::exporters`, look for typo'd class names
or missing `tls::insecure: true` blocks for in-cluster destinations.

### 2. Tailscale link flap (only relevant when collector is *external*)

Tailscale MagicDNS or `nip.io` resolution can have transient flakes
during key rotation. If we move the collector outside the cluster,
check:

```bash
tailscale status | grep otel-collector
tailscale ping otel-collector.tail7d361a.ts.net
```

**Fix**: ensure `nip.io` is the resolved form inside the pod's
containerd (Talos containerd can't reach `*.ts.net`), OR run the
collector inside the cluster and have Nexus reach it via the
`tenant-nexus`/'s kube-dns.

### 3. Service mesh sidecar is wedged

If we install a Cilium sidecar for the gateway, the L7 egress
policies can drop OTLP traffic during a policy reconciliation. Check:

```bash
kubectl -n tenant-nexus get ciliumnetworkpolicy -o yaml | head -50
```

**Fix**: temporarily `kubectl -n tenant-nexus scale deploy otel-collector --replicas=2`
to see if two pods both 4xx — if so, it's the policy, not the pod.

## Network-bucket vs 4xx-bucket diagnostic

Of our three alert rules, `network` is the easiest to disambiguate
because the failure **never reaches the collector**. The other two
buckets (`http_4xx`, `http_5xx`, `other`) all carry a `body_prefix` in
the Nexus log.

```
log line includes body_prefix?    bucket
─────────────────────────────────────
yes                                http_4xx / http_5xx / other (depending on code)
no                                 network (DNS / dial / TLS / conn-reset)
```

## Acceptance after fix

Within 3 minutes (default `repeat_interval`):

* `nexus_otlp_export_failures_total{reason="network"}` flattens.
* `nexus_otlp_export_traces_total` resumes incrementing per chat.
* Grafana alert goes from `firing` → `resolved`.

## Preventive

* V0.6 plan: enable OTLP collector `file_storage` extension so on
  pod restart the collector drains its queue rather than losing
  in-flight batches. Today (V0.5) we accept this gap.
* Run `kubectl get pod -l app.kubernetes.io/name=otel-collector -o
  wide --watch` as part of your on-call dashboard so the collector
  is always visible.
