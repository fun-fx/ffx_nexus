# Tenancy model

This is the **contract** for what is separated between organizations inside one
Nexus installation and what is shared. It exists because the boundary cannot be
maintained by code review alone.

Every organization-boundary defect found during the security review had the same
shape: a query or a config lookup that was *obviously* correct until you asked
which organization owned the row. Reviewers cannot answer that question
consistently without a written classification, because for some resources the
correct answer genuinely is "shared". This document supplies the answer, and
§6 lists the tests that fail when code and document disagree.

**If you are adding a resource, classify it here first.** A resource whose class
is not written down will be classified by accident.

---

## 1. What a tenant is

**One customer = one installation.** Nexus is deployed into the customer's own
Kubernetes cluster with the customer's own Postgres, secret store and identity
provider. Two customers never share a process, a database or a key.

**Inside one installation, an organization is a department or team boundary.**
Marketing and Platform Engineering in the same company get separate orgs so that
one cannot read the other's spend, keys or traces, and so their vendor keys and
budgets are independent.

This distinction sets the strength of the boundary:

- The org boundary is an **authorization** boundary. It is enforced in the
  server's SQL, on every read and every write.
- The org boundary is **not** an infrastructure boundary. Orgs share a process,
  a connection pool, a rate limiter and a schema. It resists a curious or
  careless colleague and an attacker who has stolen one org's session. It is not
  designed to resist an attacker who has achieved code execution inside the pod.

**If two groups require infrastructure-level separation — separate encryption
keys, separate blast radius, separate compliance evidence — deploy two
installations.** Do not model them as two orgs and tell an auditor they are
isolated.

### 1.1 How a request's org is decided

From the authenticated session, always. The `X-Org-Id` header is consulted
**only** for requests with no session at all, and can never override a session's
org. Threading the org from the header when a session exists was a real defect
and is now a regression test.

### 1.2 Rows written before org attribution existed

Historical rows carry `org_id = ''`. They belong to the installation's **default
org** and to nobody else. They are never widened to "visible to all orgs" —
that widening *is* the leak.

Where the pre-attribution rows cannot be safely assigned (an installation that
was already running several orgs when the column was added), migration
`postgres/015` leaves them `unattributed` rather than guessing. Guessing would
attribute one org's evaluation scores to another. Reattributing them is a
documented manual operation, not something a migration does silently.

---

## 2. Resource classification

Three classes:

| Class | Meaning |
| --- | --- |
| **org-scoped** | Every read and write carries `org_id`. Cross-org access returns 404, not 403, so an id cannot be probed for existence. |
| **installation-global** | Deliberately shared by every org. Requires a justification in §3 and a test in §6. |
| **global, org-filtered view** | Underlying data spans orgs; the API only ever returns the caller's slice. |

### 2.1 Identity and access

| Resource | Class | Notes |
| --- | --- | --- |
| Users | org-scoped | Membership is per org. A user id from another org is not found. |
| Invites | org-scoped | The role comes from the server-side invite row, never from a URL parameter. |
| Sessions | org-scoped | The session carries the org; see §1.1. |
| Roles / RBAC | org-scoped | `admin` is admin *of an org*, not of the installation. |
| Audit log | org-scoped | An org's admin sees their org's entries only. |
| SSO / OIDC configuration | installation-global | §3.1 |

### 2.2 Keys, credentials and money

| Resource | Class | Notes |
| --- | --- | --- |
| Virtual keys (`nxs_live_…`) | org-scoped | Revocation SQL carries `org_id`; a missing predicate here was a real defect. |
| Provider credentials (BYOK) | org-scoped | An org's vendor key must never be spendable by another org. |
| Spend / cost records | org-scoped | Including the admin per-user spend view. |
| Budgets and rate limits | org-scoped | Enforced per key and per org. |

### 2.3 Routing

| Resource | Class | Notes |
| --- | --- | --- |
| Model aliases and routing policy | installation-global | §3.2 |
| `ModelStats` (`GET /api/routing`) | installation-global | §3.2 |

### 2.4 Evaluation

| Resource | Class | Notes |
| --- | --- | --- |
| Eval profiles | org-scoped, with operator-seeded cluster-wide rows | §4 — this is the highest-consequence classification in the product |
| Eval plugins | org-scoped, with operator-installed cluster-wide rows | §4 |
| Eval scores (`eval_scores`) | org-scoped | `org_id` added in `postgres/015` + `clickhouse/010`. |
| Eval score OTLP mirror | org-scoped payload, installation-global destination | §5 |

### 2.5 Observability

| Resource | Class | Notes |
| --- | --- | --- |
| Traces (list, detail, turns) | org-scoped | Every ClickHouse read carries the org predicate. |
| Window / provider / quality summaries | org-scoped | Six of these were missing the predicate and were fixed. |
| Prompt and completion content | org-scoped, and not captured by default | See the content-capture document. |
| Prometheus `/metrics` | installation-global | §3.3 |

### 2.6 Benchmarks

| Resource | Class | Notes |
| --- | --- | --- |
| Benchmark runs | org-scoped | A run spends the org's provider key against the org's dataset. |
| Benchmark schedules | org-scoped | Cross-org access returns 404. |
| Benchmark history (`/api/eval/benchmarks/history`) | global, org-filtered view | |
| Leaderboard quality blend | installation-global average + org-scoped run detail | §3.4 |

### 2.7 Integrations

| Resource | Class | Notes |
| --- | --- | --- |
| Grafana / Metabase URLs | installation-global | Operator config; a link, not data. |
| Email transport (Resend / SMTP) | installation-global | §3.5 |
| Failover webhook / Slack | installation-global | §3.5 |
| OTLP endpoints | installation-global | §5 |
| Benchmark provider token | installation-global | Operator-held vendor account. |

---

## 3. Why each installation-global resource is global

Each entry states the information sharing the customer is accepting. If a
customer's threat model does not permit it, the answer is separate
installations, not a configuration flag.

### 3.1 SSO configuration

One issuer, one client id, one claim mapping per installation. The customer has
one identity provider; per-org IdPs would mean per-org login pages on one
hostname.

**Shared:** every org authenticates against the same IdP. An org admin cannot
choose a different one.

### 3.2 Routing policy and model statistics

Aliases, fallback chains and the latency/error/throughput statistics behind them
are properties of the **upstream providers**, which all orgs share. "Is
`gpt-4o` erroring right now" is one fact about one vendor.

**Shared:** all orgs see aggregate model health — request counts, error rates,
latency percentiles per model. `ModelStats` carries no field capable of naming an
org, user, trace or prompt, and a test fails the build if one is added (§6).

**Also shared:** an org admin cannot define a private alias or a private fallback
chain. Routing is an operator-level decision.

### 3.3 Prometheus metrics

Time series are labelled by model, provider, status and route. Some series carry
`org_id` as a label, which means **a scraper can see per-org request volume and
cost**.

**Shared:** anything that can reach `/metrics` sees every org's operational
volume. This is why the endpoint is not on the public ingress and is reachable
only from the monitoring namespace. Treat Prometheus and Grafana as
installation-wide operator tools, not per-org dashboards.

### 3.4 Leaderboard quality blend

`GET /api/eval/benchmarks/leaderboard` shows the average benchmark score **the
router is actually blending** for each model. That number is installation-wide
because the router is installation-wide (§3.2); recomputing it per org would
display a number the router does not use, which makes the page lie about why a
model was chosen.

Per-run detail on the same page — run ids, min/max, sample counts — **is**
org-scoped, because a run reflects one org's spec, dataset and provider key. A
row can therefore show a blended average with an empty `latest_run_id`, which
reads correctly as "the router is using benchmark data for this model, but none
of it is yours."

**Shared:** one aggregate number per model. Not run ids, not datasets, not
scores per run.

### 3.5 Email and alert transports

One SMTP relay or Resend account, one failover webhook. These are operator
infrastructure; an org admin cannot redirect the installation's mail.

**Shared:** invite mail for every org is sent from the same domain, and failover
alerts for every org land in the same channel. **The failover alert payload
carries `org_id`, the virtual key id and the model alias**, so whoever watches
that channel sees which org failed over. Route it to a platform team, not to one
org's channel.

---

## 4. Eval profiles and plugins: the cluster-wide exception

This is the classification that matters most, because these two resources do not
merely *display* data — they **decide where data is sent**. Getting this wrong
does not leak a score; it forwards another org's prompts to a third party.

### 4.1 The rule

- A profile or plugin with an `org_id` belongs to that org. It is applied only to
  that org's traces and is visible only to that org.
- A profile or plugin with **no** `org_id` is cluster-wide: it applies to every
  org's traces. The seeded `default-pii` and `default-completeness` heuristics
  and Helm-mounted plugin manifests are the intended instances.
- **No HTTP request can create a cluster-wide row.** The console always stamps
  the caller's org, and a PATCH cannot clear it. Cluster-wide rows come only
  from operator seeding — env, Helm-mounted manifest directory.
- An org's own row shadows an inherited row of the same name rather than
  doubling it.

### 4.2 Why this is not simply "everything is org-scoped"

The operator has to be able to say "every department gets PII detection, no
exceptions." Making that per-org would mean each org could silently opt out of a
compliance control by deleting their copy.

The trade is explicit: a cluster-wide row is a **deliberate operator decision to
send every org's traces to one destination.** The `default-pii` and
`default-completeness` heuristics run in-process and send nothing, which is why
they are safe defaults. A cluster-wide *external* plugin means every org's
traffic reaches that vendor — correct for a company-wide Langfuse install,
wrong if departments have separate vendor contracts.

### 4.3 The defect this classification exists to prevent

Eval profiles originally had no `org_id`. Any org admin could create a judge
profile pointing at their own endpoint, and the worker applied it to **every
org's traces**, POSTing other orgs' prompts and completions to a URL of their
choosing. Nothing in the console showed it. This was not a read-authorization
bug; it was active data exfiltration with an authorization bug as its enabler.

The worker now filters profiles by the trace's org **before** any other
evaluation logic (`internal/evals/worker.go`, `collectEvaluators`), and
`internal/evals/profile_tenancy_test.go` pins it.

---

## 5. Outbound data paths

Every path by which data leaves the installation. "Whose config chooses the
destination" is the column that matters: when it differs from "whose data", you
have the §4.3 defect.

| Path | Data that leaves | Destination chosen by | Content? |
| --- | --- | --- | --- |
| **LLM upstream** (gateway proxy) | Full prompt and completion | Org's own credential `base_url`, else vendor default | Yes — this is the product |
| **Eval plugin dispatch** (langfuse, langsmith, datadog, braintrust, arize, confident_ai, arize_phoenix, otel_collector, webhook) | Whatever the manifest's payload template renders — commonly prompt, completion, reference | Plugin's org (or operator, if cluster-wide) | Per manifest |
| **SLM judge** (`heuristic`→`judge` profiles) | Prompt and completion, truncated to 4000 chars each | Profile's org (or operator, if cluster-wide) | Yes |
| **Remote eval sidecar** | Prompt and completion, truncated to 8000 chars, plus a `judge_url` the sidecar may call | Profile's org | Yes |
| **Trace OTLP export** | Metadata: org, user, model, tokens, cost, latency, error | Operator (`NEXUS_OTLP_ENDPOINT`) | No, unless capture is on |
| **Eval score OTLP mirror** | Metric name, score, rationale, evaluator, trace id | Operator (`NEXUS_OTLP_LOGS_ENDPOINT`) | Rationale may quote content |
| **Failover webhook / Slack** | Org id, virtual key id, alias, models tried, reason | Operator | No |
| **Invite email** | Invitee address, inviter address, org name, invite URL | Operator transport | No |
| **Benchmark provider** | Dataset/environment ids, model, and an **ephemeral virtual key** so the provider can call back through the gateway | Operator vendor account | Provider-side prompts |
| **Credential preflight probe** | The API key being tested | The `base_url` in the request body | No |
| **OIDC discovery / token exchange** | OAuth code, then claims | Operator (`NEXUS_SSO_ISSUER`) | No |
| **Metabase bootstrap** | Datastore connection strings | Operator (`NEXUS_METABASE_URL`) | No |

### 5.1 Every one of these goes through the egress guard

Rows 1–12 construct their HTTP client through `internal/egress`, which enforces
destination-IP policy, a mandatory timeout, and a bounded redirect chain that is
re-validated at every hop. See `docs/customer-self-hosted-security.md` §6 for the
policy, and §6 below for the test that fails when a new path bypasses it.

The guard's central point: **a tenant-supplied destination may not resolve to a
loopback, link-local or cloud-metadata address**, because the two paths that
POST prompt content to a tenant-chosen URL (SLM judge, plugin dispatch) also
store the *response*, which turns a naive fetch into credential exfiltration
with the result rendered in the console.

---

## 6. Where code and this document are pinned together

These tests fail when the implementation drifts from the classification above.
If you change a classification, change the test in the same commit.

| Statement in this document | Test |
| --- | --- |
| §2.3 / §3.2 `ModelStats` carries no tenant-identifying field | `internal/router/tenancy_test.go` |
| §4.1 a profile applies only to its own org's traces | `internal/evals/profile_tenancy_test.go` |
| §4.1 no HTTP request can create a cluster-wide profile | `internal/console/idor_test.go` |
| §4.1 a plugin resolves within the caller's org | `cmd/nexus/plugin_tenancy_test.go` |
| §2.2 keys, credentials, spend are org-scoped in SQL | `internal/core/org_isolation_integration_test.go` |
| §2.6 benchmark runs and schedules are org-scoped | `internal/core/org_isolation_integration_test.go`, `internal/console/idor_test.go` |
| §2.4 `eval_scores` rows carry an org and reads filter on it | `internal/migrate/eval_scores_org_integration_test.go` |
| §1.1 a session's org outranks `X-Org-Id` | `internal/console/plugin_org_forwarding_test.go` |
| §2.1 every route has a declared authorization policy | `internal/console/authz_inventory_test.go` |
| §5.1 every egress path uses the guard | `internal/egress/inventory_test.go` |
