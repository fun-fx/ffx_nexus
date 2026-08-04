# Model benchmarks (PrimeIntellect hosted evaluations)

> Status: v1beta, ships enabled wherever Postgres is configured but
> inert until an operator stores a provider API key. As of v1beta
> the most-recent settled benchmark per model is also blended into
> the quality-aware router's signal (see "Routing blend" below).
> No in-cluster eval compute is added — the dataset and its
> scoring code run on the provider's infrastructure.

A benchmark answers a different question from an eval plugin.

| | Eval plugin | Model benchmark |
|---|---|---|
| Input | one trace that already happened | a model plus a dataset |
| Asks | "how good was this answer?" | "how good is this model at this task?" |
| Trigger | live traffic, sampled | an operator pressing Run |
| Duration | milliseconds | minutes to hours |
| Result | a score on `eval_scores` | an aggregate on `benchmark_runs` |

That difference is why a benchmark is not an `EvalPlugin` manifest.
A plugin renders `spec.send.payload` from a trace; a benchmark has no
trace to render, so the per-trace dispatcher has nothing to dispatch.

## What runs where

Nexus launches the run, polls it, and stores the aggregate. Everything
expensive — spinning up a sandbox, driving the dataset through the
model, running the verifier — happens in PrimeIntellect's account. This
keeps the config-only property of the eval stack: no Python sidecar, no
GPU, no queue lands in the Nexus cluster.

```
console → POST /api/eval/benchmarks
            ├─ (optional) mint a virtual key scoped to the one model
            └─ POST api.primeintellect.ai/api/v1/hosted-evaluations
                                            │
   provider sandbox ── inference ──────────►│  nexus.example.com/v1
                     (only when via_gateway) │  (the minted key)
                                            │
poller  → GET  /api/v1/evaluations/{id}  ──► avg_score, status, viewer_url
            └─ UPDATE benchmark_runs; revoke the key once settled
```

## Two ways to route inference

`via_gateway` decides what the number actually means.

- **`via_gateway: true`** — the provider is handed
  `api_base_url = <your gateway>/v1` and a freshly minted Nexus virtual
  key. The score then describes **what Nexus serves**: your routing
  weights, your cache, your provider fallbacks and your credential
  choice are all inside the measurement. This is the mode worth having.
- **`via_gateway: false`** — the provider serves the model itself. The
  score describes the model in isolation, which is useful as a baseline
  but says nothing about your deployment.

Gateway routing requires `NEXUS_PUBLIC_GATEWAY_URL`, because the
provider's sandbox has to reach a URL we can name. When it is unset the
console disables the option and says why rather than failing on submit.

### The virtual key

One key per run, created at launch and revoked when the run settles
(also on cancel, on delete, and on a launch the provider refused):

- scoped to the single model under test, so the sandbox has no reach
  beyond what the run needs;
- rate limited to 120 rpm, which bounds a retry-happy harness;
- named `benchmark <first 8 chars of the run id>` so it is identifiable
  in the Keys page and in the audit log.

Revocation is best effort and logged, never fatal — a leaked key is
bounded by its scope, but a failed revoke must not turn a successful
run into an error.

## Prerequisites the operator owns

These are the two things that will stop a first run, and neither is a
Nexus configuration problem:

1. **A published environment.** An *environment* is a dataset plus the
   Python code that grades an answer (`load_environment()` returning a
   `vf.Environment`). `environment_ids` resolves against environments
   the authenticated account can write to, so a public Hub slug such as
   `primeintellect/gsm8k` returns 404 for an account that does not own
   it. Publish your own with `prime env push`. There is no public
   environments API, which is why the console asks you to paste a slug
   instead of offering a picker.
2. **A funded Prime wallet.** Runs bill per token, plus sandbox compute
   when inference points at an external endpoint. A zero balance fails
   the launch with the provider's own 402.

Start with 5 examples and 1 rollout. That is enough to prove the wiring
before spending on a measurement you would actually cite.

## Cost guardrails

- `num_examples × rollouts` is capped at 2000 samples server-side
  (`benchmark.MaxTotalSamples`), and the console shows the running total
  next to the Launch button.
- `num_examples: -1` (the provider's "every example" sentinel) is
  refused, because an unbounded run cannot be checked against the cap.
- `timeout_minutes`, when set, must be between 120 and 1440.

## Credential storage

The provider key lives in the same encrypted `eval_plugin_keys` vault
as plugin credentials, under the reserved name
`benchmark-primeintellect`. It therefore inherits that table's
master-key encryption and its durability — including surviving a
deploy, which is the property those keys originally lacked. The value
is never read back by the API; `GET .../credential` reports only
whether one is stored.

## REST surface

All routes are admin-only. Launching spends money and, in gateway mode,
mints a credential that lets an external sandbox call the gateway;
neither belongs behind a read-scoped role.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/eval/benchmarks` | list runs, plus `gateway_routing_available` and the sample cap |
| `POST` | `/api/eval/benchmarks` | launch a run |
| `GET` | `/api/eval/benchmarks/{id}` | one run |
| `POST` | `/api/eval/benchmarks/{id}/cancel` | stop it at the provider and settle the row |
| `DELETE` | `/api/eval/benchmarks/{id}` | forget the record (cancels first if still live) |
| `GET` | `/api/eval/benchmarks/{id}/logs` | the provider's sandbox log |
| `GET` | `/api/eval/benchmarks/models` | the provider's inference catalogue, for the picker |
| `POST` | `/api/eval/benchmarks/refresh` | force a poll pass |
| `GET`/`PUT`/`DELETE` | `/api/eval/benchmarks/credential` | provider API key |

A launch the provider refused still returns the recorded row alongside
the error, so the console can show a failed run rather than only a
toast that disappears.

## Status model

Our five states collapse the provider's richer machine, whose raw
string is kept in `external_status` so no detail is lost:

| Nexus | Provider |
|---|---|
| `pending` | `PENDING` |
| `running` | `RUNNING`, `PROCESSING`, and any string we do not recognise |
| `completed` | `COMPLETED` |
| `failed` | `FAILED`, `TIMEOUT` |
| `cancelled` | `CANCELLED` |

An unknown status maps to `running` rather than `failed` on purpose. The
provider can add intermediate states, and treating a new one as terminal
would abandon a run that is still progressing — and still billing.

The poller runs every minute on every replica. The work is idempotent —
a duplicate poll costs one provider read — so no leader election is
needed. Refresh in the console forces a pass for an operator who does
not want to wait for the tick.

## Routing blend

v1beta blends the most recent settled benchmark per model into
the quality-aware router's `Quality` signal — the same axis the
router uses to rank candidate models for an alias. Without a
benchmark row, judgement is unchanged: a model with no benchmark
contribution still uses its judge-only Quality.

### What gets blended

For every model in `benchmark_runs` with `status='completed'` and
a non-NULL `avg_score`, only the **most recent settled row**
(`ORDER BY completed_at DESC`) contributes. Stale rows are kept
for audit but are not consulted. This means re-running a
benchmark automatically supersedes the prior result without
any operator cleanup.

### How the blend is computed

The `CombinedStatsProvider` in `internal/router/bench_provider.go`
takes the judge-only `Quality` from the existing `StatsProvider`
and replaces it with:

```
freshness   = 2^(-(now - completed_at).Hours() / halfLife.Hours())
wBench      = clamp(BenchmarkWeight * freshness, 0, 1)
newQuality  = judge * (1 - wBench) + bench.AvgScore * wBench
```

`BenchmarkWeight` and `halfLife` are env vars so they survive a
config push without invalidating the router's in-memory stats
cache:

| Env var | Default | Effect |
|---|---|---|
| `NEXUS_ROUTE_W_BENCH` | `0.5` | 0 disables; 0.5 equal influence; 1 = benchmarks dominate |
| `NEXUS_ROUTE_BENCH_HALF_LIFE` | `168h` (7 days) | time after which a benchmark's contribution halves; `0` disables decay (last-known-wins) |

`weights` are clamped to `[0,1]` at construction so a typo cannot
invert signals. The router's `QualitySamples` is also incremented
by 1 per blended row so dashboards report "we have external data
here" alongside the judge count.

### Availability

A failing benchmark query returns the judge-only stats
unchanged — Nexus keeps routing even if Postgres is degraded.
`BenchEnabled` in the routing snapshot flips on only when a
Postgres-anchored stats store is configured AND `NEXUS_ROUTE_W_BENCH`
is positive. Routing remains alive when the snapshot reports
`BenchEnabled: false`.

### Operator surface

The `/api/eval/config` snapshot extends `routing` with three
read-only fields:

- `bench_enabled` — derived: true when Postgres routing stats
  plus a positive bench weight are both wired.
- `bench_weight`  — the live value of `NEXUS_ROUTE_W_BENCH`.
- `bench_decay`   — the live value of `NEXUS_ROUTE_BENCH_HALF_LIFE`
  as a human duration string.

Both vars are also added to `restart_required` so the console
banner tells an operator that rotating the blend requires a
pod restart. There is no PATCH surface for them: rotating
during runtime would invalidate every cached `Stats` value and
the failure mode (sudden, unlogged) would be hard to diagnose in
production.

## Configuration

| Env var | Effect |
|---|---|
| `NEXUS_POSTGRES_URL` | required; without it the routes answer 503 with that explanation |
| `NEXUS_PUBLIC_GATEWAY_URL` | enables `via_gateway`; the gateway base without `/v1` |
| `NEXUS_ROUTE_W_BENCH` | benchmark blend weight (`0.0`–`1.0`, default `0.5`). `0` disables the bench layer entirely |
| `NEXUS_ROUTE_BENCH_HALF_LIFE` | decay half-life for benchmark influence (default `168h`); `0` disables decay |

No env var carries the provider token — it is pasted in the console and
stored encrypted.
