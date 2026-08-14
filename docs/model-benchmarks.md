# Model benchmarks

Run any model through any hosted evaluation suite on a hosted-verifier
platform — without standing up eval compute inside the cluster.

A benchmark asks a different question than an eval plugin: **how
good is this model at this task**, judged against a fixed dataset
and a verifier, rather than "how good was this particular trace?". A
plugin listens to live traffic; a benchmark listens to an operator
pressing **Run** once.

## Why we run benchmarks off-cluster

The dataset, the verifier, and the GPU driver all stay on the
provider's side. Nexus ships a single Go binary plus a console, so a
benchmark launch is one POST to the provider, one poller, one
`benchmark_runs` row. The most-recent settled run per model blends
into the quality-aware router — a benchmark is the source of the
"this model scores well for this work" line on the routing policy.

## Two ways to route a benchmark's traffic

A single toggle decides what the score actually means.

- **Direct** — the provider drives the dataset through the model on
  its own. The result answers *"is this model, in isolation, good
  at this task?"*
- **Routed via Nexus** — the provider calls the model through a
  freshly minted virtual key scoped to your gateway. The result
  answers *"is this model good at this task, **as Nexus serves
  it**?"*, with your routing, your cost settings, and your
  organisation's policies in the loop.

Operators usually run both shapes back-to-back: the direct shape
ranks models, the routed shape ranks production candidates.

## Recurring runs

A benchmark does not have to be a one-shot. The same launcher
schedules recurring runs on a 1h / 6h / 12h / 24h / 3d / 7d cadence,
and the resulting averages drill into the routing signal so a model
that drifts downward over a week is no longer preferred even though
its last run was strong.

## What this enables

- A model promotion path that is measurable, not opinion-based.
- A quality-aware router that picks the right model for a request
  without spending the cluster's eval compute.
- A recurring regression detection — the daily score for model X
  falling by 5 % is visible on the routing chip in the console, not
  a tactical decision someone has to make.
