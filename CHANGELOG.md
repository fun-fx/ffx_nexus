# Changelog

All notable changes to Nexus are documented in this file. The format is
loosely based on [Keep a Changelog](https://keepachangelog.com), and the
project adheres to [Semantic Versioning](https://semver.org/) for the
Go gateway binary.

## [Unreleased] — Trace content is no longer persisted by default

### Breaking for anyone relying on the Traces inspector

Prompt and completion bodies are **no longer written to ClickHouse** unless
you opt in. A new `CaptureGate` strips `InputMessages`, `OutputMessages` and
`RetrievalContexts` from the trace on its way to durable storage, so after
upgrading, the console's trace inspector shows metadata (model, tokens,
latency, cost, verdicts) with empty message bodies.

Turn it back on with either:

- `NEXUS_CAPTURE_TRACE_CONTENT=true` on the pod, or
- `config.captureTraceContent: true` in the Helm chart.

**Evaluators are unaffected.** The gate wraps only the ClickHouse recorder,
and `MultiRecorder` passes the trace by value, so in-process evaluators
(PII, completeness, heuristic metrics) still receive full bodies. Scores do
not change; only what is written to disk does. If your eval scores move
after this upgrade, that is a bug, not this change.

Default-off was chosen because the alternative default is one that stores
customer prompts indefinitely without the operator having asked for it.
Operators who need body retention for debugging now make that a recorded
decision in their values file.

- `internal/observability/capture.go` — the gate, with a
  `contentFieldCount` guard that fails the build if a content field is added
  to `Trace` without being considered here.
- `cmd/nexus/compose.go` — recorder assembly extracted into `traceFanout`
  so the wiring is testable without a live ClickHouse connection. Tests
  assert the ClickHouse branch is never ungated and the eval worker is never
  gated.
- `deploy/helm/nexus/values.yaml`, `values.schema.json`,
  `templates/configmap.yaml` — the knob, typed as a boolean.

### Security

Six open Dependabot alerts closed; `npm audit` now reports zero at every
severity including dev dependencies.

- `aquasecurity/trivy-action` → `0.35.0`, pinned to a **commit** rather than
  a tag. The Trivy release pipeline was briefly compromised
  (GHSA-69fq-xp46-6x23), which made a mutable third-party tag a
  supply-chain hole in the job whose purpose is finding supply-chain holes.
- `react-router-dom` → `^7.18.3`. The 6.30.x line has no patched release, so
  the major bump was the only available fix for three router advisories. The
  console uses declarative APIs only and no data router, so
  GHSA-337j-9hxr-rhxg (`deserializeErrors` constructor injection) was never
  reachable here.
- `browserslist` pinned to `^4.28.7` via `overrides` (GHSA-73wf-gq98-2v4g).
- **Open redirect actually fixed at the sink.** The post-login `?next=`
  guard existed in two copies, both testing `startsWith("//")` and neither
  testing for a backslash — which is the bypass the advisories use.
  `/\evil.com` starts with one slash, is not `//`, and browsers normalise it
  to `//evil.com`. Both guards read like protection and neither was. They
  are now one implementation in `web/src/lib/safeNext.ts`, which holds
  regardless of the router version underneath.

### Installation

- `values-staging.example.yaml` and `values-production.example.yaml` were
  **not installable**. Both inherit `profile=enterprise` and neither set
  `networkPolicy.enforcementAcknowledged`, so `helm install` failed outright
  for anyone who copied them. Both now carry a `networkPolicy` block that
  explains what the acknowledgement means and that the Postgres namespace is
  a chart default rather than a detected value.
- **The chart refuses `profile=enterprise` with `mode=disabled`.** Every
  validation sat inside `if eq .mode "enforce"`, making `mode=disabled` a
  single switch that skipped all of them including the acknowledgement
  requirement — the strictest profile silently produced the weakest result.
  `values.yaml` and both upgrade-rehearsal scripts already documented this
  refusal; the template never performed it.
- **The chart refuses external features with no egress path.** Under
  `profile=enterprise`, `features.sso` and `features.emailResend` need
  either the egress proxy or a declared `serviceTargets.<feature>.namespace`.
  With neither, the install succeeded and the feature broke at runtime — SSO
  redirecting to an issuer the pod cannot reach looks like an outage rather
  than a misconfiguration. Declaring a namespace is a complete answer, so
  operators routing egress themselves are unaffected.

### CI

Three gates that looked green were not running:

- The `kubeconform` step piped `helm template` into the validator without
  `pipefail`. Since neither example values file rendered, it validated empty
  input, reported "0 resource found parsing stdin", and passed. It now
  validates 13 and 14 resources.
- The `web` job ran `npm build` but never `npm test`, so 183 console tests
  went unrun and rotted; a missing Web Storage shim in `vitest.setup.ts`
  failed 125 of them. Fixed and wired in.
- Eight offline policy harnesses were committed and executed by nothing,
  appearing only in `cni-nightly.yml` path filters. Eleven now run as
  `Policy contracts (offline)` on every pull request in about three minutes.

Also: `main` now has branch protection with nine required checks (see
`.github/branch-protection.md`), twelve path-filter entries naming
nonexistent files were corrected — including `internal/urlpolicy/**`, where
the real packages are `internal/ippolicy` and `internal/netpolicy`, so the
gate was silent on the code it exists to guard — and the CNI enforcement
gate now runs nightly in addition to manual dispatch, closing D-2b.

## [v0.6.12] — Resizable columns on the Traces page

Operators reported that the `Time` column on the Traces page was being
clipped at narrow viewport widths because every `DataTable` column used
a fixed pixel width. This change gives operators first-class control
over column widths via a draggable resize handle on each header cell.

### Frontend

- `web/src/components/DataTable.tsx`
  - New resize handle rendered at the right edge of each column header
    that has a numeric width (or a stored user override).
  - Pointer-driven drag commits the chosen px width to React state on
    every move; the next pointerup releases the session.
  - Keyboard accessibility: focus the handle and use ← / → to nudge by
    8 px, Home / Escape to reset to the declared width.
  - `storageKey` prop namespaces the persisted widths in
    `localStorage`; supplied as `"nexus:dt:traces"` for the Traces
    page so the operator's drag survives reloads.
  - Per-instance `useRef` for the drag session so two `DataTable`s on
    the same page do not share a session.
  - `aria-valuenow/min/max` on the handle reflect the live width for
    screen-reader users.
- `web/src/components/DataTable.test.tsx`
  - Five new tests covering the handle being rendered, keyboard
    nudge, MIN_PX clamp at 40 px, restored-width on mount, and the
    per-key storage write contract.
- `web/src/styles.css` + `web/src/pages/Traces.tsx`
  - `.dt-row` promoted from `display: contents` to a real grid so the
    resize handle can live next to its header cell; column-header
    button is now an `inline-flex` row inside a flex `<th>`.
  - `.dt-resize-handle` is a narrow column cursor with a 1 px reveal
    bar on hover / focus / drag.
  - `body.dt-resizing` locks selection globally during a drag.



## [v0.6.11] — Server-side filter + time-window pagination for Traces (PR #157)

The Traces page previously hard-coded a fetch of the most-recent 500
events with all filters applied client-side. With gateway traffic
growing past 500 events/minute, operators searching "last week's 4xx
calls for claude" had no way to reach older rows. The page showed
exactly 500/25 = 20 pages and that was the whole story — the 90-day
TTL on `gateway_traces` had nothing to do with it (it never even
asked for the older rows).

This change moves every existing filter into the URL and adds a
cursor-paged `Load older` control so operators can walk back through
time within the active filter set.

### Backend

- `internal/observability/reader.go`
  - New `TracePage` envelope: `{items, next_cursor: {before, since}}`.
  - `Reader.TracePage(ctx, before, since, limit, userID, TraceFilter)`
    runs the windowed + filter-narrowed SELECT and uses a `LIMIT + 1`
    probe to detect whether a next page exists (no separate `count()`).
  - Pure-function SQL builder (`buildTracePageQuery`,
    `buildTracePageArgs`) so the SQL shape is unit-testable.
  - `TraceFilter.Status` ("ok"/"err"), `.Provider` (exact match),
    `.Q` (case-insensitive LIKE on `request_model | provider_name |
    user_email | guardrail_action`). `%` and `_` in user input are
    escaped to literals; pre-fix `q = "5%"` would have matched every
    row.
  - `(before, since)` describes a half-open window so adjacent pages
    never double-count at the boundary.
- `internal/console/server.go` — `parseTraceQuery` accepts
  `before`/`since`/`status`/`provider`/`q`. RFC3339 and RFC3339Nano
  both accepted.
- `internal/console/auth.go` — `myTraces` mirror.

### Frontend

- `web/src/api.ts` — `fetchTraces(TraceQuery)` returns `TracePage`;
  defensive decoding of the legacy bare-array shape so a rolling
  restart between v0.6.10 and v0.6.11 doesn't black out the page.
- `web/src/pages/Traces.tsx` — fully rewritten:
  - Date pickers for `since` / `before` (browser-local <input
    type="datetime-local">, sent to the server as RFC3339 UTC).
  - Status / provider chips + free-text search now flow through to
    the server. Client-side filtering is gone — single source of
    truth = server response.
  - `Load older` button under the table re-applies the current filter
    set with the server's `next_cursor` and merges new items into
    the in-memory list (de-duplicated by `trace_id`).
  - Button label flips to `No more pages` once the cursor is empty.

### Tests added

- `internal/observability/reader_test.go` (new): 22 cases pin every
  filter combination, the `q` LIKE escape shape, the cursor timezone
  round-trip, and the slice/empty-cursor contract.
- `internal/console/trace_query_test.go` (new): 11 cases pin
  `parseTraceQuery` — invalid `before`/`since`, inverted window,
  unknown status enum, etc.
- `web/src/pages/Traces.test.tsx` (new): 6 cases pin the wire
  contract — initial URL params, date input → ISO conversion, status
  chip round-trip, search round-trip, `Load older` follow-up
  (including cursor fields), and a negative-status-value regression
  check.

### Operator-visible effect

- "20 pages only" goes away. After `Load older` reaches the bottom
  of the result set, the button disables itself — no spurious pages
  past the time window.
- `q = "gpt-4o"` actually runs on the indexed column now. With the
  legacy client filter the operator could only search the in-memory
  500-row slice, so anything not in the first page was effectively
  invisible.
- Date range + status + provider + free-text can be combined. A
  query like `?status=err&provider=openai&q=gpt-4o&since=2026-07-20`
  walks only `gpt-4o` calls on `openai` that came back `>= 400`
  since 7/20.

### Risk

Low. Pure additive: filters AND into the existing WHERE clause,
cursor is opaque to the client, and the legacy bare-array shape is
still decoded so a rolling restart is non-fatal.

## [v0.6.10] — Cost regression fix for versioned + prefixed model ids (PR #155)

After deploying v0.6.9 the operator reported that **every trace's `CostUSD`
showed $0** in the Traces page. Two coupled root causes:

1. **Stale `pricingTable` vs. dynamic model catalog.** PR #111 added a
   dynamic `/v1/models` sync that pushed upstream-returned versioned ids
   (`gpt-4o-2024-08-06`, `o3-2025-01-31`, ...) and bare family ids into
   the registry. The pricing table only held family roots; the lookup
   had a single `<provider>/` strip fallback but no family-prefix
   fallback. The silent `return 0` in `CostUSD` then charged zero for
   every versioned id.
2. **Three providers missing entirely.** `groq`, `mistral`, and `grid`
   were absent from `pricingTable` even though the gateway registered
   them via `providers.NewGroq` / `NewMistral` / `NewGrid`. Every
   request that landed on those providers produced `trace.CostUSD = 0`,
   which polluted cost-weighted routing weights and per-member spend
   dashboards.

### Changes

- `internal/gateway/pricing.go`
  - Adds a `familyAliases` longest-prefix mapper so versioned ids
    resolve to their family root: `gpt-4o-2024-08-06` -> `gpt-4o`,
    `o3-2025-01-31` -> `o3`, `claude-3-7-sonnet-20250219` ->
    `claude-3-7-sonnet-latest`, etc. The alias list is conservative; a
    future model like `gpt-5` fails closed (cost = 0) instead of
    inheriting a sibling's price.
  - Populates `pricingTable` with the chat/embed catalog shipped in
    `providers/openai_compat.go` for **groq** (Llama 3.3/3.1, Mixtral,
    Gemma2, Llama-Guard), **mistral** (Large/Medium/Small, Codestral,
    Ministral, Pixtral), and **grid** (text/code/agent x
    standard/prime/max instruments).
  - Extends `CostUSD(requestModel, responseModel, in, out)` to accept
    the upstream `ResponseModel` alongside the `RequestModel`; some
    providers rewrite the model field on the response even though they
    price at the family level.
- `internal/gateway/handler.go`: three call sites updated to pass
  `trace.ResponseModel`.
- `internal/gateway/pricing_test.go` (new): 25-case table-driven test
  that pins every newly-priced key, both negative paths, the
  future-model fail-closed invariant, and token-scaling math. There was
  no dedicated pricing test before this change.

### Test plan
- `go test ./internal/gateway/ -run 'TestCostUSD|TestPricingTable'`: 25
  subcases pass.
- `go test ./...`: full module regression green (console, evals,
  eval-batch, observability, gateway, router, limiter, balancer,
  semcache, guardrails).
- `go vet ./...` clear.

## [v0.6.9] — Unified Eval toggles + per-user scope override (PR #153)

Round-up of the v0.6.8 release with the operator feedback that landed in the
week after. Three problems were identified by a live admin test in the
production console:

1. `Disable evaluation` was a one-way trip — the button dropped to a disabled
   state once the row flagged `enabled: false`, leaving no obvious way to flip
   SLM judge or Remote eval back on without a manual profile edit.
2. The Heuristics panel and the Eval Profiles panel underneath felt like two
   separate surfaces; admins weren't sure why seeded `default-*` profiles
   appeared after touching the top table's toggles.
3. PII and Completeness could be **double-scored** — once from the legacy
   `Worker.piiEnabled` / `Worker.completenessEnabled` fields (env-driven),
   once from the seeded `default-pii` / `default-completeness` profiles
   (`scope:org`). It wasn't visible in the UI but it was visible in
   `eval_scores`, doubling the heuristic storage.

This PR lands **"Policy A"** for the per-user override question, with all four
metrics now using the same control surface:

### Unified toggle

The Heuristics panel now exposes **one switch per metric** with a uniform UI:

- **PII** and **Completeness** remain on the same direct toggle they had; the
  only behavioural change is that they now drive a single seeded default
  profile (`default-pii` / `default-completeness`) instead of the legacy
  `Worker.piiEnabled` flag.
- **SLM judge** and **Remote eval** lose the prior "Disable evaluation" /
  read-only pill split. The row is now a real `LabelToggle` whose `aria-label`
  flips between `disable X` and `enable X`. The "Change configuration" shortcut
  is preserved for admins so they can edit the underlying default profile
  without scrolling.

A short hint under the table spells out the override rule below, so an admin
reading the page cannot miss it.

### Per-user scope override ("Policy A")

`scope: user` profiles with `enabled: false` for a given `Kind` *suppress* the
matching `scope: org` profile **for that user's traffic only**. Other members
in the same org see no change. `scope: user` profiles with `enabled: true`
remain *additive* — both the org and user profiles score that user's traces.
This is the additive-when-enabled, suppressive-when-disabled rule so a member
can opt out *or* layer their own judge model without rewriting the org
default.

### Removed legacy paths

- `Worker.piiEnabled` / `Worker.completenessEnabled` fields and their `Set*`
  methods are gone.
- `cmd/nexus/eval_runtime.go::Apply` no longer consumes `{pii_enabled}` /
  `{completeness_enabled}` from `EvalConfigPatch`. The console router already
  rejects those fields with a 400; this is defense-in-depth.
- `evalRuntime.buildSnapshot` populates `snap.Eval.PIIEnabled` /
  `snap.Eval.CompletenessEnabled` from `Worker.ProfileStatus()` now (which
  also gained those two flags).

### Surface-level touches

- A small `env-seed` chip now appears next to profiles whose id starts with
  `default-` (matches the env-seeded rows) so the operator can tell at a
  glance which entries the gateway authored vs hand-created.

### Operator notes

- Existing tenants boot with `default-pii` and `default-completeness` already
  `enabled:true`, so disabling PII/Completeness from the Heuristics panel
  produces the same wire effect as before — but now goes through a
  profile-driven path that's discoverable in the Eval Profiles list.
- The eval pod is **not** restarted. Patches to `default-*.enabled` are
  picked up by the worker on the next evaluate() loop, exactly like
  v0.6.8 for SLM judge / Remote eval.
- Personal overrides (`scope: user`) require an admin or the member
  themselves to create the user-scope row via the existing "New profile"
  drawer. The console exposes `My profiles` vs `Org profiles` groups
  under the Eval Profiles card.

### Tests

- Backend: `TestScopeOverride_UserOffSuppressesOrgForUser`,
  `TestScopeOverride_DoesNotAffectOtherUsers`,
  `TestScopeOverride_UserON_AdditiveWithOrg`,
  `TestScopeOverride_CompletenessAndPII` (covers Policy A scope semantics
  for both PII and Completeness kinds).
- Frontend: heuristic table renders 4 interactive toggles (no
  `aria-disabled` on any), admin sees `Change configuration` per row, and a
  PII / SLM judge click PATCHes the matching default profile with
  `{enabled:false}`.

## [v0.6.8] — Console-level disable + reconfigure for SLM judge / Remote eval (PR #151)

Console + gateway follow-up to [v0.6.7](#v067--toggle-round-trip--single-heuristic-panel-pr-149).

Until this release, the **SLM judge** and **Remote eval** rows in the Eval page rendered an `on/off` pill but could not be flipped from the console — their `BaseURL` / `Model` / `Metrics` were seeded by environment variables on the eval pod and the toggle was effectively cosmetic. Admins who wanted to disable either metric (or to swap a judge model) had no console-level escape hatch.

This PR introduces one via the `EvalProfile` store, while keeping the operator off any direct cluster touchpath:

- **Worker `ProfileStatus()`** derives `{SLMJudgeEnabled, RemoteEvalEnabled}` from the `configuredProfiles` slice (enabled per `Kind | ProfileSLMJudge | ProfileRemoteEval`). `cmd/nexus/eval_runtime.go::buildSnapshot` now uses this for `snap.Eval.Judge.Enabled` / `snap.Eval.Remote.Enabled`, while keeping `BaseURL` / `Model` / `URL` / `Metrics` sourced from env. The console sees whether the metric is *running*, not just whether it is wired.
- **Two business buttons on the heuristic row**, both admin-only:
  - **Change configuration** — opens the seeded `default-<kind>` profile in the existing Eval Profiles drawer. Admins can swap `BaseURL` / `Model` / `Metrics` directly, without editing the helm chart.
  - **Disable evaluation** — flips `default-<kind>.enabled` to `false`. The eval pod keeps running but the disabled evaluator is skipped at runtime; no pod restart, no scaledown.
- The previous dimmed `LabelToggle` is preserved as a *read-only* status indicator (`aria-disabled`) so non-admin operators still see the current running state and a hint that the row is env-driven.
- **Two independent kinds** — disabling `default-judge` does not touch `default-remote`. Covered by `TestProfileStatus_TwoKindsIndependent`.

### Operator notes

- The eval pod is **not** restarted or scaled. The metric is simply skipped at score time. Existing traces keep flowing for the metrics you did not disable.
- Reconfiguring a model / endpoint updates the profile store only. The next eval batch picks up the new configuration; no Helm / `kubectl` interaction is required.
- For non-admin members, the buttons are not rendered, and the page itself remains admin-gated.

### Tests

- Backend: `TestProfileStatus_RespectsProfileEnabled`,
  `TestProfileStatus_TwoKindsIndependent`,
  `TestProfileStatus_IgnoresLegacyJudgesField`.
- Frontend: heuristic table renders 4 metric toggles (2 clickable, 2 `aria-disabled`), admin sees both action buttons per row, and the disable button PATCHes `default-<kind>` with `{ enabled: false }`.

## [v0.6.7] — Toggle round-trip + single heuristic panel (PR #149)

Console + gateway follow-up to [v0.6.6](#v066--heuristic--env-driven-split--stable-profile-order-pr-147).

Real-world test surfaced two issues with the PR #147 wiring:

1. **PII / Completeness toggles were a silent no-op.** The console sent
   `{pii:true|false}` short keys, but the backend `EvalConfigPatch` only
   knew the flat `pii_enabled` form. The worker kept the previous state,
   the GET snapshot didn't change, and the click looked dead.

   `EvalConfigPatch` now also accepts the nested `{eval:{pii_enabled:..}}`
   shape (`EvalConfigPatchEval`) and merges it into the flat fields before
   delegating to the worker. Old admin scripts continue to work. The web
   console sends the canonical `{pii_enabled:..}` flat form.
2. **The heuristics card had two visually-unrelated panels** after
   #147 — one for heuristic rows, one for env-driven evaluators. This
   PR merges them into a single `DataTable` so the operator sees one
   panel with 4 metric rows. Env-driven rows render with a dimmed
   `LabelToggle` and a tooltip pointing at the env vars.

## [v0.6.6] — Heuristic / env-driven split + stable profile order (PR #147)

Console-only follow-up to [v0.6.5](#v065--label-toggle-everywhere-pr-145).
Re-shapes the Eval heuristics card so PII / Completeness keep the toggle
(they're PATCH-able from the console) and SLM judge / Remote eval no longer
look like toggles — they sit in a separate table with `env-driven` tags.
Also pins `GET /api/eval/profiles` to creation order so the operator's
mental model of "who came first" survives a refetch.

## [Unreleased]

### Fixed

- **Heuristic / env-driven split on Eval page (PR)**: PII / Completeness now render with an `Enabled` column so the toggle is actionable from the console. SLM judge and Remote eval are no longer drawn under the same row type — they live in a separate, narrower table without an `Enabled` column, with an `env-driven` hint tag so operators no longer see a non-clickable `on/off` pill. The previous UX left two rows looking like toggles but doing nothing on click which was a confusing affordance.
- **Eval profile list is now in creation order**: `coreProfileStore.List()` now sorts by `created_at` ascending (tie-break by ID) instead of leaking Go's map-iteration order. Console pages, refetches, and any CLI navigation now produce the same row order — operators keep their mental model of "who came first".

## [v0.6.5] — Label-toggle everywhere (PR #145)

Console-only follow-up to [v0.6.4](#v064--routing-weights-stats-bar-sync-pr-143).

### Highlights

- **One toggle shape across the entire Heuristics card.** Earlier
  releases left two different on/off affordances in the same table —
  a round track for PII / Completeness and a read-only `StatusPill`
  for SLM judge / Remote eval. PR #145 collapses both into one
  pill-shaped `LabelToggle`:
  - **on** → green pill `#1f9d55` with white `on` text
  - **off** → red pill   `#dc2626` with white `off` text
  - same shape, same `min-width`, same font metric in both states
- **Interactivity is preserved where it exists.** PII / Completeness
  flip calls the existing `/api/eval/config` PATCH. SLM judge / Remote
  eval render with `aria-disabled` and a non-interactive cursor so
  they're visibly distinct from rows an operator can actually flip
  while still showing what the env has the system set to. A footnote
  under the table makes plain that those two are env-driven.
- **Profile rows too.** Each `EvalProfile` row in the EvalProfiles
  card now uses the same `LabelToggle`, so the heuristic table and
  the profile table read from a single visual language.

### Notes for ops

- No backend change, no hot-path change.
- Old `.toggle-cell*` CSS removed; the round-track variant was no
  longer referenced after this PR.

## [v0.6.4] — Routing weights stats bar sync (PR #143)

Console-only follow-up to [v0.6.3](#v063--evalrules-switcher-parity-pr-140).

### Highlights

- **Upper stats bar now mirrors the routing weights card.** Saving the
  weights card rebalances the row to sum to 100% (existing behavior from
  #128), and now the upper eyebrow repaints in the same render: the
  thumbnails, the % labels, and the stats bar tiles (quality / cost /
  latency) all converge on the post-save values.
- **Smooth save/seek.** The 60/20/20 → 75/25/0 case no longer leaves
  the upper bar frozen on the pre-save numbers while everything below
  snaps to the rebalanced row.

### Developer notes

- UI only. The three `useState` calls were lifted from `WeightsCard`
  into `Eval` so the two surfaces bind to the same source. A small
  `useEffect([cfg.routing.weights])` re-hydrates the state on a fresh
  config fetch (the previous lazy initializer could leave the values
  clamped to 0 if the first render observed `cfg = null`).
- 49/49 tests pass; `tsc --noEmit` clean; `npm run build` clean.

## [v0.6.3] — EvalRules switcher parity (PR #140)

Console-only follow-up to [v0.6.2](#v062--evalprofiles-switcher--nexus-favicon).

### Highlights

- **Heuristic rows use the same switch cell as profiles.** The PII / Completeness rows
  inside the Heuristics card now use the shared `ToggleCell` (extracted in
  PR #138/#139). Drop the previous `StatusPill on/off` + `Disable/Enable`
  button pair so the heuristics card and the profiles card share one
  visual affordance for the same underlying state. Keyboard (Space / Enter)
  works the same as on profile rows.
- **Shared `ToggleCell` component.** Extracted from the inline copy in
  `EvalProfiles.tsx` into `web/src/components/ToggleCell.tsx` so future
  in-row enable flags (routing groups, persona flags, etc.) drop in the
  same primitive.
- **SLM judge / Remote eval rows intentionally still use `StatusPill`.**
  Their `enabled` flag is env-driven and intentionally has no in-UI
  affordance, so the pill stays.

### Developer notes

- UI only. No backend change, no hot-path impact.
- Build / TSC / Vitest still clean.

## [v0.6.2] — EvalProfiles switcher + Nexus favicon

Console-only follow-up to [v0.6.1](#v061--evalprofiles-console-ux-consistency).

### Highlights

- **Switch cell.** Replace the old two-element "StatusPill on/off +
  Disable/Enable button" arrangement inside each profile row with a
  single `role="switch"` cell. Same shape and size regardless of
  state — off keeps the muted panel tone, on slides the thumb across
  the accent gradient. Space / Enter work too.
- **Nexus favicon.** A simple two-node / connector motif rendered as a
  32×32 SVG on the existing gradient, declared via
  `<link rel="icon" type="image/svg+xml" href="/favicon.svg">`.
  Browser tabs and bookmarks for `https://nexus.ffx.ai` show this
  instead of the default globe.

### Changed

- `web/src/pages/EvalProfiles.tsx`: introduces `<ToggleCell>`
  replacing the StatusPill + Disable/Enable pair; `busyToggle`
  state threaded through `<Group>` (mirroring `busyDelete`).
- `web/src/styles.css`: new `.toggle-cell` / `.toggle-cell-track` /
  `.toggle-cell-thumb` rules with `.toggle-cell-on` modifier.
- `web/index.html`: added `<link rel="icon">` tag.
- `web/public/favicon.svg`: new asset.

### Performance / hot path

No backend changes. `/v1/chat/completions` and the eval worker stay
byte-identical with v0.6.0 / v0.6.1.

### Upgrade notes

Existing deployments pick this up on the next `v*` tag push. Helm
chart version bumps to `0.6.2`, `appVersion` to `0.6.2`.

## [v0.6.1] — EvalProfiles console UX consistency

Visual follow-up to [v0.6.0](#v060--profile-driven-evals-go--python-sidecar--console-ui)
that aligns the new `Eval profiles` card and its drawer with the rest
of the console (Login / Playground / Keys / Credentials / Routing /
Eval / Audit / Overview).

### Highlights

- **Shared drawer.** The profile create / edit drawer now uses the
  same `<Drawer>` component (with the existing focus-trap, ESC-close,
  and overlay dismiss) as every other tab. No more bespoke
  `drawer-overlay / drawer-head / drawer-foot` markup.
- **Shared button palette.** Primary CTAs (`+ New profile`,
  `Create profile`, `Save changes`) are `btn-neon`; secondary actions
  (`Cancel`, `Disable`, `Enable`, `Edit`, `Delete`) are `btn-ghost`.
  The danger tone for `Delete` is a single `.row-action-danger`
  modifier on top of `btn-ghost`, no more raw `.btn.danger`.
- **Shared panel shell.** `Eval profiles` now lives in a `.panel`
  (matching `Audit`, `Credentials`, `Routing`, and the existing eval
  cards) instead of a one-off `.card` variant. Old `.card-head` /
  `.card-title` are now `.panel-head` / `.panel-title`.
- **Shared field rows.** Form rows in the drawer use the global
  `.field-row` class that `Login`, `Playground`, `Keys`, and
  `Credentials` already use. The local `.field / .field-label /
  .field-control` trio is gone.
- **CSS diet.** `styles.css` sheds the duplicate `.drawer-foot`
  block, the unused `.field / .field-control`, and the standalone
  `.btn.danger` rule. Bundle loses 0.35 kB of dead CSS.

### Changed

- `web/src/components/Drawer.tsx`: optional `testId` prop forwarded
  to the dialog `div` so the existing `data-testid="profile-drawer"`
  test query keeps working without coupling the tests to internal
  structure.
- `web/src/pages/EvalProfiles.tsx`:
  - Section wrapper: `section card evals-card` →
    `section panel profiles-card`.
  - Header: `card-head / card-title (h3)` → `panel-head /
    panel-title (h2)`.
  - Action buttons: `btn btn-primary` → `btn-neon`;
    `btn small` → `btn-ghost btn-small`; `btn small danger` →
    `btn-ghost btn-small row-action-danger`.
  - Field rows: `Field` component renamed to `FieldRow` and uses
    the global `.field-row` wrapper.
  - Drawer shell: replaced with the shared `<Drawer>` component.
- `web/src/styles.css`: removed `.field`, `.field-control`,
  `.btn.danger`, and the duplicate `.drawer-foot` block; added
  `.row-action-danger` for the delete button tone.

### Performance / hot path

No gateway / eval sidecar changes. This release is purely a console
UI alignment; `/v1/chat/completions`, eval worker tick, and the
secret resolver all stay byte-identical with v0.6.0.

### Upgrade notes

Existing `v0.6.0` deployments need no configuration changes — the
console rebuilds from the existing Helm value
(`image.repository: ghcr.io/fun-fx/ffx_nexus`, tag pinned by the
Helm release). Helm chart version bumps to `0.6.1`, `appVersion`
to `0.6.1`.

## [v0.6.0] — Profile-driven evals (Go + Python sidecar + Console UI)

Replaces the global, env-only eval configuration with first-class,
per-evaluation **profiles** that admins can author, toggle, and scope
from the Console without redeploying the gateway.

### Highlights

- **One profile = one evaluator spec.** `EvalProfile` (`internal/evals/profiles.go`)
  carries the metric kind (`heuristic_pii` / `heuristic_completeness` /
  `slm_judge` / `remote_eval`), scope (`org` vs `user`), endpoint,
  `key_source` (`org` / `user` / `inline` / `builtin`), threshold,
  sample rate, and a metric-specific config blob. `Worker.ReplaceProfiles`
  swaps the active profile set on the next eval tick; profiles that are
  disabled in the UI are skipped at dispatch.
- **UI-driven secrets.** No more `OPENAI_API_KEY` / `JUDGE_URL` env vars
  required for evals. `SecretResolver` (`internal/evals/secret_resolver.go`)
  fetches keys from org credentials, the calling user's BYOK store, or
  an inline registered secret, matching the same precedence the gateway
  uses for normal traffic.
- **Dynamic per-request overrides.** `EvalOverride`
  (`internal/evals/override.go`) carries judge URL, judge model, and
  threshold from the active profile to the Python sidecar on every
  batch. The Python service honors request fields over env config
  (`eval-service/app/judge.py`), so changing a profile in the Console
  flows through without a restart.
- **Console CRUD UI.** A new *Profiles* card under the Eval page lets
  admins create / edit / enable / disable / delete profiles and groups
  them by scope (`web/src/pages/EvalProfiles.tsx`). `ProfileDrawer`
  encodes the key-source ↔ kind invariants client-side (heuristics are
  pinned to `builtin`), and the React Query cache invalidates on
  every mutation for instant feedback.

### Added

- `internal/evals/profiles.go`, `internal/evals/profile_store.go`,
  `internal/evals/profile_store_helpers.go` — `EvalProfile` schema,
  `ProfileStore` interface, and a persistent store wrapper.
- `internal/evals/secret_resolver.go`,
  `internal/evals/store_secret_lookup.go` — `SecretResolver` + a
  `core.Store`-backed lookup that mirrors gateway BYOK precedence.
- `internal/evals/batcher.go` — `Batcher` collects traces and flushes
  them to the Python sidecar in size/time-windowed batches
  (configurable `BatchConfig`).
- `internal/evals/override.go` — `EvalOverride` request envelope.
- `internal/console/eval_profiles.go` — `/api/eval/profiles` CRUD
  handlers (`profileCallerCanSee` / `profileCallerCanWrite`).
- `eval-service/app/schemas.py` — `EvalBatchRequest` /
  `EvalBatchResponse` and override fields on `EvaluateRequest`.
- `eval-service/app/main.py` — `/evaluate/batch` endpoint with
  concurrent metric dispatch (`asyncio.gather`).
- `web/src/pages/EvalProfiles.tsx` + `EvalProfilesCard` /
  `ProfileDrawer` — full CRUD UI.
- `cmd/nexus/profile_store.go`, `cmd/nexus/eval_runtime.go` — runtime
  wiring (`SeedProfilesFromConfig`, `SetSecretResolver`,
  `SetEvalProfiles`).

### Changed

- `internal/evals/worker.go` — eval execution now fans out across
  metrics (`scoreBag` + `runEvaluators`) so a request's eval latency is
  bounded by the slowest metric, not their sum. Sequential execution
  paths were removed.
- `internal/evals/remote.go` — `RemoteEvaluator` now accepts
  `EvalOverride`, supports `EvaluateBatch`, and runs on an
  `http.Transport` with keep-alive (`MaxIdleConns=64`,
  `MaxIdleConnsPerHost=16`).
- `eval-service/app/judge.py`, `eval-service/app/metrics.py` —
  LLM / embeddings / threshold construction prefers request fields
  over `settings`.
- `cmd/nexus/main.go` — startup seeds default profiles from the
  current env (if set) and registers a `SecretResolver` so existing
  configs keep working without editing the deploy.
- `web/src/pages/Eval.tsx` — renders the new `EvalProfilesCard` and
  forwards `isAdmin` to `EvalRules`.
- `web/src/api.ts` — `EvalProfile` types and CRUD helpers.

### Performance guardrails (hot path zero-impact)

- Profile resolution runs only on the eval worker tick, **never** in
  the gateway hot path. Gateway middleware, `ResolveCredential`,
  routing, and tracing remain unchanged for a `/v1/chat/completions`
  call.
- Eval dispatch uses fan-out + batched HTTP to the Python sidecar, so
  per-request eval overhead drops from `O(metrics)` to `O(1)` HTTP
  round trips and parallel metric latency.
- Remote evaluator HTTP client pools keep-alive connections
  (`IdleConnTimeout=90s`).
- Batcher caps in-flight queues (`MaxQueue`); on overflow it drops
  with a counter so a stuck sidecar can never back-pressure live
  traffic.
- `secretResolver` is invoked inside the worker goroutine; org/user
  lookups reuse the existing credential pool — no new
  database connections per request.

### Security / boundary

- Inline secrets are stored in-memory only (process-local); they are
  scoped to the registering user and never persisted.
- Profile mutations are gated by `profileCallerCanWrite`: only admins
  can edit org-scope profiles; users can only edit their own user-scope
  profiles.
- Eval override fields never leak credentials: the sidecar only sees
  the resolved key, not the source.

### Upgrade notes

- Existing deployments using `OPENAI_API_KEY` / `JUDGE_URL` env vars
  will continue to work: `SeedProfilesFromConfig` materialises a
  default profile from those vars on first boot.
- To go fully env-free, run `GET /api/eval/profiles` to find the
  seeded profile and `PATCH /api/eval/profiles/{id}` to attach an
  inline key (or switch `key_source` to `user` / `org`).
- Helm chart version bumped to `0.6.0`, `appVersion` to `0.6.0`.

## [v0.4.0] — user-defined OpenAI-compatible providers

Splits production deployment out of the public repo into a private
[`fun-fx/ffx_nexus_ops`](https://github.com/fun-fx/ffx_nexus_ops) ops repo
and lets any tenant plug an OpenAI-shaped upstream into the gateway without
us shipping a per-vendor Go adapter.

### Highlights

- **Own your provider.** From *Account → My provider keys (BYOK)* pick
  "Custom (OpenAI-compatible)…", give it a name (e.g. `openrouter`,
  `together`, `fireworks`, `mycorp-llm`), a base URL, and optional
  chat / embed model inventories. Nexus auto-registers a wrapper adapter
  on the next boot and exposes your models at `/v1/models` under
  `user/<provider>/<model>`. The Playground picker uses a live datalist
  bound to `/v1/models` so autocompletion discovers your entries on the
  fly.
- **On-prem repo separation.** The production pipeline (Talos + Cozystack
  + Kaniko + LAN Harbor) now lives in `fun-fx/ffx_nexus_ops` so internal
  identifiers never reach the public release. Verified by end-to-end
  green deploys from the ops repo while and after the public repo was
  made PUBLIC (`v0.3.6` → `main` rewrite → public visibility flip →
  tag-anchored release history).
- **Public repo hygiene.** `git filter-repo` rewrote history to scrub
  private node IP, Tailscale tailnet name, and LAN Harbor host (all
  replaced with `<node-ip>`, `<tailnet>`, and `harbor.<node-ip>.nip.io`
  placeholders or fully removed). 37 stale merged branches deleted,
  `v0.3.6` tag re-pointed at the cleaned commit, Dependabot baseline
  carried forward. **fun-fx/ffx_nexus** is now `visibility: PUBLIC`.

### Added

- `core.CredentialModels{Chat, Embed}` persisted as Postgres JSONB on
  `provider_credentials.provider_credentials` via new migration
  `005_credential_models.sql`. Additive — existing rows stay valid.
- `providers.UserCompat` wrapper around `OpenAICompat` that namespaces
  dynamic model ids under `user/<provider>/<model>` and strips the
  prefix on outbound calls so the upstream sees the raw model id.
- Console: Custom provider field group in *Account → My provider keys
  (BYOK)* (base URL + chat/embed model inventories), credentials table
  shows base url + "N chat / M embed" summary.
- `web/src/api.ts`: `fetchGatewayModels()` returning the `/v1/models`
  catalog grouped by `chat`, `embed`, and `user/<namespace>`.

### Changed

- `cmd/nexus/main.go: registerStoredCredentials`: any credential whose
  `provider` is not one of `openai|anthropic|gemini|groq|mistral|grid`
  falls through to `UserCompat`. base_url is required (logged + skip
  when missing).
- Console model picker (Playground prompt) gains a `<datalist>` backed
  by `/v1/models` for autocompletion.

### Security / boundary

- 1st-party providers and their catalog are unchanged.
- Built-in `provider == "openai"` credentials still go through the
  existing `OpenAI` adapter so third-party OpenAI-compatible endpoints
  do not piggyback on the builtin's catalog without an opt-in model
  inventory.
- BYOK precedence is preserved (`ResolveCredential` still wins
  user-owned over org-level).
- Backward compatibility: `CredentialModels{}` (empty) means "use the
  built-in default catalog", so existing 1st-party credentials behave
  identically.

## [v0.5.0] — Audit log + Onboarding + V1 observability + dev container

Closes the v1.1 design workstream (audit log WS-A, onboarding WS-B) and
ships the V1 observability stack introduced in `docs/scalability-plan.md`
(dev container, OTLP, Prometheus, replica_id). Plus the housekeeping
gaps that were blocking a "fresh clone → reproducible dev setup" path.

### Highlights

- **Audit log coverage** (WS-A of v1.1 design). Every state-changing
  admin/member action now writes to `audit_log` with a canonical action
  enum (`internal/core/audit.go`). The owner-deduped `target_id`,
  detail, and `actor_id` flow through `GET /api/audit` (admin-only)
  with `?action=`, `?user_id=`, `?limit=`, `?since=` filters and an
  RFC3339+duration-flexible parser.
- **Onboarding flow** (WS-B of v1.1 design). A new `users.onboarded_at`
  column (`migrations/postgres/008_onboarded_at.sql`) is stamped the
  first time a member successfully creates a provider credential. The
  React `Account.tsx` surfaces a 3-step `OnboardingChecklist` banner for
  members with `onboarded_at IS NULL`, auto-hiding on first credential
  create. Inline `?` tooltips on credential / virtual-key sections, and
  a copy-pasteable curl snippet rendered after virtual-key creation.
- **V1 observability stack** (scalability V1). `docker compose
  --profile dev` now brings up Grafana, Prometheus, OpenTelemetry
  collector, and (optionally) Metabase in one command. Grafana is
  pre-loaded with eight panels matching the published Prometheus
  queries (latency p50/p95/p99, RPS by model, semantic cache hit rate,
  cost/hour, failover events, BYOK adoption, quality judge score, error
  rate). OTLP collector is wired but **silent** — it requires
  `NEXUS_OTLP_ENABLED=true` and `NEXUS_OTLP_ENDPOINT=...` to forward.
- **ReplicaID on failover alerts** (V4). The router now stamps a
  per-pod `replica_id` (env `NEXUS_REPLICA_ID`, default
  `<hostname>-<randid>`) on every failover event. The Grafana
  `Failover events / hour` panel still sums across all replicas, but a
  px-quick drill-down filter by `replica` now makes flaps attributable.
- **Eval judge → Prometheus gauge** (PR #89). After this PR closing
  the wiring, `nexus_eval_quality_score{model="…"}` is now fed whenever
  the SLM judge (Qwen2.5:7b via Ollama, or any OpenAI-compatible
  endpoint) fires — see Grafana panel 7. The dev compose file ships
  the 4 judge env vars (`NEXUS_JUDGE_BASE_URL`, `MODEL`, `API_KEY`,
  `EVAL_SAMPLE_RATE`) so a fresh `docker compose --profile dev up -d`
  immediately lights up the metric.
- **Metabase Pattern B takeover** (Pattern B production scenario). The
  Metabase BI adapter leaves pre-existing customer-registered
  datasources and collections alone unless it sees a `nexus-managed`
  ownership marker. The `scripts/takeover_metabase.sh`-style
  operator
  workflow updated to stamp the marker idempotently, so adopting a
  customer's existing Metabase instance in production is a one-shot
  PR that does not destroy their dashboards.
- **V5 single-pod ceiling measurement** (PRs #84, #85). The
  `scripts/test_v5_ceiling.sh` script quantifies single-pod capacity
  via `wrk + GOMEMLIMIT=768MiB + GODEBUG=gctrace=1`. Results written
  up in `docs/v5_stress_ceiling_results.md` (p99 80–115 ms at 1000
  concurrent; 23–29 k req/s throughput plateau; linear RSS; no STW
  cycle observed). Deployment-version tuning knobs (`GOMEMLIMIT`,
  `GOGC`, per-vkey concurrency cap) are exposed in `values.yaml`
  under `config.runtime.*` and `config.maxConcurrentPerKey`.
- **Dev container one-command setup** (PR #82). `.devcontainer/`
  brings the `docker compose --profile dev up -d` stack at the
  root of any clone plus a matching Vite dev server for the React SPA.
  Replaces the "zip a tunnel of scripts and README pointers" pattern
  with a single VS Code "Reopen in Container" flow.
- **Repo hygiene cleanup** (PR #90). 9 orphan scripts and
  `cmd/loadgen` removed (no caller anywhere in the tree, no doc
  references, not in CI). The CI integration suite
  (`scripts/test_all.sh` + 13 sub-scripts) is intact and remains the
  authoritative E2E.
- **Helper charts** (`deploy/helm/nexus/Chart.yaml`) bumped to
  `version: 0.5.0` / `appVersion: "0.5.0"` so a default `helm
  install` pulls a current image in lock-step with what the dev container
  runs (previously chart's default was `0.3.3`, more than a major's
  worth behind the binary).

### Backward compatibility

- Audit log: `users.audit_log` table unchanged; new columns are nullable
  so older rows render in `/api/audit` with empty actor_id.
- Onboarding: a fresh `onboarded_at` column added at boot via the
  008 migration — idempotent and nullable for legacy users.
- Observability: existing /metrics surface alias kept; OTLP collector
  stays silent until opted in.
- Helm: chart version `0.3.3 → 0.5.0`. values.yaml keys and structure
  are unchanged — only the *default image tag* moves forward, so an
  existing `helm template | kubectl apply` followed by image pinning
  continues to work.

### Commits & PRs in this release

- PR #79  feat(observability): V1 dev container + OTLP + Prometheus + replica_id
- PR #80  feat(test-tools): loadgen + bound mock upstream + multi-node/stress bench
- PR #81  feat(metabase): Pattern B takeover script + e2e harness + seed dashboards
- PR #82  feat(devcontainer): one-command dev environment
- PR #83  feat(router): stamp ReplicaID on failover alerts
- PR #84  feat(stress): V5 single-pod ceiling measurement + first-pass results
- PR #85  feat(stress): extend V5 ceiling script with RSS + GC sampling
- PR #86  Audit log constants + filter parse + tests
- PR #87  feat(onboarding): mark onboarded_at after first credential create
- PR #88  feat(onboarding): first-run banner + help tooltips + curl code-snippet
- PR #89  feat(evals): propagate quality scores into Prometheus nexus_eval_quality_score
- PR #90  chore(cleanup): drop orphaned scripts and cmd/loadgen
- PR #91  chore(dev): wire SLM judge env by default in dev profile

## [v0.5.1] — Cursor Agent compatibility, raw SSE passthrough, Responses SSE shell

Targets the **Cursor Agent / Cursor Composer** traffic shape that drove the
v0.5.0 pilot. The pilot handoff letter
([`docs/release-notes/v0.1.0.md`](docs/release-notes/v0.1.0.md)) already flagged
that "the gateway must look like a first-class OpenAI + Responses endpoint from
both the public hostname and the API hostname" — this release closes the gap.

### Highlights

- **Cursor Agent "hybrid" bodies.** `POST /v1/chat/completions` now accepts
  Responses-shaped payloads out of the box: top-level `input` (string or
  array), flat Responses function tools, custom-type tools (e.g. `ApplyPatch`),
  `reasoning.effort`, `max_output_tokens`, Responses-only `tool_choice`
  shapes, and the full Responses extras (`store`, `include`,
  `prompt_cache_key`, `metadata`, …). `IsCursorHybridRequest` detects the
  shape by a cheap top-level key scan (no full decode on the hot path), and
  `TransformCursorHybrid` rewrites it to governance-aware
  `ChatCompletionRequest` so virtual-key limits, BYOK, guardrails,
  routing, eval, and quality routing all keep working.
- **Raw SSE passthrough for OpenAI-compatible providers.** OpenAI,
  the OpenAI-compat wrapper, and The Grid now stream the upstream
  Server-Sent Events byte-for-byte when the call is a passthrough-eligible
  model, instead of unmarshal-then-remarshal. Non-OpenAI-standard fields
  (`reasoning_content`, `thinking_blocks`, vendor-specific metadata) survive
  the trip end-to-end. The handler still parses one cheap copy per chunk
  locally for trace metrics, so the dashboard / cost / latency record is
  unaffected.
- **Responses SSE `response.completed` event.** `POST /v1/responses`
  streaming now emits a complete `response.completed` envelope per the
  OpenAI spec — `{id, object:"response", status, model, output[],
  usage, parallel_tool_calls, instructions, tools}` — closes open items
  as `status:incomplete` on truncated streams and emits `response.failed`
  with `trace.error_type=stream_error` when the upstream errors before the
  first chunk. Tool delta `call_id`s round-trip; a stable `call_<uuid>` is
  minted when the upstream never sets one, so parallel tool calls never
  collide on the cumulative `output[]`.
- **Public console vs API hostnames.** When `NEXUS_PUBLIC_GATEWAY_URL` is
  set (e.g. `https://api.nexus.ffx.ai`), the console renders that URL in
  the onboarding curl snippets and PlayGround SDK panel instead of the
  in-process listen address. The console also reverse-proxies `/v1/*` to
  the co-located gateway so the Playground and `/v1/models` discovery stay
  same-origin on the public console hostname; Cursor — which only trusts
  the API hostname — connects to that `NEXUS_PUBLIC_GATEWAY_URL` directly.
- **`/v1/chat/completions` array message content.** Cursor Agent arrays
  its `messages[].content` (text + file parts); the gateway now accepts
  both string and array content shapes per OpenAI's Chat Completions spec.
- **Inline guardrails: `maxInputChars` default raised.** The full-profile
  default was lifted from `20_000` to `200_000` bytes (≈ 50k tokens) so a
  single non-ASCII (Korean/emoji) or long-context request from Cursor
  Agent passes the inline guardrail without `403 guardrail_blocked`. The
  MaxInput in `.env.example` now mirrors the full-profile default.

### Added

- `internal/gateway/cursor_compat.go` — `IsCursorHybridRequest` +
  `TransformCursorHybrid` + `parseInputToMessages` + `normaliseTool` +
  `normaliseHybridToolChoice` + `wrapApplyPatchGrammar` +
  `pickResponsesExtras`. Preserves the Responses `tool_choice` hybrid
  shape, keeps `format`/`grammar` keys on `function.parameters.format`
  so ApplyPatch round-trips, and lets promoted Chat keys
  (`parallel_tool_calls`, `tool_choice`, …) not double-publish.
- `internal/gateway/providers/openai.go` — `scanOpenAISSERaw`,
  `parseOpenAISSEWithRaw`, and `sseEvent` buffer that emits `Raw`
  bytes; the handler selects between `parseOpenAISSE` (strict OpenAI)
  and the raw line when the Provider advertises a passthrough-eligible
  model set.
- `internal/console/gateway_proxy.go: SetGatewayProxy` + `loopbackGatewayURL`
  — the console listens for `/v1/*` and proxies to the in-process gateway
  on `127.0.0.1`, so the public console URL can serve `/v1/models` and the
  Playground without an extra hop.
- `internal/console/gateway_proxy.go: SetPublicGatewayURL` —
  env-driven `NEXUS_PUBLIC_GATEWAY_URL` plumbed to the React onboarding
  curl snippet, CSP `connect-src` allowlist, and PlayGround SDK panel.
- `internal/config/config.go: PublicGatewayURL` — new field wired through
  Helm chart's `configMap` via `deploy/helm/nexus/templates/configmap.yaml`.
- `internal/gateway/handler.go` — Cursor-hybrid detection path on
  `/v1/chat/completions`; array message-content shapes on
  `/v1/chat/completions`; Responses streaming `response.completed`
  shape with `instructions`, `tools`, `parallel_tool_calls`,
  `usage`, and OpenAI-spec output items.

### Changed

- `deploy/helm/nexus/Chart.yaml`: `version` bumped to `0.5.1`,
  `appVersion` to `"0.5.1"` so a default `helm install` pulls a current
  image in lock-step with what the dev container runs.
- `deploy/helm/nexus/values-full.yaml`:
  `NEXUS_GUARDRAILS_MAX_INPUT_CHARS` raised from `20000` to `200000`.
- `.env.example`: `NEXUS_GUARDRAILS_MAX_INPUT_CHARS` example value
  switched to `200000`.
- `internal/console/security.go` — CSP `connect-src` allowlists
  `api.<nexus-domain>` so the frontend can fetch the public gateway.
- `web/src/api.ts` — `api.nexus.ffx.ai` is the public-facing gateway
  base for the onboarding curl snippet.

### Fixed

- `internal/gateway/handler.go` — `/v1/chat/completions` no longer 400s
  on Responses-style bodies; Cursor Agent "hybrid" requests succeed.
- `internal/gateway/handler.go` — array message content (`[{"type":"text",…}]`)
  is parsed correctly (was 400'ing from Cursor Composer's file parts).
- `internal/gateway/responses.go` — streaming `response.completed` no
  longer drops `instructions` / `tools` / `parallel_tool_calls`
  fields; truncated streams close items as `incomplete` instead of
  pretending success; tool delta IDs round-trip through the cumulative
  output list.
- `cmd/nexus/main.go` — `PublicGatewayURL` is now sourced from config so
  the Helm chart ConfigMap key is honoured at boot.

### Security / boundary

- Cursor-hybrid detection runs **before** auth, so a malformed body still
  gets the standard 401-vs-400 split the rest of the API surface uses;
  Note: regression-tested against the existing
  `scripts/test_phase2.sh` and `test_phase234.sh` chains — no new tests
  are required to keep them green.
- The raw-SSE passthrough preserves comment / `id:` / `event:` lines
  verbatim so security headers (e.g. `:x-trace-id` style comments) on the
  upstream still surface to the client; no codepath actively forbids them.

### Upgrade notes

None. v0.5.1 is additive — existing clients that already speak
chat-completions or Responses keep working unchanged, and the Cursor
Agent compat is gated on detection (a true Chat body never enters the
hybrid path).

### Commits & PRs in this release

- PR #109 fix(gateway): accept array message content from Cursor Agent
- PR #110 chore(guardrails): raise maxInputChars to 200000 in full profile
- PR #112 feat(streaming, gateway): raw- SSE passthrough agent mode
- PR #113 fix(cursor): bridge Responses-shaped payloads onto Chat Completions
- PR #114 chore(release): bump chart to 0.5.1 for PR #113 gateway fix

## [v0.1.0] — initial strict-byok pilot release

First publicly consumable release. Grid team pilot.

### Highlights

- **Strict-BYOK by default.** Every gateway call uses the calling user's
  own stored provider key. Operator env keys remain loaded for
  visibility but never reach the data path unless the operator opts in
  via `NEXUS_ALLOW_SHARED_KEYS=true`. The "operator pays the bill"
  behavior from v0 is preserved as an explicit, documented escape hatch.
- **Welcome-first dashboard.** Fresh visitors land on a Sign-in panel,
  not on an empty Admin Console. Logged-in users get four tabs:
  *Overview · Playground · Audit (admin) · Account*. Demo data and
  per-user provider keys are wired into the *Playground* lane for one-
  shot chat completion testing without leaving the browser.
- **Byok-strict-byom path** for `the_grid` (The Grid spot-market) is now
  first-class in the dashboard — register a key under
  *Account → My provider keys (BYOK)* like any other provider.
- **Eval-driven model routing** (`model: "auto"` or named groups like
  `fast`/`smart`) is the new default story in docs and the demo.

### Added

- Dual-license: Apache-2.0 for the Go binary and infra, MIT for the
  React/TypeScript dashboard. `LICENSE` and `LICENSE-MIT` are committed
  at the repo root.
- New `scripts/test_strict_byok.sh` covers strict-byok gating and the
  `NEXUS_ALLOW_SHARED_KEYS` escape hatch.
- New dashboard tab: *Playground* — a one-pane chat-completion test
  surface modeled on LiteLLM's in-console playground.
- `docs/release-notes/v0.1.0.md` — pilot-handoff letter for the Grid team.

### Changed

- `NEXUS_KEY_MODE` default flipped from `shared` to `strict_byok`.
  Out-of-the-box, the operator never pays for user usage.
- `scripts/install.sh` no longer hard-codes `NEXUS_KEY_MODE`; it relies
  on the new default.
- README "BYOK & multi-tenancy" subsection: documents the new default,
  adds an opt-in shared-key fallback section.

### Fixed

- `web/src/api.ts`: `fetchMyStats`/`fetchMyTraces`/`fetchMyQuality` no
  longer throw `Unexpected end of JSON input` when the session expires
  between polls.
- `scripts/demo_reset.sh`: brings up the fake embeddings stub and the
  semantic-cache / guardrail env vars so the steps 7–9 of the demo
  walkthrough work on a fresh install.

### Upgrade notes

Existing self-hosters who relied on env-configured provider keys
running *for everyone* should set `NEXUS_ALLOW_SHARED_KEYS=true` before
upgrading to preserve old behavior. New deployments can leave the
default unchanged.

### Known limitations

- Strict-byok requires Postgres + `NEXUS_MASTER_KEY`. Without storage,
  the gateway falls back to `shared` and logs a warning — same as
  before this release.
- Playground uses `sessionStorage` to keep the user's virtual key warm
  across requests within a single tab; closing the tab prompts again.
- The Grid provider (`the_grid`) is not registered by default; it
  enters the registry only after a user registers a BYOK key for it.
