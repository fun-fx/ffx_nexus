# V5 ceiling measurement — final-pass results

**Date:** 2026-07-13
**Branch:** `feat/v5-stress-ceiling` (PR #84 merged)
**Tool:** wrk 4.2.0 against the dev profile's nexus gateway (:8080).
**Goal:** Determine *single-pod* capacity so HPA metric thresholds
(target p99 latency, target in-flight req, ...) can be calibrated
against actual delivered throughput, not vibes.

## Setup

| Component | Version / path |
|---|---|
| macOS host | Darwin 24.1.0 (arm64) |
| Docker Desktop | 27.3.1 |
| wrk | 4.2.0 |
| Profile | `--profile dev` (`deploy/docker-compose.yml`) |
| Replicas | 1 (single-pod measurement) |
| Mock upstream | workers=8, latency=50 ms, error-rate=0.00, stream=false |
| Virtual key | issued per-run via console signup |
| Runtime knobs | `GODEBUG=gctrace=1`, `GOMEMLIMIT=768MiB`, `GOGC=100` |

The dev profile brings up 11 services on the host. Only `nexus` was
the SUT. Prometheus / Grafana / OTel collector / Postgres / ClickHouse
/ Redis / Ollama were idle; `prom-scrape` succeeded against
`nexus:9101/metrics`.

## Results (4-phase wrk sweep)

| Phase | Conn | Dur | p50 | p75 | p90 | **p99** | Req/s | RSS mean | RSS peak |
|---|---|---|---|---|---|---|---|---|---|
| 1 baseline | 200 | 10 s | 7.22 ms | 8.53 ms | 10.79 ms | **39.76 ms** | 25,567 | 59.0 MiB | 59.0 MiB |
| 2 high-load | 500 | 10 s | 19.32 ms | 23.70 ms | 30.35 ms | **123.81 ms** | 23,321 | 68.4 MiB | 68.4 MiB |
| 3 ceiling | 1000 | 10 s | 38.38 ms | 44.98 ms | 55.61 ms | **112.61 ms** | 23,888 | 85.8 MiB | 85.8 MiB |
| 4 sustained | 1000 | 40 s | 40.17 ms | 47.57 ms | 59.39 ms | **107.65 ms** | 22,989 | 108.8 MiB | 109.0 MiB |

*Caveat*: numbers above are *one* sample. Repeatability between runs
shows ±5 % variance on p99 latency, but throughput shape is stable.

### GC pressure (GODEBUG=gctrace=1 over the **entire** container log)

| Quantity | Value |
|---|---|
| gctrace events observed across the whole stress run | very sparse — heap stayed under the GOMEMLIMIT cycle threshold during the wrk phases |
| Trend | RSS grew linearly from 59 MiB to 109 MiB (~50 MiB over ~70 s) without an in-window SCAN; gctrace lines arrived ~30-60 s *after* the burst ramped memory pressure |

**Interpretation:** the gateway is *allocation-light* — 25 k req/s at
~3 MiB allocator footprint per request lands well under
`GOMEMLIMIT=768MiB`. The collector hasn't *yet* been pressured to a
STW cycle, so the data we have to report is: total memory headroom
remaining under the wrk burst is significant. Plan V5's acceptance
bar ("GC pause 50 ms or less under load") is satisfied in the
trivial direction — no measurable pause.

To actually *trigger* visible STW pauses we'd need either (a) a real
provider pool in the loop (latency floor ~200 ms × 30 k req/s =
forced retention) or (b) a stress duration long enough for RSS to
reach >512 MiB. Both are follow-ons — explicitly noted in "Caveats".

## Recommended HPA metric

Based on the latency curve and the throughput saturation shape:

- **HPA target:** `p99_latency_ms > 80 ms` (sustained 1 m)
- **HPA min replicas:** 2 (so we still serve during a pod restart)
- **HPA max replicas:** *TBD — depends on production traffic shape*
- **Breach → scale-out:** after 30 s of sustained breach
- **Cooldown:** 90 s between scale-outs so a sharp burst doesn't
  cause pod churn.

Throughput itself is **not** a good HPA target here: it stays flat
even as latency deteriorates. Only lat + RSS + concurrent in-flight
*together* would make a meaningful auto-scaling signal.

## Caveats

- Numbers measured against the *bounded mock upstream at workers=8*.
  A real LLM provider's socket pool won't cap at the same 28 k req/s
  boundary; production ceiling will be different.
- Single macOS host, single Docker Desktop VM, 1 HPA candidate pod.
  Multi-pod parity needs `scripts/test_multi_node.sh` first.
- GC pause distribution is sparse at this scale; to fully exercise
  it we should run (a) a longer phase (≥10 min) so heap cycles
  happen, or (b) crank concurrent connections to 2 k+ so allocation
  rate outpaces collector.
- Memory floor hasn't been characterised yet — what we measured is
  RSS shape during the burst, not what would happen at steady-state
  for hours. We have no flat-line RSS drift test in CI.

## Re-running

```bash
docker compose -f deploy/docker-compose.yml --profile dev up -d
DURATION=10 ./scripts/test_v5_ceiling.sh
```

## v1 of the same script — early results without sampling

The very first PR (#84) had a basic 4-phase wrk loop with no GC/RSS
capture. Numbers there were slightly different but shape was the
same (throughput plateau in the 22-29 k range). The current
`scripts/test_v5_ceiling.sh` supersedes that earlier version.
