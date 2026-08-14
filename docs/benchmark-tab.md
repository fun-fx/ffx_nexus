<!--
category: operations
title: Benchmark tab in the console
summary: A walkthrough of the Benchmark tab — credential setup, one-off runs, recurring schedules, and the launcher's two routing modes (via_gateway vs direct-to-provider).
order: 35
status: stable
-->
# Benchmark tab in the console

The console's **Benchmarks** tab (the URL is `/eval/benchmarks`) is
the user-facing front end of the Prime Intellect integration in
[`internal/benchmark/`](../web/src/api.ts). It launches a model
against a dataset, polls the provider for the average score, persists
the run to `benchmark_runs`, and (most-recent settled run per model)
blends the result into the quality-aware router's signal.

The deep operator-tier materials — the `PrimeIntellect hosted eval`
contract, the `via_gateway` / direct split, the virtual-key mint-and-
revoke lifecycle, the cost guardrails — live in
[`docs/model-benchmarks.md`](../docs/model-benchmarks.md). This page
is the **UI walkthrough** a signed-in admin needs to drive a launch.

> The Benchmarks tab requires either Postgres *or* ClickHouse backing
> for `benchmark_runs`. Without one of those, the page renders an
> empty runs list with a sticky banner explaining what's missing;
> the launch button is disabled until storage is configured. The
> Helm chart wires Postgres by default; an ops PR switches to
> ClickHouse via `config.benchmark.provider: clickhouse`.

## Layout at a glance

```
┌─────────────────────────────────────────────────────────────┐
│ CredentialPanel — Prime API key + optional team_id          │
├─────────────────────────────────────────────────────────────┤
│ LaunchPanel — New run form                                   │
│   name, environments, model, num_examples, rollouts,         │
│   via_gateway checkbox, Validate / Launch buttons           │
├─────────────────────────────────────────────────────────────┤
│ Runs table — Run / Env / Samples / Status / Avg /           │
│              Measures / Started; per-row actions            │
├─────────────────────────────────────────────────────────────┤
│ SchedulesPanel — recurring fires; arm/pause/overdue chip     │
├─────────────────────────────────────────────────────────────┤
│ EnvPushGuide — when dryRun returns 404; six-step CLI guide   │
└─────────────────────────────────────────────────────────────┘
```

The page mounts from `web/src/pages/Benchmarks.tsx`. The launch-form
fields are mirrored in the Schedule drawer; the push guide is a
verbatim copy of the help block at `Benchmarks.tsx:881–932` for
operators who skip the manual install.

## CredentialPanel — the one-time setup

A Prime API key is the only thing the launcher needs to start a run.
Paste it once, optionally set a `team_id` (only useful if this Prime
account is shared with a teammate), and press **Save**. The page
shows `configured | not set` as a chip so you can spot the boot
profile at a glance.

> Removing the credential (`Remove` button) deletes the row from
> `/api/eval/benchmarks/credential`. Existing runs on disk are not
> touched — they came from this credential but they aren't owned by
> it. Rotating the key on Prime's side and re-saving here is
> equivalent to a configure-once; the launcher will mint fresh
> virtual keys per run regardless.

A configured key unlocks the **model dropdown** in the launch panel
(direct-to-provider model IDs are pulled from Prime's catalogue via
`fetchBenchmarkModels`). Without a key the model field is free
text and the dropdown is hidden.

## LaunchPanel — one-off run

The form mirrors REST `POST /api/eval/benchmarks`. Required fields:

| Field | Notes |
| --- | --- |
| `name` | shown in the runs table; not unique, you can have two `gpt-4o-mini smoke` runs labelled the same. |
| `environments` | multi-select; the built-in preset `select` is recommended. Custom env modules via `add` are for operators who have a private environment published against their Prime account. |
| `model` | free text or catalogue entry once a credential is saved. The Prime catalogue is the source of truth here for direct-to-provider runs. |
| `num_examples` | clamped server-side to `benchmark.MaxTotalSamples`. If your number is above the cap the launcher rounds it to the cap and adds a yellow hint. |
| `rollouts` | repeats the dataset; `1` is recommended for a smoke test, `>1` is for variance estimates. |
| `via_gateway` | the routing decision — see the next section. |

Two buttons at the bottom:

- **Validate environments** runs `POST /api/eval/benchmarks/validate`
  and is *not* a launch. Use it after you add a private env module
  but before you spend wallet balance on a real run.
- **Launch run** calls `launchBenchmark`. The button shows a
  spinner and the row appears in the runs table immediately at
  status `queued`. The CLI snippet **under the form** copies the
  equivalent `curl` so the operator can sanity-check what the
  launcher actually sent.

### The `via_gateway` checkbox — read this before flipping

Two ways to route inference:

- **`via_gateway: false`** — the provider talks to the model
  vendor directly. The score reflects **what the model does on
  Prime's account** with no Nexus routing attached. Use this for
  "what is the model good at, in isolation" measurements.
- **`via_gateway: true`** — the provider is handed your gateway URL
  (`NEXUS_PUBLIC_GATEWAY_URL`) plus a freshly minted Nexus virtual
  key scoped to the one model being tested. The score reflects
  **what Nexus serves** — your routing, your cost settings, your
  organisation. Use this for "is this model safe to add to
  production".

The checkbox is **disabled** with an explanatory hint when
`NEXUS_PUBLIC_GATEWAY_URL` is unset. The hint names the exact env
var; you can fix the underlying gap at the Helm-config level
without leaving the page.

## Runs table

Columns:

- **Run** — the name you typed at launch, plus a `this gateway` /
  `provider` badge for the `via_gateway` choice.
- **Environments** — comma-separated, with a tooltip listing
  custom-pushed env module slugs when present.
- **Samples** — the dataset size that actually fired (server-
  clamped).
- **Status** — `queued | running | settled | failed | cancelled`.
  Polled against a reverse channel every few seconds; a row that
  doesn't update for two minutes is suspect.
- **Avg score** — only populated when status is `settled`. The
  underlying raw score by `env_slug` is in the **Logs** drawer.
- **Measures** — provider-side P50 / TPF / GPU time. Useful for
  the wallet-side cost review, not the quality-review.
- **Started** — wall-clock timestamp with the jiffies-corrected
  to your locale.

### Per-row actions

- **Provider link** — deep-link to Prime's hosted view of the run.
  Available only after `settled`.
- **Logs** — opens `LogsDrawer`. By default the drawer shows the
  Nexus-side stdout/stderr for this run; the top tab in the drawer
  switches to "Provider raw" for Prime's response payload. Failed
  runs get a yellow-highlighted panel for the error field.
- **Cancel** — only when status is `queued | running`. Calls
  `cancelBenchmark`; the row's status flips to `cancelled` and
  the wallet-side charges stop with the next poll.
- **Delete** — first calls cancel if the run is live, then deletes
  locally. Provider-side records are unaffected — this is a Nexus
  console-side delete only.
- **Refresh from provider** (header button) — for stuck queues
  the page lets you manually fan-out a `refresh` against the
  provider. Use sparingly: each refresh costs an HTTP roundtrip.

## SchedulesPanel — recurring fires

The schedule CRUD mirrors the launch form minus the live submit, and
adds a cadence preset list (1h / 6h / 12h / 24h / 3d / 7d) with a live
"next 4 launches" preview. CRUD calls:

- `fetchBenchmarkSchedules` — read.
- `createBenchmarkSchedule` — open the drawer, fill, save.
- `pauseBenchmarkSchedule` / `resumeBenchmarkSchedule` — flip the
  arm toggle on the row.
- `deleteBenchmarkSchedule` — hard-delete after a confirm.

Schedule rows show three stamps:

- **Armed / paused chip**.
- **Next launch** — wall-clock with the cadence delta applied.
  Hovering shows you the cron-like expression and the human elapsed
  delta.
- **Last launch** — populated only after the first fire; clicking
  it deep-links you to the matching run in the table above, so an
  overdue schedule's history is one click away.

The overdue chip is yellow when the wall-clock crossed the next-launch
stamp and the launcher hasn't picked the row up yet. Cluster-side the
runner polls every minute; if your overdue chip persists for longer
than five minutes, check pod logs for the schedule's UUID.

### Bursty schedules

Two schedules back-to-back for the same model + environment get
deduplicated at the launcher: only one fires. The second row shows a
yellow "skipped" tag and the reason in the logs drawer. This is
deliberate — back-to-back fires against the same model+env at the
same day look identical to the provider, so the wallet would
double-charge for one result.

## EnvPushGuide — when dry run returns 404

When `dryRunM` (the gate that runs `prime env init` server-side)
returns a 404 the launcher cannot resolve any environment names, and
the page renders `EnvPushGuide` — a six-step CLI walkthrough for:

1. `prime env init`
2. `prime env push <module.yml>`
3. `prime auth whoami` (helps you find the `team_id`)
4. `prime auth teams list` (find which team owns the wallet you
   want charged)
5. (optional) `POST /api/eval/benchmarks/push-report` with
   `{slug, ok}` so the cluster knows you've shipped.
6. Use the GSM8K starter module shipped with the Console.

The push-report step exists so that Nexus can keep a record of
"this Nexus cluster ran module X from this Prime account" — it's
informational, not blocking. The optional
`<SAMPLE_ENV_MODULE>` file at the bottom of the guide is a fully
runnable GSM8K starter you can `prime env push` without edits to
verify your credential chain end-to-end.

## What the page does **not** expose

A note on what hit's *not* a UI control:

- **`NEXUS_ROUTE_W_BENCH` / `NEXUS_ROUTE_BENCH_HALF_LIFE`** — the
  router blend constants. They are surfaced as a chip on the page
  and on the Eval tab, but the toggle is in Helm and a config
  reload.
- **The provider-side query syntax** — the launcher renders the
  `payload` template server-side; the JSON shape is in
  [`docs/model-benchmarks.md`](../docs/model-benchmarks.md).
- **Cost guardrails** — `benchmark.MaxTotalSamples` is a server-
  side cap; the page shows a hint when your number hits it but does
  not let you raise it from the UI.

## Verifying what the cluster is actually doing

Three signals worth keeping open while a run is live:

- `kubectl logs deploy/nexus -f | grep -i 'bench\|prime'`. The
  launcher logs each poll against the provider; a stalled run shows
  a long gap between status updates.
- `/api/eval/benchmarks` and `/api/eval/benchmarks/schedules` are
  JSON. Quick reconsile:
  `curl -sS https://nexus.ffx.ai/api/eval/benchmarks | jq '.[] | {name, status, via_gateway}'`
- `/api/eval/benchmarks/credential` shows whether the credential is
  configured without exposing the key itself.

## See also

- [`docs/model-benchmarks.md`](../docs/model-benchmarks.md) — full
  REST surface, `via_gateway` semantics, virtual-key lifecycle,
  prerequisites (env module publish, wallet balance), cost guardrails.
- [`docs/onboarding.md`](../docs/onboarding.md) — the RBAC gate; the
  Benchmarks tab is admin-only.
- [`web/src/pages/Benchmarks.tsx`](../web/src/pages/Benchmarks.tsx)
  — every panel here maps 1:1 to a component on this page.
