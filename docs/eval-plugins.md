# Eval plugins (Nexus `external` evaluator)

> Status: v1alpha1, ships disabled in `v0.7.0`. Heuristic detectors (PII,
> Completeness) remain in-process and default-on. The legacy `slm_judge` and
> `remote_eval` evaluators stay available but are no longer self-installed by
> the runtime.

Eval plugins turn a remote evaluation service into one of Nexus's
evaluator kinds. Nexus ships with three first-class kinds today:

| Kind                  | Implementation                                | Default |
|-----------------------|-----------------------------------------------|---------|
| `heuristic_pii`       | in-process Go regex ([`internal/evals/heuristics.go:14-61`](internal/evals/heuristics.go)) | on |
| `heuristic_completeness` | in-process Go regex ([`internal/evals/heuristics.go:63-92`](internal/evals/heuristics.go)) | on |
| `external` (plugin)   | YAML manifest → plugin registry → dispatcher | off, admin opt-in |

Plugins are **config only**. The Nexus binary never ships the Python
sidecar or the SLM judge infrastructure by default — those are
preserved for back-compat as the "local backend implementations" of
the plugin contract, and `NEXUS_EVAL_PLUGIN_ONLY=true` refuses them
outright (see *Plugin-only mode* below).

### Plugins score traces, not models

A plugin's input is a trace that already happened. To measure a **model
against a dataset** instead, see
[`docs/model-benchmarks.md`](model-benchmarks.md) — that runs on an
external platform too, but it is launched by an operator rather than by
traffic, and its results land on `benchmark_runs`.

### Plugins do not need an eval profile

An enabled plugin receives traces on its own. Eval profiles configure the
in-process evaluators; the plugin evaluator is appended to whatever the
profile set produced, so installing a plugin is sufficient and creating a
profile changes nothing about it. Two things do gate it: the plugin must
be **enabled**, and its `spec.send.sampling` roll must pass.

Dispatch is also scoped to the trace's organisation — see
[Cluster-wide vs per-org](#cluster-wide-vs-per-org).

## Quickstart — Langfuse in 5 minutes

Langfuse is the adapter to start with: it is the one wired end to end,
and it self-hosts.

1. In Langfuse → **Project settings → API keys**, create a key pair. You
   need both the public key (`pk-lf-…`) and the secret key (`sk-lf-…`).
2. Store them in a Secret:

   ```bash
   kubectl create secret generic langfuse-creds -n <namespace> \
     --from-literal=public_key=$LANGFUSE_PUBLIC_KEY \
     --from-literal=secret_key=$LANGFUSE_SECRET_KEY
   ```

3. Project the Secret into the pod, because Nexus reads plugin
   credentials from the environment:

   ```yaml
   # values.yaml
   envFrom:
     - secretRef:
         name: langfuse-creds
   ```

4. In the Nexus console → **Eval → Plugins**, create a Langfuse plugin
   (the preset fills in the endpoint and the `public_key|secret_key`
   key ref), then press **Test**. A pass means the endpoint *and* the key
   pair are good; anything else prints the vendor's own message.
5. Enable the row.

Traces are then forwarded at the manifest's `spec.send.sampling` rate as
OTLP spans, and Langfuse-computed scores come back via webhook
(`mode: webhook`) or polling and are persisted to `eval_scores`.

If Langfuse stays empty, check the gateway logs for
`plugin dispatch failed` — dispatch errors are logged with the vendor's
response, since a rejected send is otherwise indistinguishable from a
vendor with nothing to report.

## YAML schema (v1alpha1)

```yaml
apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: langsmith-judge
spec:
  service:
    type: langsmith               # langsmith | langfuse | datadog | braintrust | arize | otel | webhook
    endpoint: https://api.smith.langchain.com
    auth:
      secretRef: langsmith-api-key   # K8s Secret or eval_credentials key_ref
  send:
    trigger: on_trace             # on_trace | scheduled | manual
    sampling: 0.1                 # 0..1
    payload:                       # Go-text/template strings over Trace
      input:  "{{ .trace.input_messages }}"
      output: "{{ .trace.output_messages }}"
      reference: "{{ .trace.eval_reference }}"
      metadata: { app: nexus, env: "{{ .env }}" }
    redact: [pii]                 # heuristic_PII pass → mask before send
  collect:
    mode: webhook                 # webhook | poll
    interval: 60s                 # only when mode=poll
    mapping:                      # flat-key remap, NOT JSONPath
      name:        "key"          # source field name (without $.)
      score:       "score"
      label:       "value"
      explanation: "comment"
      trace_id:    "trace_id"
      metric:      "metric"
  timeout: 30s
```

### Field reference

| Field                      | Required? | Notes                                                           |
|----------------------------|-----------|-----------------------------------------------------------------|
| `apiVersion`               | yes       | Only `nexus.io/v1alpha1` accepted today.                        |
| `kind`                     | yes       | Only `EvalPlugin`.                                              |
| `metadata.name`            | yes       | DNS-style, lowercase, ≤ 64 chars, kebab-case. Reserved words rejected. |
| `spec.service.type`        | yes       | Closed enum — see top row. Adds fail-fast protection against typos. |
| `spec.service.endpoint`    | yes       | URL of the upstream service.                                    |
| `spec.service.auth.secretRef` | one of two | Kubernetes Secret name. Resolved from the environment — see [Credentials](#credentials). |
| `spec.service.auth.keyRef` | one of two | Pipe-separated key names inside the Secret, in vendor order (Langfuse: `public_key\|secret_key`). |
| `spec.service.auth.inlineKey` | never    | Forbidden; secrets never appear in source-controlled YAML.     |
| `spec.send.trigger`        | yes       | One of `on_trace` (default — sends every trace inline), `scheduled` (Nexus buffers traces per plugin and forwards in a batch every `spec.collect.interval`), or `manual` (the dispatcher ignores inline traces entirely; an admin must POST to `/api/eval/plugins/<name>/fire` to flush). Buffer overflow for `scheduled` returns `ErrBufferFull` and the worker logs a structured drop event — see [Trigger semantics](#trigger-semantics). |
| `spec.send.sampling`       | yes       | `[0, 1]`, and now actually enforced per plugin. Use ≤ 0.1 by default to keep egress and vendor cost bounded. |
| `spec.send.payload`        | yes       | Map of strings; each value is a Go-text/template.               |
| `spec.send.redact`         | no        | Only `pii` is accepted today; masks email-shaped spans in every string field of the rendered payload as `[REDACTED:email]`, using the same conservative pattern as the heuristic PII evaluator. A bare `@` in prose is left alone. |
| `spec.collect.mode`        | yes       | `webhook` (recommended) — vendor pushes to Nexus via a URL Nexus renders in the UI. `poll` available; `sync` was retired (the inline path is now the OTel-native `gen_ai.evaluation.result` event, Add-C). |
| `spec.collect.interval`    | when poll | Polling cadence when webhook is unavailable.                    |
| `spec.collect.mapping`     | yes       | Flat-key map. Each value is the *source key* on the vendor's wire format (e.g. `key`, `value`, `comment`). Do NOT prefix with `$.` — Nexus does flat-lookup, not JSONPath. Adapters provide defaults; you only override when the vendor uses nonstandard keys. |
| `spec.timeout`             | no        | Default 30s.                                                    |
| `spec.service.metric.name` | when type=heuristic | Closed enum: contains / pii / exact_match / rouge_l. |
| `spec.service.metric.path` | when type=heuristic + metric.path-style | Optional. Reserved for future extension; today the closed enum is the only contract. |
| `spec.flags`               | no        | Array of strings. `strict` rejects unknown fields instead of silencing them. |

### Heuristic metric kinds

`spec.service.type: heuristic` runs the metric in-process on the
trace Nexus already collected. The plugin is *config-only*: no HTTPS
call, no OTLP send, no auth block. The orchestrator hands the trace
to the matching metric and writes the score straight to
`eval_scores`.

Every metric in the enum is pure Go on the worker goroutine, so a
heuristic plugin needs neither egress nor eval compute Nexus has to
host — which is what lets it sit beside config-only plugins without
reintroducing the dependency they exist to remove.

The closed `metric.name` enum:

| Name | Implementation | Notes |
| --- | --- | --- |
| `contains`  | In-process Go (`internal/evaluators/heuristic/local.go`) | Tests the output against a substring list (substrings from `metric.args`). |
| `pii`       | In-process Go (regex set) | Reuses the production heuristic_pii pattern list. Stays default-on (see *Why PII must remain default-on* below). |
| `exact_match` | In-process Go | Trivial case-insensitive string equality. |
| `rouge_l`   | In-process Go (LCS F-measure) | Rouge-L F1 score equal to 1.0 means a perfect recall/precision match. |

#### Removed: `hf_evaluate`, `lighteval`, `ragas`

These three ran HuggingFace Evaluate, LightEval and Ragas through a
Python subprocess inside the Nexus pod. They were removed: the
libraries need eval compute Nexus would have to ship, install and
run, which is precisely the dependency the plugin model exists to
avoid. A manifest still naming one is rejected at decode time with a
message pointing at the alternatives, rather than saving and then
scoring nothing.

For those metrics, use a vendor that runs them for you — Confident
AI (`service.type: confident_ai`) covers the Ragas-style RAG
metrics, and Langfuse or LangSmith can host an LLM-judge equivalent.

The grep-shaped `references_from` example below shows how to lay
out a heuristic plugin without an auth block, with the manifest
hooking up a custom reference path on the trace:

```yaml
apiVersion: nexus.io/v1alpha2
kind: EvalPlugin
metadata:
  name: llm-rouge-on-trace
spec:
  service:
    type: heuristic
    metric:
      name: rouge_l
      args:
        references_from: trace.metadata.reference   # flat-key path on the trace
  send:
    trigger: on_trace
    sampling: 0.2
  collect:
    mode: webhook            # the in-process branch has nothing to send back; webhook is still required for the manifest to parse.
```

### Result model — OTel-aligned

All incoming vendor records are normalised to
[`gen_ai.evaluation.result`](https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/gen-ai-events.md)
before being written to `eval_scores`:

| OTel field                       | `eval_scores` column |
|----------------------------------|----------------------|
| `gen_ai.evaluation.name`         | `metric`             |
| `gen_ai.evaluation.score.value`  | `score`              |
| `gen_ai.evaluation.score.label`  | `passed` (`(value ≥ 0.5) ? true : false`) |
| `gen_ai.evaluation.explanation`  | `rationale`          |
| `gen_ai.response.id`             | (joined later)       |

`evaluator` column gets the value `plugin:<metadata.name>` so a single
SQL query can segment plugin-sourced scores from heuristics or legacy
rows.

### Why PII must remain default-on

The TechSy survey places Confident AI and DeepEval at the top of
the closed vendor enum specifically because the open-source eval
frameworks all converge on adversarial / privacy testing as a
quality bar. The legacy `heuristic_pii` evaluator is Nexus's
nearest local analogue. Operators removing `heuristic_pii` because
they have a "better" external plugin is a regression — the
plugin path doesn't run inside the gateway, only after egress —
so a PII violation would land in `eval_scores` long after the
prompt had been sent to the upstream model. Heuristic PII runs
before any egress (the trace is checked first, then the plugin
sees a redacted copy). Do not turn heuristic_pii off even if
Langfuse or LangSmith is enabled.

### Plugin-only mode (`NEXUS_EVAL_PLUGIN_ONLY`)

If you want every byte of eval scoring to leave the cluster —
Langfuse, LangSmith, Confident AI, Datadog, Arize Phoenix, a
private OTLP collector — flip the pod-level flag
`NEXUS_EVAL_PLUGIN_ONLY=true` (or `config.evalPluginOnly: true`
in the Helm chart). The runtime controller then skips seeding:

- the `default-pii` and `default-completeness` heuristic profiles, and
- the legacy `default-judge` (local Ollama / vLLM) and
  `default-remote` (Python sidecar) profiles.

Scoring becomes a pure plugin-driven pipeline. The console's *Eval*
page also surfaces a banner ("Plugin-only eval mode") so the seam
is visible without grepping the pod's environment.

#### What the flag refuses

Skipping the seed alone was not enough: `NEXUS_JUDGE_BASE_URL` and
`NEXUS_EVAL_SERVICE_URL` commonly outlive the decision to go
plugin-only (a values file keeps pointing at `ollama` and
`eval-service`), and the worker used to build both evaluators from
those variables regardless of the flag. A cluster could therefore
report plugin-only while calling an in-cluster judge on every
sampled trace. The flag now closes every door to Nexus-hosted eval
compute:

- **At boot** the SLM judge and the eval-service client are not
  constructed, and the leftover env vars are named in a WARN so the
  operator can see what was ignored. Silence there reads exactly
  like a healthy judge.
- **Per trace** a `slm_judge` or `remote_eval` profile that survived
  in the store is skipped before its credential is even resolved.
- **At the API** a PATCH on `/api/eval/config` touching any judge or
  eval-service field is rejected, and saving a profile of either
  kind fails — an enabled row that never scores is the failure mode
  this is here to remove.
- **At runtime** `ConfigureJudges` is a no-op, covering direct
  callers that bypass the API.

Getting that compute back is a deployment decision: unset the flag.
The `RemoteEvaluator` and `SLMJudge` code and the `eval-service/`
directory stay in the tree as the on-premises option, disabled by
default and ignored under plugin-only.

#### Destructive companion (`NEXUS_EVAL_PURGE_LEGACY_PROFILES_ON_BOOT`)

The flag above is **non-destructive** by itself — it skips seeding,
but rows already in the profile store from a prior boot are
untouched. To converge the cluster on a clean plugin-only set
without manual console cleanup, opt into the destructive
companion:

```
NEXUS_EVAL_PURGE_LEGACY_PROFILES_ON_BOOT=true   # + NEXUS_EVAL_PLUGIN_ONLY=true
# Helm equivalent:  config.purgeLegacyProfilesOnBoot: true
```

When both flags are on the controller hard-deletes the four
well-known seed rows (`default-pii`, `default-completeness`,
`default-judge`, `default-remote`) from the profile store on every
boot. The console's *Eval* page surfaces a danger-tone banner so
admins can correlate after-the-fact deletions with their explicit
config change.

**Caution**: the rows deleted include `default-pii`. Operators who
flip on the destructive flag without a plugin that covers PII
detection (e.g. Confident AI adversarial-quality, Langfuse score
with PII rule) will let traces go unscored for personally
identifiable information. Confirm coverage first.

### PrimeIntellect mental bridge — local vs hosted

The survey's standout vocabulary is PrimeIntellect's local /
hosted split:

| Prime mode | Nexus kind |
| --- | --- |
| `prime eval` (no `--hosted`) | `ServiceType: heuristic` — but only for the four pure-Go metrics; see the caveat below |
| `prime eval --hosted`        | `ServiceType: <langfuse\|langsmith\|confident_ai\|arize_phoenix\|...>` (existing `external` plugin) |

The local row is a partial match, and the gap is the interesting
part. Prime's local mode runs a Python harness on the machine that
invokes it, which is the same shape as the `hf_evaluate` / `ragas`
metrics we removed. Nexus deliberately has no equivalent: its
"local" family is the four deterministic Go metrics, and anything
needing a harness runtime belongs on the hosted side. So the useful
translation of `prime eval` inside Nexus is *hosted* — point a
plugin at the vendor that runs the harness for you.

Operators new to Nexus benefit from the same mental model:
*in-process metrics are deterministic, zero-egress, and free of any
runtime Nexus has to host; external plugins are config-only and ship
every trace over the network.* Stop adding evaluator kinds without a
concrete in-process metric or external adapter — the closed enum is a
feature, not a limitation, because vendor bandwidth is finite and an
open-ended enum forces every plugin to ship its own authentication,
sampling, and observability plumbing.

### Why there is no OpenAI Evals adapter

OpenAI's Evals platform was deprecated on 2026-06-03. Existing evals
go read-only on 2026-10-31 and the dashboard and API shut down on
2026-11-30; OpenAI points migrating users at Promptfoo. Building an
adapter against an API with a few months left would ship a preset that
breaks on a known date, so we deliberately did not.

What we took from it instead is its shape — *dataset + graders + run*,
scored by a service you do not operate. That is the same contract as
`spec.send` (what leaves) plus `spec.collect.mapping` (how a grader's
verdict becomes an `eval_scores` row), which is why a vendor swap here
is a manifest edit rather than a code change.

### Inbound webhook contract (`mode: webhook`)

When the plugin declares `collect.mode: webhook`, the vendor must
POST evaluation results back to Nexus at:

```
POST /api/eval/plugins/<metadata.name>/webhook
```

The body is one of:

- a single OTel-shaped JSON object, or
- a JSON array of objects (batched deliveries are accepted and
  processed up to 1 000 per request).

The collector then applies `spec.collect.mapping` (JSONPath-less;
flat key lookup is enough for the OTel wire shape) and writes one
`eval_scores` row per object. A score is *passed* when the numeric
`score >= 0.5`, OR when the `label` is `pass`/`true`/`1`. Any other
label forces *failed*.

Minimum payload fields the collector honours:

```json
{
  "name":        "answer-relevance",
  "score":        0.83,
  "label":       "pass",
  "explanation": "Output addresses the user's question.",
  "trace_id":    "0a1b2c3d4e5f"
}
```

Below is how each first-class vendor maps these fields to the
OTel-aligned column model. The defaults in the manifest are already
tuned for these vendors, so most operators only need to set them
when their vendor uses keys not listed here.

#### Langfuse (cloud)

Langfuse's "Webhooks" feature (Settings → Webhooks → Create a
Webhook) accepts our URL above. Pick **Score created** as the
event, and the body your team receives is an array of score
objects with this shape:

```json
[
  {
    "id": "<score uuid>",
    "traceId": "0a1b2c3d4e5f",
    "name": "answer-relevance",
    "value": 0.83,
    "comment": "Output addresses the user's question.",
    "dataType": "NUMERIC"
  }
]
```

The default `spec.collect.mapping` (`name → name`,
`score → value`, `explanation → comment`,
`trace_id → traceId`) handles this **without override**.

#### LangSmith

LangSmith's "Webhook" automation sends evaluation results in their
native feedback shape:

```json
{
  "run_id": "0a1b2c3d4e5f",
  "key":    "answer-relevance",
  "score":  0.83,
  "value":  "pass",
  "comment":"Output addresses the user's question."
}
```

The bundled `langsmith-judge` manifest already sets
`trace_id → run_id` and `label → value`.

#### Custom HTTPS vendor

For anything outside the first-class list, use `service.type:
webhook` and author `spec.collect.mapping` directly. The collector
accepts any flat JSON object whose keys match the OTel-side names
above.

#### Quick verification from the UI

The "Test" button (`POST /api/eval/plugins/<name>/test`) confirms
the outbound side works — but it does **not** confirm the inbound
webhook. To smoke-test the inbound side from a workstation:

```bash
curl -X POST \
  -H 'Content-Type: application/json' \
  -d '{"name":"answer-relevance","score":0.9,"label":"pass","explanation":"smoke","trace_id":"smoke-trace"}' \
  https://<nexus>/api/eval/plugins/<plugin-name>/webhook
```

A `202 Accepted` response means Nexus queued the score; check the
`/eval` page a moment later — heuristic/legacy/plugin rows share
the same evaluator table, so the score will appear under
"plugin:<name>".

#### Operational note

The collector route is **unauthenticated by design** because
vendors don't have Nexus bearer tokens. To keep the inbound
endpoint safe in production:

1. Front the cluster with an ingress that requires an
   `X-Nexus-Webhook-Token` header for `/api/eval/plugins/*/webhook`.
2. Rotate the token via the same pattern as your plugin
   `secretRef`s (Kubernetes Secret + `secretKeyRef`).

Sigv4-style signature verification is deliberately not part of
the in-process handler — vendors vary too much and adding per-
vendor verification logic into the Nexus binary would couple us
to that vendor's signature format.

## Helm rendering

A single ConfigMap aggregates all plugin YAMLs declared under
`eval.plugins.configMaps`:

```yaml
# values.yaml
eval:
  plugins:
    configMaps:
      - name: nexus-eval-plugins
        files:
          - path: langsmith-judge.yaml
            content: |
              apiVersion: nexus.io/v1alpha1
              kind: EvalPlugin
              metadata:
                name: langsmith-judge
              spec:
                # …
```

The `deploy/helm/nexus/templates/eval-plugins-configmap.yaml` template
renders one `data.*` entry per plugin file. The Nexus binary mounts
the ConfigMap at `/etc/nexus/eval-plugins/` and `evalplugin.Registry.LoadFromDir`
absorbs every `.yaml`/`.yml` file at startup.

## Cluster-wide vs per-org

Two precedence layers exist:

1. **Cluster-wide** (this ConfigMap approach, and DB rows written with
   an empty `org_id`). Admin installs once; every org inherits the
   plugin.
2. **Per-org**. The admin REST endpoint `POST /api/eval/plugins`
   records a plugin in `eval_plugins` under the caller's org.

The runtime registry is keyed by `(org_id, metadata.name)`, so:

- Two orgs may install the same `metadata.name` without overwriting
  each other.
- Dispatch is scoped to the trace's org. A trace is only ever sent to
  its own org's plugins plus the cluster-wide ones — never to another
  tenant's vendor account. This is enforced by
  `Registry.EnabledForOrg`; `Registry.Enabled` spans every tenant and
  is reserved for tenant-agnostic work such as the result poller.
- When an org installs a plugin under a name that also exists
  cluster-wide, the org's row **shadows** the inherited one rather
  than adding a second send.
- Within one scope, a Helm-sourced record still beats a DB record of
  the same name, so admins can guarantee a baseline.

Creating, editing, or deleting a plugin through the console updates
the registry immediately, so the dispatcher picks up the change on the
next trace instead of at the next pod restart. Each save logs the
outcome — `eval plugin live` when the entry the dispatcher reads matches
what was stored, and a warning naming the reason when it does not.

That log line exists because the failure has no other symptom. A row
that is stored but missing from the live registry looks *identical* to a
vendor with nothing to report: the console lists the plugin as enabled,
the Test button passes (it probes the vendor directly), and dispatch
forwards nothing without an error. For the same reason the registry is
re-derived from the database every minute, so drift is repaired within
one interval instead of at the next restart — and a plugin installed
through one replica reaches the others. A reconcile that changes
nothing stays silent; one that moves the live set logs
`eval plugin registry reconciled from database`.

Polling follows within 30 seconds. The collector reconciles its poll
goroutines against the registry on that cadence, so a `mode: poll` plugin
created in the console starts collecting on its own; deleting one stops
its poller. Previously the poller set was captured once at boot, which
meant a console-created plugin never polled until the pod restarted.

## Why the Test button never returns 5xx

`POST /api/eval/plugins/{name}/test` answers **200 with
`{"ok": false, "message": "..."}`** when the probe fails. A failed
probe is a *result*, not a transport failure.

This matters in production: when the endpoint returned 502 for a
failed probe, Cloudflare treated it as an origin gateway failure and
replaced the JSON body with its own branded "Error code 502" HTML
page. The console then reported `auth or ingress likely intercepted
the request`, and the real reason was never visible. Keeping the
status at 200 guarantees the typed body survives every proxy in front
of the console.

## Migration from `slm_judge` / `remote_eval`

If your existing environment (v0.5 / v0.6) was running
`default-judge` or `default-remote` profiles, they continue to work
read-only. The `/eval` page surfaces a **"Migrate to plugin"** banner
with a one-click wizard that pre-creates a `langsmith-judge` plugin
stub so the same functionality can be achieved without the
in-cluster Ollama / Python sidecar.

For new installs (v0.7+) the runtime **does not seed
`default-judge` / `default-remote`**. PII / Completeness heuristics
still default to 100% sampling so fresh installs have working eval
coverage out of the box.

## Credentials

Keys are pasted into the console — the **Keys** button on the plugin
row — and never appear in source-controlled YAML. The manifest only
names them:

```yaml
auth:
  secretRef: langfuse-judge      # the plugin's own metadata.name
  keyRef: public_key|secret_key  # key names, in the order the vendor wants
```

`auth.secretRef` is no longer a Kubernetes Secret name; it is the token
the key store is addressed by, and the console emits the plugin's
`metadata.name` for it automatically. `auth.keyRef` is pipe-separated and
**ordered**: Langfuse takes public key then secret key, so a swapped pair
authenticates as nothing.

Pasted values are encrypted with `NEXUS_MASTER_KEY` — the same key that
protects `provider_credentials` — and stored in `eval_plugin_keys`. Each
pod caches them in memory after the first read, and boot warms that
cache, so dispatch pays no per-trace database round-trip.

Deployments with no control-plane database keep the keys in process
memory only, and they have to be re-pasted after a restart.

A plugin that declares an auth block and whose keys do not resolve
**fails dispatch and says so in the logs**. It does not fall back to an
unauthenticated request: every vendor rejects those, and the rejection is
invisible from Nexus, which reads as "the plugin works but the vendor has
no data". A manifest with no auth block at all still dispatches, so
self-hosted collectors that need no credential keep working.

Press **Test** after pasting the keys. For Langfuse the probe reads one
score off the authenticated API, so it verifies the endpoint, the key
pair and the API version together. Other vendors' probes only check that
the host answers HTTP.

Langfuse keys are also **region-scoped**: a key pair issued in the US
project returns 401 against the EU endpoint. Pick the region in the
plugin editor rather than editing the endpoint by hand.

### Trigger semantics

`spec.send.trigger` controls when each trace is forwarded to the
plugin. The YAML validator accepts three values:

| Value        | Behaviour |
|--------------|-----------|
| `on_trace`   | Every trace is dispatched inline through the vendor's HTTP endpoint. This was the only behaviour pre-fix; the dispatcher used to ignore `trigger` and always act as if it were `on_trace`. |
| `scheduled`  | Traces accumulate in a per-plugin FIFO buffer inside Nexus and are flushed as a batch every `spec.collect.interval`. The buffer is bounded (`SchedulerConfig.MaxBufferPerPlugin`, default 4096) so a vendor that hangs cannot exhaust the heap — overflow returns `ErrBufferFull` which the worker logs through the structured plugin-dispatch path. |
| `manual`     | The dispatcher's hot path ignores inline traces entirely; only the admin REST endpoint `POST /api/eval/plugins/<name>/fire` drives the plugin. The body is `{"trigger": "<audit-tag>"}`; if omitted, the handler stamps `<admin-email>@<RFC3339>` so the scheduler's `log.Info("manual eval plugin fire", "plugin", name, "trigger", trigger)` line correlates to the user without extra clicks. |

The dispatcher honours `trigger` per PR #198 — picking `scheduled`
or `manual` is now an actual control plane decision rather than an
intent-revealing, ignored-by-runtime field.

### Manual trigger UX (console)

A plugin with `spec.send.trigger: manual` shows a neon **Run now**
button on its row in the *Evaluators* card (and on the legacy
`/eval/plugins` page). Clicking it issues
`POST /api/eval/plugins/<name>/fire` against the admin endpoint
mounted in `internal/console/eval_plugins.go:pluginFireManual`:

```http
POST /api/eval/plugins/manual-judge/fire
Content-Type: application/json

{"trigger": "weekly-bake-off-2025-10-31"}     ← optional audit tag
```

The response includes the count and the audit tag the worker used:

```json
{
  "ok": true,
  "count": 17,
  "message": "manual eval plugin fire (trigger=weekly-bake-off-2025-10-31)"
}
```

Refresh behaviour: the response does not include per-trace outcomes
because the dispatch path is async — the buffer is drained into the
dispatcher's queue and the rest of the dispatch lifecycle is
identifiable by the structured `log.Info("manual eval plugin fire", ...)`
line. The row chip reads `manual` (warn tone) so the operator can spot
which plugins still need a push.

`scheduled` plugins do not surface a Run-now button: their flushing
is on the `spec.collect.interval` and the only knob is to wait (or
to short-circuit the interval by flipping the trigger).

### Durability

An install and its keys both outlive the pod that accepted them:
`eval_plugins` holds the manifest and `eval_plugin_keys` holds the
credentials. This was not always true — both lived in process memory, so
every rolling update silently uninstalled every console-installed plugin
while the console kept listing it as enabled, and **Test** kept passing
because it ran in the same process as the paste that preceded it.

## Adapters currently shipped

| `service.type` | Auth sent | Collect | Notes |
|-----------------|------|---------|-------|
| `langfuse`      | Basic (`public_key\|secret_key`) | Poll (real scores) + webhook | OTLP/JSON to `/api/public/otel/v1/traces`; polls `/api/public/v3/scores`. Cloud or self-host. The recommended default and the only adapter verified end-to-end against a live vendor account. |
| `langsmith`     | `x-api-key` (LangChain API key) | Webhook (poll = liveness only) | OTLP/JSON to `/otel/v1/traces`; survivability check on `/api/v1/info` requires a 2xx. **Live-vendor verified end-to-end**: the previous `Authorization: Bearer` form was silently 401'd by LangSmith, which is why the test button used to pass with no key — fixed in PR #195. Self-hosted LangSmith uses the same headers but with `https://<host>/api/v1` as the base URL. |
| `confident_ai`  | Basic pair, or single key | Webhook | Confident AI / DeepEval Cloud. Refuses to send when neither a pair nor a single key resolves, rather than posting anonymously. **Test** now reaches `GET /v1/projects` with the resolved credential; 401/403 surfaces as `credentials rejected (...)` rather than the previous `endpoint reachable` false-positive. |
| `arize_phoenix` | `Bearer` (optional) | Webhook | Self-hostable OTLP target; auth is optional because a local Phoenix usually has none. **Test** sends a `GET /v1/traces` (Phoenix returns 405 Method Not Allowed for non-POST; that is the cheapest live signal) with Basic/Bearer/empty depending on what the operator configured. |
| `datadog`       | `DD-API-KEY` | Webhook | Rewrites hex trace ids to decimal, which Datadog requires. **Test** hits `GET /api/v1/validate` with the resolved DD-API-KEY; 403 surfaces as `DD-API-KEY rejected (...)`. |
| `braintrust`    | `Authorization: Bearer` | Webhook | OTLP traces. **Test** hits `GET /v1/projects` with Bearer; 401 surfaces as `credentials rejected (...)`. |
| `arize`         | `Authorization: Bearer` | Webhook | Arize AX remote-evaluator endpoints. |
| `otel_collector` | Whatever the collector wants | Webhook | v1alpha2 adapter; pick the wire shape with `collect.transport`. Supersedes `otel` and `webhook`. |
| `otel`, `webhook` | — | Webhook | v1alpha1 names, both routed to the `otel_collector` adapter. |
| `heuristic`     | none (no egress) | n/a | Scores in-process; see *Heuristic metric kinds*. |

Every adapter reads its credential from the same `external.Target`, so
"the key is wired" is true across the table. What is *not* uniform is
verification: only `langfuse` has been driven end-to-end against a real
vendor project for *score collection*. For the others, treat a passing
**Test** as evidence the host *and* the resolved credential are
accepted — score rendering on the vendor dashboard still requires
following that vendor's onboarding steps.

WhyLabs and RagaAI have no adapter: their SDKs are Python-only and
neither exposes a generic "attach a score to this trace id" HTTP
endpoint. Promptfoo and DeepEval-as-a-library are development-time
tools that run in CI rather than services a gateway can post to — use
`confident_ai` for the hosted DeepEval path.

## LangSmith automation rule

> **Why this section exists.** LangSmith delivers traces to Nexus just
> fine — the issue is the *return path*. LangSmith's REST API has no
> generic "give me back the scores I generated" endpoint, so without an
> Automation rule the vendor simply keeps its output to itself and
> Nexus renders an empty traces page.

The console's LangSmith plugin drawer renders the same checklist
inline while you are installing, so you do not have to come back to
this document for every new project. The rest of this section is for
operators who want the full write-up.

### Step 1 — install the plugin

Create a key in [smith.langchain.com → Settings → API
Keys](https://smith.langchain.com/settings). In Nexus install the
`langsmith` preset, paste the key into the **Keys** modal on the
plugin row, and click **Test**. The probe returns "Auth accepted by
LangSmith." only if `/api/v1/info` answers 2xx with `x-api-key`
attached (PR #195). Earlier releases used `Authorization: Bearer`,
which LangSmith silently 401s.

### Step 2 — create one Automation rule

In [smith.langchain.com](https://smith.langchain.com) open your
project and choose **Automations → + New**, then set:

- **Trigger**: *On a run finish* (or *On a feedback created* if you
  prefer to score once reviewers have approved the run).
- **Action**: *POST to webhook*.
- **URL**: `${NEXUS_PUBLIC}/api/eval/plugins/${PLUGIN_NAME}/webhook`.

  The drawer fills that value in for you as you type the plugin name;
  the same URL is shown under **Manage → Inbound webhook** after
  install, so the two ends cannot drift.
- **Body** — the contract Nexus already accepts is:

  ```json
  {
    "name":        "<run name>",
    "trace_id":    "<trace id from the run>",
    "score":       <0 .. 1>,
    "explanation": "<optional rationale>"
  }
  ```

### Step 3 — verify the round trip

Send a single request through Nexus, watch its trace id surface in
LangSmith, then re-enter Nexus → **Traces** and confirm a score
arrived a few seconds after the run finish. If the trace is present
but the score never lands, the Automation rule is missing — the
LangSmith plugin is otherwise wired correctly.

### Things that look like a bug but aren't

- **Test passes without a key.** That was a real bug until PR #195;
  today the probe attaches the key from the Keys modal and refuses
  any non-2xx.
- **Manual runs tarry several minutes.** That is a LangSmith feature
  for batch scoring; the rule fires at the end of the run, not on
  each token.
- **Different project than the dropshot key.** API keys are scoped
  per project; an automation rule in project A cannot read score
  events from project B.
