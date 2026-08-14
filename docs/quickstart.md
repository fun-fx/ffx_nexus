# Quickstart

Nexus is a single Go binary that fronts every LLM API call behind a
gateway, a router, an eval layer, and a benchmark integration — and
ships with a console that drives all four from one screen.

The four parts each answer a separate question:

| Layer | Question it answers |
| --- | --- |
| Gateway | Will my call reach the model, and what did it cost? |
| Router | Which model should this particular call go to? |
| Evaluator | Was the answer worth the spend? |
| Benchmark | Is the model I've picked still the model I want? |

Each layer is configurable independently, but they compose — the
router's quality-aware blend reads from the evaluator's `eval_scores`
table and the benchmark's `benchmark_runs` table, so a model that
drifts downward over a week slips out of the preferred list without
a tactical decision.

Going further, the docs are organised around the console tabs that
drive each piece:

- **[Eval tab in the console](eval-tab.md)** — how the eval layer
  and the quality-aware router are configured at runtime.
- **[Benchmark tab in the console](benchmark-tab.md)** — how model
  benchmarks are launched on the hosted-verifier provider.
- **[Team onboarding](onboarding.md)** — bringing a new team onto a
  running Nexus instance.

## Why it's different

A traditional LLM gateway routes the cheapest available model;
Nexus routes the model whose recent **scored** quality justifies the
cost. A traditional LLM eval adds an async LLM-as-judge on the side;
Nexus's eval layer keeps a small set of heuristic rules synchronous
in the gateway worker, and the rest stays on the operator's choice
of evaluator (Langfuse, LangSmith, Datadog, Braintrust, Arize,
Confident AI, Arize Phoenix, OTLP collector, or a webhook into your
own scorer).

The console is one screen for every layer — there is no separate
ops console and no separate benchmark console. A signed-in
operator sees quality, cost, and latency at the top of the page
and the four layers stack below.
