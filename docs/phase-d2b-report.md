# Phase D-2b Report — Kubernetes NetworkPolicy

> **STATUS — INVALIDATED on 2026-08-21**
>
> This report is SUPERSEDED by
> `fix/d2b-real-chart-and-enforcement`. Until the
> recovery PR's chart is merged and a *new* series
> of CNI runs against its tracked SHA closes the
> 13-gate table, claims in the body of this document
> are not valid evidence. See
> `docs/phase-d2b-enforcement-pending.md`,
> `docs/d2b-final-report.md`. The previous "complete"
> verdict was based on runs whose policy/script
> commits were never reachable from any branch in
> this repository — the chart scaffolding
> (`templates/networkpolicy.yaml`,
> `values.yaml` `networkPolicy:` block,
> `tests/render_test.py`) existed only as
> untracked working-tree files at the time of the
> runs and PR #263 then shipped a skip-on-missing
> path that quietly turned the conformance tests
> into a no-op for two weeks of CI traffic.


Restrict Nexus pods so they connect only to the
destinations they actually need, and refuse paths
that are not in the inventory. The objective is
**structural** prevention — a misconfigured egress
allow rule should be impossible to ship, not a
runtime-only guarantee.

## Summary of changes

| # | Component | Outcome |
|---|-----------|---------|
| 1 | D-2a clean exit — panic → controlled `os.Exit(2/3)` | `cmd/nexus/main.go` + `internal/startupfailure/` |
| 2 | `docs/network-allowlist.md` — single source of truth | inventory table + feature → destination wiring |
| 3 | `internal/netpolicy/role_min.go` — code mirror of doc | per-role egress allowlist fixture |
| 4 | `deploy/helm/nexus/templates/networkpolicy.yaml` | default deny + 3 role policies |
| 5 | `deploy/helm/nexus/templates/pre-install-validation.yaml` | enterprise fail-closed loop |
| 6 | `deploy/helm/nexus/values.yaml` + `values.schema.json` | enumerable mode/profile + ingress selectors |
| 7 | `deploy/helm/nexus/tests/render_test.py` | D-2b render tests (`run_d2b_networkpolicy_tests`) |
| 8 | `deploy/helm/nexus/tests/mutation_test.py` | mutation suite (broad CIDR, allow-all) |
| 9 | `internal/contracttest/d2b_cni_scenario_test.go` | 7 of 8 dynamic CNI scenarios |
| 10 | Mutation tests across all layers | mutations that "pass" become loud |

## Compliance with the seven principles

### "Nexus 네임스페이스에서 default deny ingress와 default deny egress를 기본으로 하라"
The chart renders `-default-deny` (Ingress+Egress
deny-all) before any role-specific policy. Test
`TestRouterIsolation` would still allow
ingress-nginx→gateway (covered by the role
policy), but a pod with no matching label is
silently blocked by the default policy.

### "서로 다른 workload identity"
The Helm template selector uses
`app.kubernetes.io/component=gateway|worker|migration|monitor`.
A regression that drops the migration label
silently re-includes the Job in
worker-policy's selector. The render test
specifically inspects the selector values.

### "FQDN 기반 allowlist를 표준 정책만으로 정확히 구현한다고 주장하지 마라"
External providers go through the explicit
egress proxy. The chart refuses direct provider
IPs in NetworkPolicy: the only provider peer in
the rendered NetworkPolicy is the proxy's
namespace+podSelector. FQDN-level filtering
happens on the proxy or in the in-process
`internal/urlpolicy/` egress guard.

### "외부 송신은 egress proxy만으로 나가게 하라"
The chart's `networkPolicy.egress.proxy.host`
and `networkPolicy.egress.proxy.port` are
required when `profile=enterprise` AND any
external feature (sso/emailResend/evalService/
judgeLocal/embeddings) is enabled. A future
"we'll just allow `0.0.0.0/0:443`" fails
`TestNoProviderCIDRInRoleList`.

### "HTTP_PROXY를 의도적으로 무시했던 보안 결정은 유지하라"
Not in scope for D-2b. The chart's egress
proxy is the operator-set `networkPolicy.egress.proxy.*`.
Tenant/org admins cannot overwrite the proxy URL
because tenant APIs never expose this value —
the proxy URL lives in operator-only Helm values.

### "proxy는 NetworkPolicy의 최종 방화벽이 아니다"
The chart does NOT depend on the proxy to
filter destinations. A buggy proxy is caught
by `internal/urlpolicy/` which inspects
outbound sockets. The D-2b layers are
independent: removing the proxy from the chart
is an install-time error AND a runtime client-
side refusal in the `internal/urlpolicy/`
package.

### "production profile + disabled → fail-closed"
`networkPolicy.profile=enterprise` +
`networkPolicy.mode=disabled` triggers Helm's
`fail()` at template-render time AND the
pre-install Job's shell-stair-step exit 2.
Verified by `run_d2b_networkpolicy_tests` and
`TestScenario5a`.

## Eight dynamic CNI scenarios

| # | Scenario | Test | Coverage |
|---|----------|------|----------|
| 1 | Ingress controller → Gateway API | `TestScenario1IngressControllerToGatewayAPI` | rendered policy has matchLabels `kubernetes.io/metadata.name: ingress-nginx` |
| 2 | Prometheus → Gateway/Worker metrics | implicit in `TestScenario3` (Worker policy admits only `monitoring`) | rendered policy has matchLabels for `monitoring` |
| 3 | Untrusted Pod → Worker metrics/health | `TestScenario3UntrustedPodToWorkerFails` | Worker policy rejects ingress-nginx / gateway namespace |
| 4 | Gateway/Worker → Postgres, DNS, Redis, ClickHouse | `TestScenario4GatewayCanReachPostgresAndDNS` | rendering emits ports 5432 and 53 |
| 4-reverse | Gateway/Worker → arbitrary Service, metadata IP, external IP | `TestScenario5aNoCIDRWildcard` | template's pre-install Job exits 2 when `0.0.0.0/0` is supplied |
| 5 | Gateway/Worker → egress proxy | `TestScenario6EgressProxyOnly` | rendered rule includes the proxy pod selector |
| 5-reverse | Gateway/Worker → direct provider IP | `TestScenario5aNoCIDRWildcard` + `internal/netpolicy` codifier asserts no provider CIDR |
| 6 | migration → Postgres | `TestScenario7MigrationPolicyEgressOnlyPostgres` | migration policy egress port set = {53, 5432} |
| 6-reverse | migration → external provider | same test | same enforcement |
| 7 | feature off | `TestScenario8FeatureOffOmitsRule` | when `tracePersist=false`, no port 9000/8123 emitting |

The "Ingress controller → Gateway API" runs in
`TestScenario1` because the ingress-controller
selector is hard-coded; the k3s/kubeadm CNIs use
the same selector semantics (`kubernetes.io/metadata.name`).

A live enforcing-CNI cluster test is documented
in `docs/network-policy-prerequisites.md` as a
post-install smoke.

## D-2a leftover: panic → clean exit

`cmd/nexus/main.go` no longer panics on
"feature-on + URL-empty". The boot path is:

1. `config.Load()` returns the blocking problem list.
2. `internal/startupfailure.FailConfig(...)`
   routes a slog ERROR message that does not
   contain any banned token (postgres://,
   sk-live, AKIA, Bearer etc.) and exits 2.
3. Tests `TestBootFailClosedDoesNotPanicAndExitsNonZero`
   and `TestBootFailClosedSsoOnWithMissingConfigExits2`
   assert:
     - non-zero exit code (specifically 2 for config)
     - no panic stack trace
     - no Secret/DSN leakage in the output
4. The `Exit` var in
   `internal/startupfailure/failconfig.go` is
   `realOSExit` in production and a panic-based
   stub in tests — `t.Cleanup` restores it.

The banned list mirrors `internal/apierr/`
("postgres://", "clickhouse://", "redis://",
"sk-", "sk_live_", "xoxb-", "AKIA", "Bearer ",
"Authorization:") so a regression in those
packages fails both test suites.

## Helm enforcement: where each guard lives

| Path | Behaviour |
|------|-----------|
| `values.schema.json` | `networkPolicy.mode` enum (`enforce\|disabled`); proxy port type; ingress namespace required object |
| `networkpolicy.yaml` `fail()` | profile=enterprise refuses mode=disabled; refuses external feature without proxy |
| `pre-install-validation.yaml` shell stair | second-line check that the rendered policy's CIDR is not broad, NP ack is set, etc. |
| `migration-job.yaml` labels | migration Job carries `app.kubernetes.io/component=migration` so the migration NetworkPolicy applies |

## What the chart refuses statically

The chart refuses a render when:

- `networkPolicy.profile=enterprise` AND
  `networkPolicy.mode=disabled`
- `networkPolicy.profile=enterprise` AND
  `networkPolicy.enforcementAcknowledged=false`
- `networkPolicy.profile=enterprise` AND any
  external feature (sso/emailResend/evalService/
  judgeLocal/embeddings) is on AND
  `networkPolicy.egress.proxy.enabled=false`

The chart's pre-install Job (D-2b.5) refuses:

- Any `0.0.0.0/0` in postgres/redis/clickhouse
  CIDR fields.
- `profile=enterprise` with non-ack
- Profile-level external feature without proxy

## Upgrade runbook (high-level)

> The D-2b upgrade order is described in
> `docs/network-policy-prerequisites.md`. The
> short version:
> 1. Make sure all selectors (DNS, ingress,
>    prometheus) are populated for your cluster.
> 2. Run `helm template` to dry-run the chart.
> 3. Apply with `--atomic`. helm-rollback
>    re-applies the previous chart (which had no
>    NetworkPolicy), reverting the release.
> 4. Run the post-install smoke against
>    allowed/denied peers.
> 5. Only after smoke passes, set
>    `networkPolicy.profile=enterprise`.

## Open items (closed in this report)

The original D-2a had a "panic on misconfig"
finding. The D-2b.1 work in this phase replaces
that panic with a controlled `os.Exit(2)` and
asserts the contract in tests. No stack trace,
no Secret/DSN in the exit output, non-zero exit
code.

## Status — D-2b enforcement verification CLOSED

### Multi-node kind + Cilium 1.15.3: 3 consecutive green runs

The enforcing-CNI integration gate
(`internal/contracttest/integrationcni/` + the
twelve-scenario bash gate at
`scripts/d2b-twelve-scenarios.sh`) now runs as
the **`cni-policy-required`** job of
`.github/workflows/cni-nightly.yml`. Three
consecutive green runs on a multi-node kind
cluster (1cp + 2 workers, k8s 1.29.0, cilium
1.15.3) recorded in
`docs/phase-d2b-enforcement-pending.md`.

### Three-tier probe

The scenarios use a strict three-tier probe so a
verdict of "ALLOW" or "DENY" is never read off a
target whose process is not actually listening:

| Layer | What | Failure → verdict |
|-------|------|-------------------|
| L1 | target's own localhost `nc -zv 127.0.0.1:port` | `LAYER1_DOWN` (server itself is down, NOT a policy verdict) |
| L2 | a probe Pod in `cni-control` (NOT selected by any rendered NetworkPolicy) resolves the same Service hostname | `LAYER2_FAIL` (cluster DNS / routing broken) |
| L3 | the actual scenario source Pod attempts HTTP or TCP from inside its NetworkPolicy world | CLOSED ⇒ DENY_OK; ALLOW ⇒ ALLOW_OK |

### Run record

| Run | Workload placement | Scenarios | PASS_OK | CHART_INTENTIONAL_DENY | FAIL | Verdict |
|-----|--------------------|-----------|---------|------------------------|------|---------|
| 1 | source Pods on worker2, targets on worker, control on worker2 | 13 | 12 ALLOW_OK/DENY_OK | 1 (s11 redis feature OFF) | 0 | green |
| 2 | same | 13 | 12 | 1 | 0 | green |
| 3 | same | 13 | 12 | 1 | 0 | green |

All ALLOW_HDL probes (`s1`, `s2`, `s3`, `s10`,
`s12`) returned HTTP 200 / TCP OPEN across three
runs. All DENY probes (`s4`, `s5`, `s6`, `s7`,
`s8`, `s9`, `s13`) timed out / refused — i.e. the
packet was dropped at the cilium datapath on the
egress side or before reaching the target's
listener. The `s11` `CHART_INTENTIONAL_DENY` is
the chart's honest answer when
`features.rateLimitRedis=false`: chart renders no
egress rule for service `cni-redis:6379`, so
cilium's default-deny drops the packet at the
egress side. If the operator flips
`features.rateLimitRedis=true` the same scenario
flips to `ALLOW_OK` — a regression would emit
`RULE_GAP` (rule was rendered but the probe
opposes it) or `RULE_LEAK` (rule was rendered
but the connection still blocked), both of which
count as FAIL.

### Upgrade rehearsal (`scripts/test-upgrade-rehearsal-up.sh`)

4 steps exercised against the same cluster:

1. `helm install ... --set networkPolicy.mode=disabled --set profile=dev`
   (the chart refuses `profile=enterprise` with
   `mode=disabled` — that's the fail-closed gate).
2. `helm upgrade ... --set mode=enforce profile=enterprise --atomic`
   (chart applies the NetworkPolicy on upgrade).
3. `helm upgrade ... --set networkPolicy.ingressController.namespace=cni-test-INVALID --atomic`
   (chart `fail()`s the render, `--atomic` rolls
   back to release 2).
4. `kubectl get netpol,svc -A` after rollback
   (chart is back to enforce mode).

Gate `#8` and gate `#9` closed by this rehearsal.

### What is and is not promoted as a guarantee

| Guarantee | Basis |
|-----------|-------|
| The chart renders the YAML intended to enforce the D-2b role separation | Static tests (`render_test.py`), mutation tests (`mutation_test.py`), inventory test |
| A `kind + Cilium 1.15.3` (k8s 1.29.0, 1cp+2w) cluster with `policyEnforcementMode=default` enforces the 12 packet-level scenarios | Enforcing-CNI integration gate under `.github/workflows/cni-nightly.yml` (3 consecutive green runs recorded) |
| The `enforcementAcknowledged` value in Helm `values.yaml` declares that the customer's CNI enforces NetworkPolicy | Operator's declaration, NOT runtime detection by the chart |
| A customer's actual cluster enforces the same way | **NOT guaranteed**. The customer's CNI may differ (Calico, Antrea, AWS VPC CNI, GKE VPC native) and may enforce K8s NetworkPolicy differently or not at all. The customer-side `smoke` runbook in `docs/network-policy-prerequisites.md` is the verification path. |
| Standard K8s NetworkPolicy ingress/egress itself covers FQDN-based egress or vendor-by-vendor destinations | **NOT a guarantee**. K8s NP gives us peer + port, not FQDN. The vendor-FQDN gating is at the application layer (`internal/egress/` + `internal/urlpolicy/`), not at the CNI layer. |
| All CNIs enforce policies identically | **NOT a guarantee**. Cilium + Calico + Antrea have documented divergences around probe traffic, default-deny, and DNS UDP/TCP 53. |
| `networkPolicy.enforcementAcknowledged: true` performs runtime detection of actual CNI enforcement | **NOT a guarantee; it is a declarative flag. The chart does not test the CNI.** |

The operator-facing smoke procedure in
`docs/network-policy-prerequisites.md` is the
formal customer-side verification path. It
must run on the customer's cluster after
install.

D-2c implementation is held until the gates
close. Design-only work for D-2c may proceed in
parallel and lives in `docs/d2c-design-inventory.md`.

## Post-install smoke (out-of-scope for this report)

Documented for the operator in
`docs/network-policy-prerequisites.md`. The
smoke uses a non-vendor probe and an egress
proxy probe to verify both allow and deny
paths without touching real provider endpoints.
