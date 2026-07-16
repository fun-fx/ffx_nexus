# docs/observability — V0.5 observability docs

This folder ships with V0.5.0 and contains:

| File | Use it for |
|------|------------|
| [`v0.5-architecture.md`](./v0.5-architecture.md) | End-to-end 3-layer pipeline diagram (Mermaid), wire-format notes, acceptance tests. Onboarding anchor for new engineers and on-call. |
| [`otlp-4xx-runbook.md`](./otlp-4xx-runbook.md) | `nex-otlp-4xx-sustained` alert — collector rejects the envelope. |
| [`otlp-network-runbook.md`](./otlp-network-runbook.md) | `nex-otlp-network-flapping` alert — collector unreachable (DNS / dial / TLS). |
| [`otlp-no-traffic-runbook.md`](./otlp-no-traffic-runbook.md) | `nex-otlp-no-traffic` alert — recorder wedged, not failing. |

All Grafana unified alerting rules referenced here ship in
`ffx_nexus_ops/deploy/cozystack/10-grafana-alerting.yaml`. Pair this
folder with that manifest — they evolve together.
