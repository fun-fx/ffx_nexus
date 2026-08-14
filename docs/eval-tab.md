<!--
category: operations
title: Eval tab in the console
summary: A walkthrough of what the Eval tab does, which controls are exposed, and which env vars / cheat-codes shape the UI.
order: 30
status: stable
-->
# Eval tab in the console

The console's **Eval** tab is where the cluster's quality-control
layer is operated at runtime. It is the user-facing front end for
[`internal/evalplugin/`](../web/src/api.ts) (heuristic evaluators and
plugin manifests) and the quality-aware router's weight knobs. The
deeper operator-tier reference — the YAML `v1alpha1` schema, vendor
adapter quirks, the closed enum of `metric.name`, and Helm rendering
of `configMaps` — lives in
[`docs/eval-plugins.md`](../docs/eval-plugins.md). This page is the
**UI walkthrough** a signed-in admin needs to drive the page.

> The Eval tab is admin-only. A non-admin signed in to the console
> sees a `Forbidden` placeholder instead. That's the RBAC gate, not a
> bug — adjust the user's role via the **Members** page if you expect
> to see it.

## Layout at a glance

The page is one scrollable column with five stacked panels
(top-to-bottom) and a stats bar pinned at the top:

```
┌─────────────────────────────────────────────────────────────┐
│ Page-head stats bar — quality / cost / latency %, sample     │
│ rate, worker count                                          │
├─────────────────────────────────────────────────────────────┤
│ Banners (only when triggered)                               │
│   • Plugin-only mode banner (NEXUS_EVAL_PLUGIN_ONLY=true)   │
│   • Legacy-deprecation banner                                │
│   • Bench-blend chip when w_bench > 0                        │
├─────────────────────────────────────────────────────────────┤
│ EvaluatorsCard — one merged table, three row kinds           │
├─────────────────────────────────────────────────────────────┤
│ EvalProfilesCard — per-profile cards, deep-linkable          │
├─────────────────────────────────────────────────────────────┤
│ WeightsCard — three sliders, simplex-normalised on save     │
├─────────────────────────────────────────────────────────────┤
│ GroupsCard — read-only summary of NEXUS_ROUTE_GROUPS         │
│ (only renders when non-empty)                                │
└─────────────────────────────────────────────────────────────┘
```

Above the table sits a **+ Install plugin** button (or **Install your
first plugin** when the cluster has none and plugin-only mode is on).
Below the table sits the **`PluginQuickStart`** tile gallery — vendor
presets you can fill in YAML-style or via the form. The page mounts
from `web/src/pages/Eval.tsx`; profile CRUD lives in
`web/src/pages/EvalProfiles.tsx` and the install-edit form in
`web/src/pages/EvalPlugins.tsx`.

## EvaluatorsCard — the merged table

There is one table, not three. Each row's colour/foreground kind
tells you how the row is sourced:

| Row kind | Examples | Where the row comes from |
| --- | --- | --- |
| `heuristic` | PII, Completeness | in-process Go regex in the gateway worker — toggle the row to enable / disable |
| `legacy` | SLM judge, Remote eval | back-compat with the pre-plugin contract, kept but no longer self-installed |
| `plugin` | Langfuse, LangSmith, Datadog, Braintrust, Arize, Confident AI, Arize Phoenix, OTEL Collector, Webhook | YAML manifests retrieved from `/api/eval/plugins` |

Per-row controls:

- **Toggle** on the left flips the row. Each toggle is a `PATCH` to
  the matching profile id. The page resolves PII / Completeness to
  `default-pii` / `default-completeness`; SLM judge to `default-judge`;
  remote eval to `default-remote`. If those rows have no id (operator
  purged them via plugin-only boot) the toggle falls back to the first
  profile of the same kind in your org.
- **Test** runs `POST /api/eval/plugins/{name}/test` against the
  vendor. It sends a single synthetic trace with the manifest's
  payload template rendered; a `Pass` means the endpoint *and* the
  credentials are good. Anything else prints the vendor's own
  message — there is no Nexus-side normalisation, since a rejected
  send is otherwise indistinguishable from a vendor with nothing to
  report.
- **Edit** opens `PluginEditorDrawer` from `EvalPlugins.tsx`. It
  shows the manifest as YAML by default but toggles to a form view;
  both views round-trip via `parseYamlToForm` /
  `serializeFormToYaml` in `web/src/lib/pluginManifest.ts`.
- **Delete** removes the row after a confirm. Deletion is soft-
  disabled if the row is referenced by a profile — disable that
  profile first.
- **Keys** opens `PluginKeysModal` (`web/src/components/PluginKeysModal.tsx`)
  for rotating the `secretRef` without re-saving the whole manifest.

## PluginQuickStart — the vendor tile gallery

The tile gallery only renders when both:

- `NEXUS_EVAL_PLUGIN_ONLY=true` (or `config.evalPluginOnly: true`
  in Helm), and
- there are no installed plugins yet.

The tile order is fixed: **Langfuse → Webhook → LangSmith → Confident
AI → Datadog → Braintrust → Arize → Arize Phoenix → OTEL Collector**.
Langfuse goes first because it is the only adapter that's wired end
to end and self-hosts; everyone else either needs a SaaS key or your
own collector. Click a tile to fill the editor drawer with that
vendor's preset (`endpoint`, default sampling, default scope,
default send.trigger), then store credentials in your secret manager
and **Test** the row before flipping the toggle.

The first-pass cost of going Plugin-only is this: every trace the
operator wants scored has to leave the cluster. The console's banner
warns about that the moment the cluster boots with plugin-only on,
and the destruction-twin flag
`NEXUS_EVAL_PURGE_LEGACY_PROFILES_ON_BOOT` (Helm
`config.purgeLegacyProfilesOnBoot`) hard-deletes the four seed rows on
boot. That second flag is **load-bearing**: flipping only one of the
two leaves historical rows intact but no longer seeded, which is the
deliberate "I haven't decided yet" state.

## EvalProfilesCard — your own profiles

Profiles are how an evaluator set gets bound to live traffic. The
table from the previous section shows the **installed** evaluators;
profiles show **which ones fire on which routes**, and what their
weighting is when more than one fires on the same trace.

This page links back to the `EvalProfilesCard` automatically when you
deep-link with `?focus=…` from a profile's row in the operator-tier
docs, or when you create a profile from scratch via the **+ Add
profile** button.

Inside the card:

- **Create profile** opens `Drawer` with a name, an evaluator picker,
  and per-evaluator weight. Weights are summed to 1.0 on save; the
  backend enforces that with a normalise-on-write contract, so a UI
  that doesn't normalise will produce a profile that scores traces
  with whatever weighting the backend decides.
- **Edit profile** lets you re-weight or remove evaluators without
  deleting the profile itself.
- **Delete profile** is hard-delete; it does not disable the
  underlying evaluators at the table above — that's a different
  toggle.

## WeightsCard — quality / cost / latency

Three sliders, one per axis the quality-aware router blends:

| Slider | Drives |
| --- | --- |
| Quality (high) | latest benchmark avg_score + recent eval plugin PII/Cmpl scores |
| Cost (low) | the upstream USD/Gb-token figure the gateway recorded |
| Latency (low) | P50 from the gateway's per-model timing deck |

`finalizeForSimplex` normalises the three values to 100% on save, so
UI fiddling with two sliders at once is safe. Unchanged values are
preserved across saves; the backend stores the normalised triplet so
two operators who set "100 quality / 0 cost / 0 latency" produce the
**same** routing intent.

The values above the sliders are live counts (e.g. "Quality sampled:
482 traces this hour"). Below 50 sampled traces the slider is
advisory, not authoritative — the operator banner flips to yellow.

## GroupsCard and the route-group toggle

This panel only renders when `NEXUS_ROUTE_GROUPS` is non-empty. It
is purely informational: the route groups are configured elsewhere
(usually in Helm `configMap.yaml`), and the table is a read-back for
verifying that what got deployed is what you meant. There is no
create / edit / delete here; that's a Helm PR, not a console action.

## What the page does **not** expose

A note on what hit's *not* a UI control:

- **`spec.flags: [strict]`** — strict-mode is read at boot from the
  manifest. The console surfaces strict-field warnings via slog at
  warn level but has no toggle; the operator changes it in YAML and
  redeploys.
- **`metric.name`** — the closed enum (`contains`, `pii`,
  `exact_match`, `rouge_l`, `hf_evaluate`, `lighteval`, `ragas`)
  lives in `internal/evalplugin/types.go`'s
  `ValidMetricNames`. A typo is rejected at decode; the console
  highlights the offending field, not the typo'd name.
- **Plugin creation flow control** — sampling rate, send.trigger
  kind, dispatch scope, payload template: all of these are
  manifest-level decisions and the editor drawer simply reflects
  the manifest's view of them. There is no UI affordance for "I
  want every-third-trace-and-only-on-errors" because that needs
  the `sampling` field, not a button.

## Verifying what the cluster is actually doing

Three signals to look at when the page itself is suspicious:

- `kubectl logs deploy/nexus | grep -i 'eval\|plugin'`. Dispatch
  failures and decoding errors are loud at warn level by design
  (they go through `evalplugin.Decode` -> strict-mode
  `StrictFieldSink` -> slog).
- `/api/eval/profiles` and `/api/eval/plugins` are JSON. Quick
  command-line reconcile:
  `curl -sS https://nexus.ffx.ai/api/eval/profiles | jq '.[] | {name, kind, active}'`
- The helm chart's `eval.plugins.configMaps` rendering (see
  [`docs/eval-plugins.md`](../docs/eval-plugins.md)) is the
  single-source-of-truth for "what will exist at boot" once the
  console's table goes stale.

## See also

- [`docs/eval-plugins.md`](../docs/eval-plugins.md) — full YAML
  `v1alpha1` schema, vendor adapter quirks, Helm rendering.
- [`docs/onboarding.md`](../docs/onboarding.md) — the RBAC gate and
  how to grant the Eval tab to a role.
- [`web/src/pages/Eval.tsx`](../web/src/pages/Eval.tsx) — every
  component on this page maps 1:1 to a section here.
