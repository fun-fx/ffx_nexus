# OTLP no-traffic runbook — `Nexus OTLP exporter — no traffic seen for 10m`

This runbook covers the alert:

```
nex-otlp-no-traffic
  expr: |
    sum(rate(nexus_otlp_export_traces_total[10m])) == 0
    and
    sum(rate(nexus_gateway_requests_total[10m])) > 0
  for: 10m
  severity: warning
```

## Symptom

Grafana sees Nexus exporting **zero traces** for 10 minutes, **and** the
gateway is still serving real traffic. So the exporter pipeline is not
draining — but the gateway hot-path itself is healthy.

## What this alert is *not*

This alert is **not** a "Nexus is down" alert — that one is via the
gateway pod's `up{job="nexus"}==0` for 5m, served by vmagent's
Kubernetes SD and is a separate Grafana rule.

This alert is also **not** a `network` failure — if all recordings are
network-failing, the `nex-otlp-network-flapping` alert above will fire
first. So if you see `no-traffic` firing in isolation, the gateway is
successfully *writing* traces into its in-memory recorder, but the
flush is **never leaving**.

## Common root causes (in order)

### 1. Recorder channel is wedged — non-blocking `select` is faulty

```go
select {
case r.ch <- t:
default:
    // dropped — should drop counter
}
```

If a regression broke this branch, every `Record` call dropped the
trace. Verify by checking the drop counter:

```bash
curl -s http://<gateway>:9101/metrics | grep ^nexus_otlp_buffer_drops
```

If that counter increases, the bug is in `internal/observability/multi.go` /
`internal/observability/otel.go`. Roll back to last known-good image.

### 2. MultiRecorder fan-out is misconfigured

`compose.go` wires OTLP only if `NEXUS_OTLP_ENABLED=1` is set in the
configmap. We've seen engineers accidentally set it as `NEXUS_OTLP=true`
without the dedicated flag handler:

```go
if os.Getenv("NEXUS_OTLP_ENABLED") != "1" {
    opts.OTLP = nil
}
```

**Fix**: ensure the configmap has the flag set verbatim. Use:

```bash
kubectl -n tenant-nexus exec -it $POD -- printenv | grep OTLP_ENABLED
```

Expected: `NEXUS_OTLP_ENABLED=1`. If it returns a different value
(`true`, missing, `0`), patch the configmap and roll.

### 3. Flush ticker never fires

The OTLPRecorder flush ticker is a 2-second `time.Ticker`. If a
debugger / SIGSTOP / cooperative fuzzer has frozen the gateway, the
ticker never drains.  V0.5 has no healthcheck for "ticker alive" yet.
Conventional reinstantiation migrate: `kubectl rollout restart deploy
nexus`.

## Acceptance

After fix, the alert auto-resolves. Specifically:

* `nexus_otlp_export_traces_total` rate over last 10 minutes becomes
  > 0 traces/second.
* The chart in **Nexus Overview** (`02-llm-spend.json` style, but
  noting trace counter instead of LLM tokens) crosses the threshold
  again.

## Preventive

* Add a periodic `RecordOTLPExportSuccess(0)` heartbeat in the
  *tick* goroutine that doesn't make any recording-side cost. This
  makes the absence of records detectable via a per-tick counter.
* Add a `livenessProbe` on `/metrics` that pings a tiny counter like
  `up{job=otel-collector}` we materialize in VictoriaMetrics.
