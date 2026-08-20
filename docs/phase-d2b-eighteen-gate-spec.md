# Phase D-2b Twelve-Gate Scenario Spec

The D-2b enforcement verification gate is a
single Go test with twelve sub-tests, one per
allowed/denied peer pair. Run with `-tags=integrationcni`
against a fresh `kind + Cilium` cluster. The
spec is the executable contract; the code mirrors
this doc line-by-line so a divergence fails.

## Test fixtures (created at start of the gate)

1. `nexus-e2e` namespace with Helm chart installed
   in `mode=enforce profile=enterprise enforcementAcknowledged=true`.
2. `nexus-test-ingress-nginx` namespace with
   `app.kubernetes.io/component=ingress` Pod
   with mocked ingress-nginx controller labels.
3. `nexus-test-prometheus` namespace with
   `app.kubernetes.io/component=monitor` Pod
   serving Prometheus-compatible /metrics text.
4. `nexus-test-untrusted` namespace with
   `app.kubernetes.io/component=untrusted` Pods
   (TCPDump + curl + dig available).
5. `nexus-test-tcp-target` Service in `default`
   namespace exposing TCP listener; verified
   reachable when no NetworkPolicy applies, to
   prove the "without policy, connection works"
   baseline.
6. `nexus-test-egress-proxy-mock` Deployment in
   `nexus-test-proxy` namespace; netshoot TCPDump
   pod listens. The mock accepts CONNECT on
   operator-chosen port and forwards to the
   test-target Service. The mock records the
   CONNECT target and asserts it equals the
   Nexus-controlled endpoint (not a random
   fqdn bypass).

## Twelve scenarios

Each scenario has the same shape:

- `Arrival` — source Pod identity (namespace,
  label).
- `Destination` — target service:port.
- `Probe` — TCP/HTTP/DNS test code.
- `Verdict` — `allowed` or `denied` (network
  policy refused).
- `Negative control` — `kubectl exec`-based
  command the test runs to mechanically separate
  "rejected by policy" from "ClusterIP not yet
  scheduled".

### Scenario 1 — Ingress controller → Gateway API

| | |
|---|---|
| Source | nexus-test-ingress-nginx Pod, label matchLabels the gateway's NetworkPolicy expects |
| Target | nexus-gateway service Gateway port |
| Probe | HTTP GET via `curl --max-time 5` |
| Verdict | allowed (200 OK; response body `{ok:true}`) |
| Negative control | Confirm DNS resolves; if not, the test is misconfigured, not the policy |

### Scenario 2 — Prometheus → Gateway metrics

| | |
|---|---|
| Source | nexus-test-prometheus Pod |
| Target | nexus-gateway service metrics port (9101) |
| Probe | HTTP GET /metrics via `curl --max-time 5` |
| Verdict | allowed (200 text/plain) |

### Scenario 3 — Prometheus → Worker metrics

| | |
|---|---|
| Source | nexus-test-prometheus Pod |
| Target | nexus-worker service metrics port |
| Probe | same as #2 |
| Verdict | allowed |

### Scenario 4 — Untrusted Pod → Worker metrics/health

| | |
|---|---|
| Source | nexus-test-untrusted Pod |
| Target | nexus-worker service metrics port |
| Probe | `curl --max-time 5` |
| Verdict | denied: connection refused or expires |
| Negative control | Without the chart's policy applied, the same probe returns 200. A future regression where the Worker policy drops the Prometheus-only ingress rule is caught here. |

### Scenario 5 — Untrusted Pod → Gateway API

| | |
|---|---|
| Source | nexus-test-untrusted Pod |
| Target | nexus-gateway service Gateway port |
| Probe | `curl --max-time 5` |
| Verdict | denied |

### Scenario 6 — Gateway → Postgres / Redis / ClickHouse / DNS

| | |
|---|---|
| Source | nexus-gateway Pod (executed via `kubectl exec`) |
| Targets | hosted Postgres, Redis, ClickHouse in their `namespaceSelector`-permitted namespaces; DNS in `kube-system` |
| Probes | `nc -zv` for TCP; `dig +short` for DNS |
| Verdict | allowed |
| Note | The `feature.tracePersist` flag gates the ClickHouse egress rule; this scenario runs with `tracePersist=true` for the full allow-list, and a sub-test with `tracePersist=false` confirms the rules are absent |

### Scenario 7 — Worker → Postgres / DNS / required Worker deps

| | |
|---|---|
| Source | nexus-worker Pod |
| Targets | hosted Postgres, DNS |
| Probes | `nc -zv` for TCP; `dig` for DNS |
| Verdict | allowed |

### Scenario 8 — Gateway/Worker → arbitrary in-cluster Service

| | |
|---|---|
| Source | nexus-gateway or nexus-worker Pod |
| Target | `nexus-test-tcp-target.default.svc` (any TCP port) |
| Probe | `nc -zv` |
| Verdict | denied |
| Negative control | Without the chart's policy, the same probe succeeds |

### Scenario 9 — Gateway/Worker → link-local metadata IP

| | |
|---|---|
| Source | nexus-gateway Pod |
| Target | `169.254.169.254:80` (AWS / GCP metadata) |
| Probe | `curl --max-time 2 http://169.254.169.254/` |
| Verdict | denied (timeout or refused) |

### Scenario 10 — Gateway/Worker → egress proxy

| | |
|---|---|
| Source | nexus-gateway Pod |
| Target | nexus-test-egress-proxy-mock |
| Probe | TCP connect on operator port |
| Verdict | allowed |
| Negative control | Without the chart's proxy egress rule, this fails: i.e. the rule is the *only* external egress |

### Scenario 11 — Gateway/Worker → direct provider destinations

| | |
|---|---|
| Source | nexus-gateway Pod |
| Target | vendor IP (RFC5737 documentation range, e.g. `192.0.2.10:443`) |
| Probe | TCP connect |
| Verdict | denied |

### Scenario 12 — migration Job → PostgreSQL allows, → provider denies

| | |
|---|---|
| Source | migration Job Pod (run-once) |
| Target | hosted Postgres |
| Probe | `nc -zv` |
| Verdict | allowed |
| Source | migration Job Pod |
| Target | egress proxy |
| Probe | `nc -zv` |
| Verdict | denied |

## Network rule: how each scenario's "denied" is asserted

A naive test that asserts "curl did not succeed"
catches general connectivity problems too. The
correct assertion separates:

**Pattern A — connection refused**: the
destination does not respond at the IP/port.
CNI policy can drop the SYN without an ICMP
unreachable. Wait for full TCP timeout.

**Pattern B — iptables drop**: the kernel
emits an ICMP unreachable after policy rejects
the SYN; `nc -zv` may exit with "Connection
refused" rather than "timeout".

**Pattern C — namespace isolate**: target
Service has no Endpoints (e.g. no Pod matched
its selector). `nc -zv` exits with "no route
to host" or simply times out.

The tests standardize on:

1. `--max-time 5` curl probes with `--connect-timeout 3`.
2. `nc -w 3 -zv` for raw TCP.
3. Each scenario `expects 0` (verdict allowed) or
   `expects non-zero with stderr in {refused,timeout,i/o}`
   (verdict denied).
4. The negative control runs WITHOUT the chart's
   NetworkPolicy (`mode=disabled`) and confirms the
   denied probe succeeds — so the difference is
   demonstrably the policy, not operator error.

## Run command

```bash
make test-cni
```

is the canonical operator. The script:
- destroys any prior `nexus-cni-test` cluster
- creates a fresh cluster with Cilium

Multi-node enforcement: by default the cluster
is `1cp + 2 workers` so cross-node datapath
enforcement is exercised. Override with

```bash
KUBE_WORKER_COUNT=1 make test-cni  # dev only
```

This re-uses `scripts/d2b-twelve-scenarios.sh`
plus the same `install + scenarios + upgrade
rehearsal + teardown` pipeline used in the
`cni-policy-required` CI job.

## Three-tier probe

Each scenario runs three layers so a verdict of
"ALLOW_OK" or "DENY_OK" is never read off a
target whose process is not actually listening,
or off a cluster whose DNS / Service routing is
broken.

| Layer | Probe | Failure → verdict |
|-------|-------|--------------------|
| L1 | `kubectl exec <target-pod> -- nc -zv -w 2 127.0.0.1 <port>` | `LAYER1_DOWN` (NOT a policy verdict; record and continue if scenario explicitly tolerates external-IP targets via `IGNORE-L1`) |
| L2 | `kubectl exec <cni-control-probe> -- nslookup <target-host>` (Pod in `cni-control` namespace, NOT selected by any rendered NetworkPolicy) | `LAYER2_FAIL` (DNS / routing broken; NOT a policy verdict) |
| L3 | `kubectl exec <source-pod> -- curl --max-time 5 -sS -o /dev/null -w "%{http_code}" http://<host>:<port>/`  OR  `nc -zv -w 5` | `OPEN` / `HTTP:2xx` / `HTTP:3xx` / `HTTP:4xx` / `HTTP:5xx` ⇒ `ALLOW_OK` response;  `CLOSED` (timeout, exit 28, refused, "Could not connect") ⇒ `DENY_OK` answer |

For external-IP targets (s9 metadata, s13 vendor
demo IP), the scenario explicitly sets
`IGNORE-L1 IGNORE-L2` so Layer 3 alone decides
the verdict — the IP has no localhost listener
by definition and the hostname is not a Service
to resolve.

## Multi-node enforcement: 3 consecutive green runs closes the gate

D-2b's "actual enforcement" gate is closed only
when:

1. `scripts/d2b-twelve-scenarios.sh` returns
   PASS_OK ≥ 12, CHART_INTENTIONAL_DENY = 1
   (scenario s11, the chart-policy-driven DENY
   while `features.rateLimitRedis=false` was the
   default; chart deliberately omits the rule
   in the rendered manifest), and 0 DENY_LEAK /
   RULE_LEAK / RULE_GAP verdicts.
2. The same verdict is observed on **three
   separate workflow runs** of the
   `cni-policy-required` job on the same
   commit SHA, recorded in
   `artifacts/integrationcni/`.
3. The cluster has at least 2 worker nodes so
   `cilium-agent` cross-node datapath is in the
   test path.
4. The static label conformance test passes
   for the fixture (`TestFixtureLabelsConformToChart`
   on go test) so fixture Pods and chart selector
   labels are byte-for-byte aligned — otherwise
   every "DENY_OK" might silently be a "deny for
   the wrong reason".

A failure on any of these is a P0 and the gate
stays open. Re-runs (a new workflow_run record)
are recorded individually; the "3 consecutive
green" rule does not auto-merge failures into a
single "we're still trying" status.

## Mutation injection sequences

After the green path runs, the test injects
these mutations and re-runs the same scenarios:

| Mutation | Affected scenarios | Expected outcome |
|----------|--------------------|------------------|
| Allow `*` ingress on Prometheus selector | #4 #5 #8 | denied scenarios now allowed → fail |
| Allow port 8080 ingress to Prometheus in Worker policy | #3 | Prometheus now reaches Gateway port too → fail |
| Allow egress to arbitrary Service in Gateway policy | #8 | allowed when must be denied → fail |
| Drop egress 0.0.0.0/0 allow on Gateway | #10 #11 | both proxied and direct fail at network layer → fail |
| Disable migration policy | #12 | migration Pod can reach egress proxy → fail |

The mutations live in CI fixtures
(`scripts/mutation-*.yaml`) and are applied via
`kubectl patch networkpolicy ... -p "$(cat ...)"`
with the test then re-running the relevant
scenarios.
