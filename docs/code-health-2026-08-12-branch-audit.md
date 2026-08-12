# Code health audit — 2026-08-12

`sweeper` snapshot used to triage stale remote branches. Captured at the
end of `chore/code-health-cleanup` while reviewing what `git fetch origin
main --prune` was refusing to fold in. Lines below are produced by:

```bash
git branch -r --no-merged main \
  | xargs -I {} sh -c 'b={}; s=$(git log -1 --format="%s" "$b"); echo "$b :: $s"'
```

## Counts

- Total remote branches **not merged into main**: 39 (this list shows 39
  distinguishable commit subjects; some are duplicate subjects at different
  SHAs, so a few rows may collapse on follow-up audit).
- Categories (eyeballed from the subject line):

| Category | Count | Intent |
|---|---|---|
| `chore/release-v0.x.y` | many | These tag-shaped release branches — the **commit that v0.6.x shipped**. They predate the linear `main`-only release pipeline. Generally **safe to delete** once the production CD no longer references them. (Confirm with the `ffx_nexus_ops` repo workflow first.) |
| `feat/*-bench-*` | several | Bench launch / dry-run UI work scattered across multiple branches — most of these features have already landed on `main` through the merged PR chain (search for the subject in main). **Likely stale; safe to delete after a `git log main --grep` confirms.** |
| `feat/*-console-*` | several | Console UI work, several A11y / redesign branches. **Audit individually**, most predate the merged pages redesign. |
| `feat/eval-*` | many | Eval plugin mode / heuristic / profile merging work. **Cross-reference `main` history** before touching — many of these intent lines now live on `main`. |
| `docs/*` | several | Doc-only branches; rarely conflict, **safe to delete** if their content is on `main`'s `docs/`. |
| `fix/*` | several | Spot fixes (gofmt, secret-ref msg, env-push command). **Audit individually** for merge conflicts and time-since. |

## How to read this file

For each line, the format is:

```
origin/<branch> :: <latest commit subject on that branch>
```

To check whether its intent already landed on `main` without leaving the
branch in place, run:

```bash
git log --all --grep="<unique keyword from subject>" --oneline
```

If `main` already contains a commit with the same subject the branch is a
dead-end and can be deleted:

```bash
git push origin --delete <branch>
```

If only *part* of the work landed on main, evaluate whether the remaining
diff is worth its own cherry-pick.

If the branch's work is genuinely independent and the `cd-prod.yml`
workflow in the private `ops` repo references its SHA directly (look at
`/Users/munsojin/ffx_nexus_ops/deploy/cozystack/*.yaml`), treat it as a
release lock file and leave it alone until the equivalent manifest lands
on `main`.
origin/chore/console-ui-cleanup :: chore(console): drop legacy 5-tab UI after Pages migration
origin/chore/dist-snapshot-v0.6.3 :: chore(dist): sync build artifacts after v0.6.3 release
origin/chore/eval-plan-coverage-pass :: eval-stack: close plan coverage gaps (OTLP emit, strict-mode, references_from, 删除 no-op fetch stubs)
origin/chore/improve-secretRef-empty-msg :: chore(eval/plugins): clarify auth.secretRef empty error
origin/chore/release-v0.6.10 :: chore(release): v0.6.10 cost-regression fix
origin/chore/release-v0.6.11 :: chore(release): v0.6.11 — server-side trace filter + window pagination
origin/chore/release-v0.6.9 :: chore(release): v0.6.9 unified toggle + scope override
origin/chore/release-v066 :: chore(release): v0.6.6 heuristic split + stable profile order
origin/chore/release-v067 :: chore(release): v0.6.7 toggle round-trip + single heuristic panel
origin/chore/release-v068 :: chore(release): v0.6.8 console-level disable + reconfigure for SLM judge / Remote eval
origin/ci/bench-live-gate :: ci(bench): live contract gate for PG + ClickHouse
origin/ci/gofmt-drift :: fix: gofmt asks for trailing operator placement in benchmarks_test.go
origin/docs/benchmark-gateway-troubleshooting :: docs(bench): troubleshoot gateway 403 and api.ffx.ai hostname
origin/docs/eval-adapter-status :: docs(eval): correct the adapter table and record why OpenAI Evals is absent
origin/docs/langsmith-automation-rule-guide :: docs(plugins): walk operators through the LangSmith automation rule
origin/docs/v0.5-observability-architecture :: docs(observability): add V0.5 architecture diagram and OTLP runbooks
origin/feat/audit-log-coverage-tests :: feat(audit): central action constants + parse validation + tests
origin/feat/bench-blend-ui :: feat(ui): expose bench blend state on Eval and Routing list
origin/feat/bench-env-picker :: feat(bench): env push guide on 404 — copyable CLI + downloadable sample files
origin/feat/bench-provider-clickhouse :: feat(router): ClickHouse benchmark provider
origin/feat/bench-push-report :: fix(bench): rewrite env-push guide around the actual Prime CLI shape
origin/feat/benchmark-dry-run :: feat(bench): pre-flight dry run validates environments before launch
origin/feat/benchmark-prime-team-billing :: chore: gofmt internal/benchmark/types.go
origin/feat/console-a11y-tests :: test(console): vitest + axe-core a11y suite for new UI shell
origin/feat/console-key-resolver :: feat(console): in-product plugin keys (replaces chart-rendered Secret)
origin/feat/console-logout-button :: feat(console): Sign out control on the top bar
origin/feat/console-ui-grid-redesign :: feat(console): grid-inspired UI redesign — app shell, theme tokens, new pages
origin/feat/credential-preflight :: feat(console): pre-flight BYOK credentials before commit
origin/feat/daily-spend-dashboard :: formatting
origin/feat/dt-column-resize :: feat(web): resizable columns on the Traces page (#158)
origin/feat/dynamic-model-sync :: feat(gateway): refresh /v1/models from upstream providers in background
origin/feat/eval-api-v1alpha2-heuristic :: feat(eval): v1alpha2 schema + in-process heuristic backend
origin/feat/eval-config-only-plugins :: feat(evals): config-only plugins (Phase A–G)
origin/feat/eval-console-disable-sl-judge-remote :: feat(eval): console-level disable / reconfigure for SLM judge + Remote eval
origin/feat/eval-evaluators-merged-page :: style: gofmt -w on Phase A–G eval-plugin files (#162 follow-up)
origin/feat/eval-plugin-only-enforcement :: feat(eval): make plugin-only actually refuse Nexus-hosted eval compute
origin/feat/eval-plugin-only-mode :: feat(eval): honour NEXUS_EVAL_PLUGIN_ONLY to skip heuristic seeding
origin/feat/eval-profiles-pr135 :: feat(evals): per-eval profile schema, store, and console CRUD API (PR #135)
origin/feat/eval-quickstart-survey-gallery :: ui(eval): surface survey-driven presets + add quickstart gallery
origin/feat/eval-survey-followups :: feat(eval): survey-driven keep/remove/add closeouts (Add-B, C, vendor auth, dispatch routing)
origin/feat/eval-ui-plugin-first :: ui(eval): plugin-first layout when NEXUS_EVAL_PLUGIN_ONLY is on
origin/feat/eval-ui-pr137 :: feat(eval-ui): CRUD cards + drawer for per-eval profiles (PR #137)
origin/feat/eval-unified-toggle-v0.6.9 :: feat(eval): unify 4 metric toggles + add scope override rule
origin/feat/eval-wire-pr136 :: feat(evals): wire per-request override + secret resolver + TraceBatch (PR #136)
origin/feat/gateway-cost-usd-on-response :: feat(gateway): surface per-call cost as usage.cost_usd + x-nexus-cost-usd header
origin/feat/gateway-streaming-raw-passthrough :: fix(config): add NEXUS_PUBLIC_GATEWAY_URL and gofmt touched files
origin/feat/heuristic-dispatch-routing :: feat(eval): route ServiceHeuristic plugins through in-process evaluator
origin/feat/langsmith-auto-rule :: feat(plugins): Nexus creates LangSmith automation rules automatically
origin/feat/load-tools :: feat(test-tools): loadgen + bound mock upstream + bench/stress scripts
origin/feat/manual-trigger-ui :: feat(console): surface 'Run now' button for manual-trigger plugins
origin/feat/manual-trigger-ui-v2 :: feat(console): surface 'Run now' button for manual-trigger plugins
origin/feat/metabase-takeover :: feat(metabase): Pattern B takeover script + e2e harness + seed dashboards
origin/feat/model-benchmarks :: feat(eval): add model benchmarks via PrimeIntellect hosted evaluations
origin/feat/onboarding-frontend :: fix(nexus): apply 008_onboarded_at migration at boot
origin/feat/overview-turn-grouping :: feat(console+gateway): group overview rows by agent turn
origin/feat/overview-why-cards :: feat(console): replace Overview TierCard row with 'Why FFX Nexus' value props
origin/feat/plugin-keys-button-in-main-eval-page :: Remove deprecated secretRef from plugin form + expose Keys on /eval row
origin/feat/plugin-webhook-contract :: feat(eval-plugin): surface the inbound webhook URL on every plugin row + docs
origin/feat/prime-bench-max-use-suite :: ui(traces): restyle Since/Before datetime-local to match page tokens
origin/feat/prime-routing-blend :: feat(routing): blend PrimeIntellect benchmark scores into quality
origin/feat/provider-stats-and-grid-aliases :: fix(evals): stop the external scheduler from panicking on nil maps at shutdown
origin/feat/public-split-hostnames :: Merge branch 'main' into feat/public-split-hostnames
origin/feat/purge-legacy-profiles-on-boot :: Hard-delete legacy default profiles under plugin-only mode
origin/feat/resizable-table-columns :: feat(console): make div-grid tables column-resizable
origin/feat/router-scope-caller-filter :: feat(console): filter playground catalog per caller (team vs personal routers)
origin/feat/router-scope-metadata :: feat(gateway): tag every registered provider with a public/org/user scope
origin/feat/router-scope-ui-badge :: feat(console): playground model picker groups by visibility scope
origin/feat/scalability-delivery :: feat(observability): V1 dev container + OTLP + Prometheus + replica_id
origin/feat/scheduled-flush-ui :: feat(plugins): scheduled-flush now button + FireScheduled backend
origin/feat/session-rollup-overview :: fix(console): drop now-unused React imports from Overview
origin/feat/spend-chart-tweaks :: fix(spend): chart tweaks + Grid redirect flatten + reopenable drill
origin/feat/spend-hero-cards :: feat(spend): hero-card redesign (cost headline + savings-pct + 4-tile strip)
origin/feat/traces-server-filter-window :: feat(console): server-side filter + time-window pagination for traces
origin/feat/traces-turn-grouping :: feat(console): group Traces rows by agent turn
origin/feat/turn-continuation-linking :: feat(gateway): keep mid-flight operator replies in the same turn
origin/fix/anon-incognito-blank :: fix(console): guard anonymous routes so incognito opens render Login, not a black SPA
origin/fix/bench-dryrun-cancel-409 :: fix(bench): treat dry-run cancel 409 and billing FAILED as probe success
origin/fix/bench-launch-billing-error :: fix(bench): surface Prime billing failures on launch and validate
origin/fix/bench-slug-resolve-and-push-guide :: fix(bench): resolve env slugs before validate and rewrite push guide
origin/fix/benchmark-gateway-byok :: fix(bench): unblock gateway-routed hosted evals under strict BYOK
origin/fix/ch-contract-nullable-key :: fix(bench): live CH contract — nullable key + multi-row seed
origin/fix/cost-pricing-family-aliases :: fix(gateway): resolve cost regression for versioned and prefixed model ids
origin/fix/cursor-agent-hybrid-protocol :: style: apply gofmt to gateway package
origin/fix/dependabot-vitest-cve :: fix(deps): pin @rollup/rollup-linux-x64-gnu for CI runner
origin/fix/eval-plugin-auth-shape :: fix(eval-plugin): emit secretRef/keyRef under nested auth: block
origin/fix/eval-plugin-registry-reconcile :: test(eval/plugins): synchronise the reconcile loop's log sink
origin/fix/eval-plugin-response-source-header :: fix(eval-plugin): stamp response headers + row delete button
origin/fix/eval-plugin-test-502-and-org-scope :: fix(eval-plugin): stop reporting probe failures as 502, scope dispatch per org
origin/fix/eval-plugin-yaml-string-coercion :: fix(evals): decode plugin manifests that use quoted sampling/duration strings
origin/fix/eval-redaction-infinite-loop :: fix(eval): stop PII redaction from spinning on prompts that mention '@'
origin/fix/eval-toggle-disabled-and-profile-order :: fix(eval): split heuristic and env-driven tables; stable profile order
origin/fix/eval-toggle-wire-and-merge-tables :: fix(eval): PII/Completeness toggle actually toggles; merge panels
origin/fix/eval-weight-negative-clamp :: fix(console): clamp negative weights in Eval sliders + keep simplex invariant
origin/fix/eval-weights-free-drag :: fix(console): isolate Eval slider drags, normalise at save time
origin/fix/gateway-disagg-cost :: formatting
origin/fix/langfuse-keys-secretref-emit :: fix(eval/plugins): re-emit auth.secretRef so consoleKeyResolver can route keys
origin/fix/langfuse-plugin-auth-and-ingestion :: fix(eval-plugin): make the Langfuse plugin actually deliver data
origin/fix/langfuse-region-dropdown :: feat(eval/plugins): Langfuse region dropdown + tighter langfuse cloud preset
origin/fix/langsmith-auth-and-probe :: fix(eval): LangSmith probe asserted the wrong shape — traces never landed
origin/fix/otel-collector-form-transport-field :: fix(plugins): ship transport field in otel_collector preset
origin/fix/otlp-camelcase-json-keys :: fix(otlp): emit lowerCamelCase JSON keys so receivers stop discarding spans
origin/fix/overview-view-traces-link :: fix(console): View Traces and Open Playground buttons now actually navigate
origin/fix/persist-eval-plugins-and-keys :: style: gofmt struct field alignment
origin/fix/plugin-edit-patches-instead-of-duplicating :: fix(eval/plugins): edit a plugin in place instead of forking a duplicate row
origin/fix/plugin-org-scope-default-sentinel :: fix(eval/plugins): stop scoping console-installed plugins to a phantom org
origin/fix/plugin-preset-switching :: fix(plugins): preset chip now actually changes the form beneath it
origin/fix/plugin-test-body-hint :: fix(eval-plugin): surface body-hint on unexpected test failures
origin/fix/plugin-test-message-fidelity :: fix(eval-plugin): faithful probe-failure messages and typed webhook shape
origin/fix/plugin-test-result-shape :: fix(eval-plugin): Test action surfaces a typed PluginTestResult
origin/fix/plugin-trigger-dispatch-bug :: fix(eval-plugins): dispatcher honours Send.Trigger; scheduled + manual now work
origin/fix/router-weights-clamp-v3 :: fix(router): clamp negative weights before normalize
origin/fix/streaming-cost-trailer :: fix(gateway): actually send the per-call cost on streaming responses
origin/fix/turn-rollup-alias-shadowing :: test(observability): fail live turn tests when clickhouse is required
origin/fix/upstream-reported-cost-and-grid-lab-latest :: fix(gateway): record the spend providers report, and make Grid's lab-latest markets callable
origin/fix/vendor-probes-auth-verify :: style(plugins): gofmt pass for vendor probe comments
origin/release/v0.6.3 :: chore(release): v0.6.3 EvalRules switcher parity (PR #140)
origin/release/v0.6.4 :: chore(release): v0.6.4 stats-bar sync with weights card (PR #143)
origin/release/v0.6.5 :: chore(release): v0.6.5 label-toggle everywhere (PR #145)
origin/test/bench-e2e-live :: test(router): live E2E for benchmark snapshot -> routing blend
origin/test/collector-e2e-live-flow :: test(eval-plugins): live e2e tests for collector inbound path
origin/test/plugin-e2e-live-flow :: test(eval-plugins): live e2e tests covering all three trigger paths
origin/ux/eval-rules-switcher :: ux(eval-rules): use shared ToggleCell for PII / Completeness rows
origin/ux/eval-weights-bar-sync :: Merge branch 'main' into ux/eval-weights-bar-sync
origin/ux/evalprofiles-share-design :: ux(eval-profiles): align cards, drawer, and buttons with the rest of the console
origin/ux/evalprofiles-switcher-flat-favicon :: ux(eval-profiles): single on/off switch cell + Nexus favicon
origin/ux/label-toggle-everywhere :: ux(eval): label-toggle pill everywhere — on=green, off=red
