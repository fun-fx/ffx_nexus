# OTLP 4xx runbook — `Nexus OTLP exporter — sustained 4xx from collector`

This runbook covers the alert:

```
nex-otlp-4xx-sustained
  expr: max(nexus_otlp_export_failures_total{reason="http_4xx"}) > 0
  for: 5m
  severity: critical
```

## Symptom

The OpenTelemetry collector is rejecting Nexus trace envelopes with
HTTP 4xx. The receiver's response body is logged in the Nexus gateway
pod — `grep "otlp export failed"` will show the structured reason:

```
WARN msg="otlp export failed"
  err="otlp unexpected status code 400"
  count=…
  status="400 Bad Request"
  body_prefix="{\"code\":3,\"message\":\"readSpan.spanId: parse span_id:invalid length…\"}"
  payload_bytes=…
  payload_head="{\"resourceSpans\":[{…}]}"
```

## Background

There are two envelope-shape mistakes Nexus has historically hit, both
returning HTTP 4xx:

1. **Missing `resourceSpans` envelope** — pre-V3 Nexus sent a bare
   JSON array `[{"trace_id":"…"}]`, which the receiver rejected with
   `400 read span: expect { or n, but found [`.
2. **Non-hex / non-canonical `span_id` & `trace_id`** — Nexus upstream
   IDs are usually UUIDs (`xxxxxxxx-xxxx-…`) for `request_id`. Pre-#105,
   Nexus passed those through verbatim; the OTLP receiver requires
   8-byte / 16-byte hex, returning `400 parse span_id:invalid length`.

## Fix procedure

### Step 1 — identify the failure mode

```bash
POD=$(kubectl -n tenant-nexus get pod -l app.kubernetes.io/name=nexus -o jsonpath='{.items[0].metadata.name}')
kubectl -n tenant-nexus logs $POD --since=15m \
  | grep -E '"otlp export failed"|body_prefix' \
  | tail
```

### Step 2 — if the body_prefix shows `expect { or n, but found [`

You're hitting the missing-envelope failure (1). Nexus is shipping a
binary that predates PR #102. Roll forward:

```bash
# confirm what image tag the pod is on
kubectl -n tenant-nexus get pod $POD -o jsonpath='{..image}'
# expected: …:b7a3578… or later (any commit hash on or after #102)
```

If the image is older, trigger `cd-prod.yml` to roll to `main`.

### Step 3 — if the body_prefix shows `parse span_id:invalid length` or `parse trace_id:invalid length`

You're hitting failure (2). The fix landed in #104 (parent_span_id
trim) and #105 (general span_id/trace_id normalization). Verify the
deployed commit:

```bash
kubectl -n tenant-nexus get pod $POD -o jsonpath='{..image}' | awk -F: '{print $NF}'
# expected: …:83c343b…  or later
```

If the image is older than `83c343b446230a36b1b6f8b98b186a67f3569fd7`,
trigger `cd-prod.yml` to roll.

## Acceptance

After the new image rolls, the alert should auto-resolve within 3
minutes (default `repeat_interval`):

* `nexus_otlp_export_failures_total{reason="http_4xx"}` no longer
  increases.
* `nexus_otlp_export_traces_total` resumes incrementing.
* `failover-echo` POSTs shift from 4xx to silent (no firing alert).

## Preventive

* Add a smoke test in CI that constructs a minimal Nexus trace,
  marshals it via `otlpEnvelopeFromTraces`, and POSTs it against an
  ephemeral `otel-collector` container in the test pipeline. This
  catches envelope regressions before they reach production.
* Document the wire format in [`v0.5-architecture.md`](v0.5-architecture.md).
