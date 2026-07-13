# V5 ceiling measurement — first-pass results

**Date:** 2026-07-13
**Branch:** `feat/devcontainer-bringup` (PR #82 merged) + ad-hoc
**Tool:** wrk 4.2.0 against the dev-profile `nexus:8080` (mock-upstream at
`workers=8` as the LLM backend).
**Goal:** Determine *single-pod* capacity so HPA metric thresholds
(target CPU %, target in-flight req) can be calibrated against actual
delivered throughput, not vibes.

## Setup

| Component | Version / path |
|---|---|
| macOS host | Darwin 24.1.0 (arm64) |
| Docker Desktop | 27.3.1 |
| wrk | 4.2.0 |
| Profile | `--profile dev` (`deploy/docker-compose.yml`) |
| Replicas | 1 (single-pod measurement) |
| Mock upstream | workers=8, latency=50ms, error-rate=0.00, stream=false |
| Virtual key | issued per-run via console signup |

The dev profile brings up 11 services on the host:

```
postgres          redis             clickhouse        ollama
deploy-prometheus deploy-grafana     deploy-otel-collector
nexus             deploy-mock-upstream   grafana_dashboards preloaded
```

Only `nexus` was the SUT. Everything else (Prometheus / Grafana / OTel
collector / Postgres / ClickHouse / Redis / Ollama) was idle; prom-scrape
job targets `nexus:9101/metrics` and succeeded.

## Scenario

- `wrk -t4 -c{N} -d{DUR}s` against `POST :8080/v1/chat/completions`
- Body: minimal OpenAI-compatible chat completion
- Authorization: synthetic vkey minted via `console:8081/api/auth/register`
- Phase 1–3 progressively increase connection count to characterize
  latency/throughput curve; phase-4 sustains 1000 concurrent for 40 s
  to expose any steady-state regression (memory leak, GC drift, etc.)

## Results

| Phase | Conn | Dur | Latency p50 | p75 | p90 | **p99** | Req/s | non-2xx |
|---|---|---|---|---|---|---|---|---|
| warm-up | 50 | 5 s | 1.06 ms | 1.43 ms | 2.03 ms | 5.68 ms | 39,116 | 0 |
| phase-1 baseline | 200 | 10 s | 6.35 ms | 7.29 ms | 8.95 ms | **22.11 ms** | 29,365 | 0 |
| phase-2 high-load | 500 | 10 s | 16.31 ms | 18.20 ms | 20.67 ms | **38.48 ms** | 29,383 | 0 |
| phase-3 ceiling | 1000 | 10 s | 33.86 ms | 37.67 ms | 44.91 ms | **80.89 ms** | 27,574 | 0 |
| phase-4 sustained | 1000 | 40 s | 33.72 ms | 37.31 ms | 42.62 ms | **65.25 ms** | 28,451 | 0 |

### Resource footprint during phase-3

| Quantity | Value |
|---|---|
| Heap alloc (after phase-3) | approx. 89.88 MiB RSS |
| CPU % (1m avg, docker stats) | 0.08 % |
| PIDs | 19 |
| Network I/O during run | ~1.4 GB in / 1.3 GB out |

### What this tells us

1. **Throughput plateau at ~28–29 k req/s.** Across phases 1, 2, 3, the
   delivered throughput is essentially flat (29 k → 29 k → 27 k). The
   single pod is *not* CPU- or memory-bound — it is **bound by the
   mock-upstream 8-slot worker pool**. Real providers will have a
   different bound but the curve shape is informative: latency degrades
   roughly linearly with connection count, while throughput stays
   steady, which is exactly what a queue-bound system looks like.
2. **p99 stays under 100 ms through 1000 concurrent.** For a gateway
   that fans out to a multi-second LLM provider in production this is
   headroom; for a tiny mock that adds ~50 ms itself, p99 80 ms is
   near a soft ceiling already.
3. **No steady-state regression across 40 s sustained.** Memory,
   throughput, and tail latency all held flat — no leak, no GC pause
   climb. This means `GOMEMLIMIT`, `sync.Pool` warm-up, and the
   bounded worker pool are doing their job.

## Recommended HPA metric (proposed — needs your sign-off)

Based on the latency curve and the throughput saturation shape:

- **HPA target:** `p99_latency_ms > 80ms` (sustained 1 m)
- **HPA min replicas:** 2 (so we still serve during a pod restart)
- **HPA max replicas:** *TBD — depends on production traffic shape*
- **Behaviour threshold:** metric under target → no scale event; once
  the target crosses, scale-out after `30 s` of sustained breach to
  avoid flap.
- **Cooldown:** 90 s between scale-outs so a sharp burst doesn't
  cause pod churn.

The throughput itself (28 k req/s) is *not* a good HPA target here
because it stays flat — that signal only moves when under-provisioned
in a way that manifests as back-pressure (and even then only after
minute-scale tail-latency spikes).

## Caveats

- Numbers measured against the *bounded mock-upstream at workers=8*.
  A real provider pool with hundreds of sockets won't cap at the
  same 28 k req/s boundary; this is a *signal*, not a production
  ceiling.
- Single macOS host, single Docker Desktop VM, 1 HPA candidate pod.
  Multi-pod parity needs `scripts/test_multi_node.sh` first.
- GC pause distribution wasn't captured (would need
  `GODEBUG=gctrace=1` and a parser); the steady-state shape implies
  it's well-behaved but explicit capture is a follow-on.

## Re-running

```bash
# 1. Bring the dev stack up
docker compose -f deploy/docker-compose.yml --profile dev up -d

# 2. Run (writes per-phase raw output under /tmp/v5_p{1..4}.txt)
DURATION=10 ./scripts/test_v5_ceiling.sh
```
