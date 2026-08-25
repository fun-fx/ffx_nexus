# Phase D-2b — Enforcement Verification Tracker

> **STATUS — INVALIDATED on 2026-08-21**
>
> This file is SUPERSEDED by D-2b being reopened in
> fix/d2b-real-chart-and-enforcement. The "3 consecutive
> green runs" table below references artifacts that
> were never tracked into a merged commit in the
> repository at the time the runs were recorded:
>
>   1. The Helm chart in main did not contain a
>      `networkPolicy:` values block or a
>      `templates/networkpolicy.yaml` file. The runs
>      were produced by a working tree whose chart
>      scaffolding was untracked. Without a tracked
>      SHA, the runs cannot be reproduced from the
>      history — a future cherry-pick of those tests
>      would synthesize a green CI on a chart that
>      does not exist in main.
>   2. The conformance tests
>      (`TestFixtureLabelsConformToChart`,
>      `TestMigrationJobLabelMatchesNetworkPolicy`)
>      were modified to skip when the chart
>      scaffolding was absent in PR #263. A skip
>      silently vacates the verification instead of
>      surfacing the gap. Two weeks of green CI was
>      therefore evidence only that helm template
>      could be invoked, not that the rendered policy
>      enforced anything.
>
> **Consequence**: every "closed | green" entry in
> the table below is INVALIDATED. The recovery PR
> bundles (a) the real chart, (b) non-skip
> conformance tests, (c) a chart-inventory test,
> (d) the rerun-ready CNI workflow. New runs that
> close any gate must be recorded against the SHA
> of that recovery PR; until then D-2b is open.

This file pins D-2b's status honestly: every gate
below is the contract the customer / CI is asked
to keep green. Crossing out a gate means we have
artifacts to show for it.

A live **policy-required** job in
`.github/workflows/cni-nightly.yml` is the gate's
single source of truth. Pull requests that touch
network-policy code MUST be green on
`cni-policy-required` for branch protection to be
satisfied. A flake or a deterministic failure on
`cni-policy-required` is a P0: a security
regression in chart enforcement.

## What is and is not claimed by this gate

| Claim | Source | Evidence |
|-------|--------|----------|
| Nexus helm chart renders the right NetworkPolicy YAML for enterprise profile + enforce mode | Static contract tests in `internal/contracttest/d2b_cni_scenario_test.go` and Python `render_test.py` | Rendered YAML, parse-asserted |
| Fail-closed gates (broad CIDR, mode=disabled with profile=enterprise, no proxy with external features) | Python `mutation_test.py` + `internal/contracttest/integrationcni/d2b_mutation_live_test.go` | Helm `fail()` exit, cluster patch denied |
| Real enforcing-CNI behavior: ingress + egress + port separation + allow/deny at packet layer | `scripts/d2b-twelve-scenarios.sh` against `kind + Cilium 1.15.3` multi-node (1cp + 2 workers) | `cilium status` shows `policyEnforcementMode=default`, kernel drops packet, kubectl exec probe timed out / refused |
| Hub / customer CNI is identical to Cilium | **NOT claimed** | Operator's cluster can drift. CNI-aware smoke must run there |
| Customer's NetworkPolicy YAML is the same enforcement Nexus trusts | **NOT a claim** | `enforcementAcknowledged` is a *customer declaration*. The chart does not detect CNI non-enforcement |

## Gate inventory

| # | Gate | Owner | Status |
|---|------|-------|--------|
| 1 | CNI selection rationale | `docs/cni-comparison.md` | closed |
| 2 | Reproducible test cluster (kind 0.22 + k8s 1.29 + cilium 1.15.3, 1cp + 2 workers, pinned via versions.txt) | `scripts/test-cluster-up.sh`, `scripts/test-cluster-down.sh` | closed |
| 3 | Twelve TCP/DNS/HTTP scenarios with three-tier probe (L1 target localhost, L2 cluster DNS, L3 policy path) | `scripts/d2b-twelve-scenarios.sh` + `scripts/fixtures/integrationcni/` | **closed** (3 consecutive green runs on multi-node cluster) |
| 4 | Port-level ingress separation (ingress ≠ Prometheus, mutual block) | scenario spec §7-§8 enforced by render + runtime | closed |
| 5 | Egress proxy contract: mock proxy fixture, low-cardinality metric, operator-only source, credential hygiene | `internal/config` config.go EgressProxyAuthHeader=empty enforcement + runtime test | closed |
| 6 | DNS UDP/TCP 53 verified in real CNI | scenario spec §6 probes (gateway/worker egress DNS) | closed |
| 7 | Kubelet / probe traffic tested under chosen CNI | scenario pod readinessProbe + cilium identities observed before probe | closed |
| 8 | Helm upgrade disabled→enforce rehearsal | `scripts/test-upgrade-rehearsal-up.sh` | closed |
| 9 | Helm upgrade --atomic rollback tested with policy blocking migration | step 3 of `test-upgrade-rehearsal-up.sh` exercises `bad selector + --atomic` | closed |
| 10 | all-in-one vs split dev/prod mode separation | networkPolicy.profile=dev vs enterprise in NP template + scenario fixtures | closed |
| 11 | Mutation tests: omitted rule / bad selector / wildcard are caught in enforcement | `internal/contracttest/integrationcni/d2b_mutation_*_test.go` | closed |
| 12 | CI workflow + nightly + PR-required-on-policy-change | `.github/workflows/cni-nightly.yml` (lightweight+heavy jobs + concurrency) | closed |
| 13 | Static: rendered chart selector labels match the fixture pod template labels | `internal/contracttest/d2b_fixture_label_conformance_test.go` | closed |
| 14 | Required-check names documented and grep-able against the workflow file | `.github/branch-protection.md` | closed |

## Run record (local multi-node kind + Cilium 1.15.3)

The 3 consecutive green runs that close gate #3:

| Run | Date (UTC) | Cluster | Scenarios | PASS_OK | CHART_INTENTIONAL_DENY | FAIL | Verdict |
|-----|-----------|---------|-----------|---------|------------------------|------|---------|
| 1 | 2026-08-20T14:18:00Z | kind+k8s1.29.0+cilium1.15.3, 1cp+2w | 13 | 12 ALLOW_OK/DENY_OK | 1 (s11 redis feature OFF) | 0 | green |
| 2 | 2026-08-20T14:38:37Z | same | 13 | 12 | 1 | 0 | green |
| 3 | 2026-08-20T14:48:24Z | same | 13 | 12 | 1 | 0 | green |
| 3+upgrade rehearsal | 2026-08-20T15:04:56Z | same | 13 (post-upgrade) | 12 | 1 | 0 | green |

3 consecutive green runs = **D-2b ALLOW gate closed**.

### Verdict grammar

| Verdict outcome driven by chart_policy | Reported in scenario-summary.txt | Counts as PASS |
|----------------------------------------|----------------------------------|----------------|
| ALLOW scenario, L3 OPEN / HTTP 2xx / 3xx / 4xx / 5xx | `ALLOW_OK` | yes |
| DENY scenario, L3 CLOSED | `DENY_OK` | yes |
| ALLOW scenario where chart renders no rule (feature OFF) and L3 CLOSED | `CHART_INTENTIONAL_DENY` | yes — but a regression where the rule was rendered and yet L3 stayed CLOSED shows up as `RULE_GAP` and counts as FAIL |
| DENY scenario where L3 OPEN / HTTP 2xx | `DENY_LEAK` (security regression) | NO |

If a future run produces `CHART_INTENTIONAL_DENY` for an `ALLOW_FEATURE_OFF` scenario in a `features.<flag>=true` config, that is a `RULE_LEAK` regression and counts as FAIL.

## What "closed" means per gate

A gate is **closed** when:

1. The named artifact exists in the repo (`docs/`,
   `scripts/`, `internal/contracttest/integrationcni/`).
2. The artifact runs in a reproducible way on a
   fresh `kind + Cilium` multi-node cluster created
   by `scripts/test-cluster-up.sh`.
3. The failure path (negative scenarios / mutations)
   is automated, not manual.
4. Output is captured to `artifacts/integrationcni/`
   named `<script>-<run-id>.log` and `probes.jsonl`,
   so a regression is greppable.
5. Three consecutive green runs of `cni-policy-required`
   are recorded for the same commit SHA before the
   "actual enforcement" gate is declared closed.

## Why this exists

The static tests we shipped in earlier revisions
(`TestScenario*` in
`internal/contracttest/d2b_cni_scenario_test.go`)
prove that `helm template` produces YAML with
the right `matchLabels`, `ports`, and feature
gating. They DO NOT prove that the YAML, once
applied, actually drops packets. A customer's
cluster running a CNI without policy enforcement
would happily pass those CI checks while shipping
a chart that "looks protected but isn't".

This tracker is the difference between
**declarative compliance** ("the chart renders
a policy") and **enforced compliance** ("packets
are dropped unless the policy admits them").

## Nightly run workflow

`make test-cni` orchestrates:

1. `scripts/test-cluster-up.sh`: `kind create
   cluster --config=<kind-config>` (1cp + 2 workers)
   then install Cilium via Helm at pinned versions
   (cilium 1.15.3, k8s 1.29.0, kind 0.22.0).
   Versions are pinned in
   `artifacts/integrationcni/versions.txt` for every
   successful run.
2. `scripts/install-nexus-test.sh`: `helm template`
   the NetworkPolicy chart + apply the rendered
   NetworkPolicy + apply the twelve-scenario
   fixture Pods and Services.
3. `scripts/d2b-twelve-scenarios.sh`: run the
   twelve scenarios (three-tier probe) and write
   verdicts to `artifacts/integrationcni/probes.jsonl`.
4. `scripts/test-upgrade-rehearsal-up.sh`:
   disabled→enforce transition +
   `--atomic` rollback rehearsal.
5. `scripts/test-cluster-down.sh`: tear the
   cluster down to avoid leaking pods/state.

PR-required run: triggered by `.github/workflows/cni-nightly.yml`
when a policy-bearing file path matches. The
workflow runs two jobs — `cni-lightweight-gate`
(always on, <=30s) and `cni-policy-required`
(policy-changes-only, ~20-30min including cluster
up, install, scenarios, upgrade, teardown).
`cni-lightweight-gate` is the canonical merge-bar
on every PR; `cni-policy-required` is the
additional merge-bar on policy changes.
