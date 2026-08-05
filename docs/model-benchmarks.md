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
   environments API, so the console's picker is a curated list of slugs
   worth trying rather than a discovered one — it cannot tell you which
   of them your account can see, which is what the pre-flight validate
   below is for. Any slug can also be typed in by hand.
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
unchanged — Nexus keeps routing even if the configured backend
is degraded. `BenchEnabled` in the routing snapshot flips on
only when **a benchmark-bearing stats store** is connected
(Postgres *or* ClickHouse — both support the schema) AND
`NEXUS_ROUTE_W_BENCH` is positive. Routing remains alive when
the snapshot reports `BenchEnabled: false`.

### Backend selection

| Backend | Table | Probe query |
|---|---|---|
| Postgres (`NEXUS_POSTGRES_URL` set) | `benchmark_runs` per migration `012_benchmark_runs.sql` | `SELECT DISTINCT ON (model) ... ORDER BY model, completed_at DESC` |
| ClickHouse (`NEXUS_CLICKHOUSE_URL` set, no PG) | `benchmark_runs` per migration `007_benchmark_runs.sql` | `SELECT model, argMax(avg_score, completed_at) ... WHERE status='completed' GROUP BY model` |

The selection follows the same rule as the rest of the eval
stack: whichever backend the operator installed the plugin
table on wins. If both backends are configured, Postgres takes
precedence (the routed eval scores also live in Postgres when
present, so the operator gets a single source of truth). When
neither is wired, `BenchEnabled` stays false and routing falls
back to judge-only — silently, with a one-line "benchmark
blend not wired (needs Postgres or ClickHouse)" message in
the boot log so a misconfiguration is visible without a tracer
fire.

### Operator surface

The `/api/eval/config` snapshot extends `routing` with three
read-only fields:

- `bench_enabled` — derived: true when a benchmark-bearing
  stats store (PG or CH) is wired *and* the bench weight is
  positive.
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
| `NEXUS_CLICKHOUSE_URL` | optional; when set **and** Postgres is not, the bench blend reads `benchmark_runs` from CH (migration `007_benchmark_runs.sql`) via `argMax(completed_at)` |
| `NEXUS_PUBLIC_GATEWAY_URL` | enables `via_gateway`; the gateway base without `/v1` |
| `NEXUS_ROUTE_W_BENCH` | benchmark blend weight (`0.0`–`1.0`, default `0.5`). `0` disables the bench layer entirely |
| `NEXUS_ROUTE_BENCH_HALF_LIFE` | decay half-life for benchmark influence (default `168h`); `0` disables decay |

No env var carries the provider token — it is pasted in the console and
stored encrypted.

## Live CI gate

The bench provider has unit-test coverage in
`internal/router/bench_provider_ch_test.go` and
`internal/router/bench_provider_test.go`, plus live contract
tests in `internal/router/bench_e2e_live_test.go` (PG) and
`TestCHBenchProvider_LiveContract` (CH). The live tests are
gated on `NEXUS_POSTGRES_URL` / `NEXUS_CLICKHOUSE_URL` so a
contributor build skips them.

A separate `bench-live` job in `.github/workflows/integration.yml`
launches a Postgres + ClickHouse service pair and exports the
DSNs so the live tests run as a CI gate. Catch regressions
like:

- A future migration dropping `(status, model, completed_at)`
  from CH would break the `argMax` projection.
- A future migration on PG that stops using `DISTINCT ON` and
  reverts to `AVG(avg_score)` (we explicitly pin against this
  case in `bench_e2e_live_test.go`).
- A stale `internal/core/benchmark_runs.go` writer that loses
  `avg_score` on a status-only update path (covered by the
  existing `TestBenchmarkRunProgressPreservesFieldsItIsNotTold`).

The gate runs in parallel with the existing `e2e` gateway
suite, both sharing the same services via Docker `host` DNS —
PG and CH tolerate the additional concurrent SELECTs.

## Pre-flight validate ("Validate environments" button)

Vendors reject environment slugs they cannot see with a 404. The
console offers a pre-flight dry-run so an operator can confirm:

1. The pasted API key is accepted by the vendor (a 401 surfaces the
   vendor's reason verbatim).
2. The slugs in the form are visible to the authenticated account
   (the most common failure mode for first-time operators).
3. The cancel path works end-to-end — a credential that creates but
   cannot cancel is not safe to launch with.

Implementation:

- **`benchmark.Client.DryRun`** in `internal/benchmark/client.go`
  posts a `NumExamples=1, Rollouts=1` evaluation, then immediately
  PATCHes `/cancel`. The cancel returns before the sandbox provisions
  any inference, so the run never bills tokens. No `benchmark_runs`
  row is written.
- **`benchmark.Runner.DryRun`** wraps it with the same credential and
  validation gate `Launch` walks, so the probe can't succeed on
  inputs the regular launch would reject.
- **Admin REST**: `POST /api/eval/benchmarks/validate` returns
  `{"ok": true}` on success or `{"ok": false, "error": "<vendor
  reason>"}` on a vendor-side failure. Status codes: 400 for missing
  credentials / bad input, 500 for vendor 4xx, 200 for success.
- **Frontend**: the launch form gains a "Validate environments"
  button next to "Launch run". It uses the same disabled-on-empty
  logic as Launch, so an attempt without a credential, env, or
  model stays gated.

The probe costs one POST + one PATCH against the vendor. Curlable
example:

```bash
curl -X POST https://nexus.ffx.ai/api/eval/benchmarks/validate \
  -H "Cookie: $COOKIE" \
  -H 'Content-Type: application/json' \
  -d '{"environments":["your-org/gsm8k"],"model":"openai/gpt-4o-mini"}'
```

Success: `{"ok":true}`. A 404 returns `{"ok":false,"error":"…"}`.

## Env-push guidance and reports

A 404 from the pre-flight means the slug is not published to the
account. The fix is a local CLI flow, and the console reacts to that
specific failure by expanding a panel with the seven steps the
operator runs on their own machine.

The shape is folder-based, not config-flag-based. Prime does not
accept `--config ./env.yaml`; the CLI is the only path and the
operator writes:

```bash
mkdir -p prime-envs/your-org/gsm8k
cd prime-envs/your-org/gsm8k
prime env init your-org/gsm8k -p .        # scaffolds pyproject.toml + README.md
# Operator writes:  your-org/gsm8k.py    (load_environment() + grade())
# Operator writes:  your-org/gsm8k.jsonl (one {question, answer} per line)
prime env push your-org/gsm8k -p .        # cwd must stay inside the folder
```

Why the file paths look the way they do: `pyproject.toml` includes
`["<owner>/<name>.py"]` and Prime deduces the slug from the
`[project] name` field, so editing either is likely to break the
push. The console's Copy / Download buttons write the module and
dataset with the names Prime expects, and the operator just has to
drop them in the right place.

The slug's owner (`your-org` above) must be a username or team slug
already attached to the operator's Prime account. With a fresh API
key, both are unset, and the CLI returns `Owner '<x>' not found` at
the push step. The console calls this requirement out before the
operator runs the commands so a 404 from the CLI is not mistaken for
a broken install.

The panel appears only on a 404 from the vendor. A 401 or a balance
error needs a different fix, and offering CLI steps there would send
the operator down the wrong path.

### The report endpoint

Because the push happens on a laptop, Nexus otherwise has no way to
know it ever ran. The last step of the guide is an optional curl:

```bash
curl -fsS -X POST https://nexus.ffx.ai/api/eval/benchmarks/push-report \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"slug":"your-org/gsm8k","ok":true}'
```

- **Advisory, not authoritative.** Anyone with an admin token can post
  `ok: true` for a slug that was never published. Visibility is still
  decided only by the dry-run, which asks the vendor. The console
  labels these "reported", never "verified", and does not let a report
  unlock Launch.
- **No CLI output.** The body carries `slug`, `ok`, and an optional
  `completed_at` — nothing else. `prime` echoes request details on
  failure and the API key can ride along in them, so accepting stdout
  would put a credential in memory and then render it into an admin
  page. The operator reads their own failure text in their own
  terminal.
- **In memory, 24h TTL, capped at 256 entries.** Keyed by slug, so a
  re-push corrects rather than accumulates. No table and no migration:
  the data is a same-day UI convenience, and losing it on restart costs
  one click on Validate. `GET .../push-report` lists live reports,
  newest first.
- Both methods are admin-gated, and both work on a deployment with no
  control-plane database — the operator publishing environments exists
  there too.
