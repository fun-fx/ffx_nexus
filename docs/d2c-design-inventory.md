# D-2c Capture Surface Inventory

> **Status**: verified against the tree, not proposed.
> Every row below was located in source before being
> written down. Rows the implementation plan claimed
> but which do not exist are listed separately under
> "Claimed and absent" rather than silently dropped,
> because the plan still cites them.
>
> `docs/d2c-implementation-spec.md` names this file as
> its source of truth. It did not exist when D-2b
> closed, while the plan's own prerequisite checklist
> marked it reviewed. This file is that gap being
> filled, so the plan's §2 table is superseded by the
> tables here wherever the two disagree.
>
> **Verification basis**: repository at merge commit
> `69c75bf`, the SHA on which the D-2b enforcing-CNI
> gate closed 3/3 (runs 33754919869 / 33781917109 /
> 33783150371).

---

## The one unconditional leak — closed in D-2c.3

**Status: closed.** The table below is the state as
audited in D-2c.1. `observability.CaptureGate` now sits
between the fan-out and the ClickHouse recorder, and
`config.CaptureTraceContent` (`NEXUS_CAPTURE_TRACE_CONTENT`,
chart `config.captureTraceContent`) defaults to false, so
the "Persist" and "Schema" rows below are no longer
reached unless an operator opts in. Rows 1–3 are
unchanged and deliberately so — see "Coupling the plan
missed".

The audited state, retained because it is the reason the
gate exists and the shape the gate has to cover:

| Stage | Location | What happens |
| ----- | -------- | ------------ |
| Trace build | `internal/gateway/handler.go:1138-1141` | `json.Marshal(req.Messages)` → `t.InputMessages`, inside no conditional |
| Completion attach | `internal/gateway/handler.go:685,792,801,1027`; `internal/gateway/responses.go:144,530`; `internal/gateway/messages.go:266` | `trace.OutputMessages = …`, likewise unconditional |
| Fan-out | `internal/observability/multi.go` `MultiRecorder.Record` | Trace passed **by value** to each recorder |
| Persist | `internal/observability/clickhouse.go:166,183` | `INSERT INTO gateway_traces (… input_messages, output_messages …)` |
| Schema | `migrations/clickhouse/001_init.sql:34-35,40` | both columns plain `String`; `TTL … + INTERVAL 90 DAY` |

The comment at `handler.go:1138` reads *"Capture input
messages (opt-in content capture; on by default in
dev)"*. There is no opt-in, and no dev/prod branch. The
comment describes a control that was never written —
the same failure mode D-2b hit twice, where a rule was
documented and left unenforced.

Reproduced in D-2c.1 per the plan's §7.1. Those tests
now live at `internal/gateway/capture_content_coupling_test.go`,
and they did not invert — the gate landed one layer below
the handler, so the handler still puts bodies on the Trace
and the assertions still hold. What changed is what they
are for: they are now the guard that the evaluators keep
receiving bodies. Retention is asserted separately by
`internal/observability/capture_gate_test.go` and
`cmd/nexus/compose_capture_test.go`. Git history holds the
original leak reproduction.

### Why "by value" is the load-bearing detail

`MultiRecorder.Record(t Trace)` hands every recorder its
own copy. The ClickHouse recorder and the eval recorder
are siblings, not a chain. Gating the durable write
therefore does **not** starve in-process evaluation — see
"Coupling the plan missed" below. This is what makes an
OFF default implementable without removing a feature.

---

## Durable capture surfaces

| Surface | Store | Column / key | Content | Gated today |
| ------- | ----- | ------------ | ------- | ----------- |
| Gateway trace input | ClickHouse `gateway_traces` | `input_messages` | full request messages, incl. tool messages and any base64 attachment parts serialised by `internal/gateway/messages.go:862` | **No** |
| Gateway trace output | ClickHouse `gateway_traces` | `output_messages` | full completion text | **No** |
| RAG eval context | ClickHouse `gateway_traces` | `retrieval_contexts`, `eval_reference` | caller-supplied contexts (`handler.go:1142-1149`) | only by the caller choosing to send `nexus_eval` |
| Semantic response cache | Redis / process heap | list at `semcache:{scope}:{model}` (`internal/semcache/cache.go` `redisKey:55`, `Redis.Store`, `Memory.Store`) | full cached response JSON | **No** — TTL only |
| Judge rationale | Postgres + ClickHouse `eval_scores` | `rationale` (`internal/evals/pg.go` `PGSink.WriteScores`, `internal/evals/clickhouse.go` `CHSink.WriteScores`) | judge explanation, which can quote the content it judged | **No** |
| Eval plugin manifest | Postgres `eval_plugins` | `spec_yaml` (`internal/evalplugin/pg_store.go:94-137`) | operator-authored YAML; carries Go templates referencing trace fields, not live bodies | n/a — operator's own document |

Attachments have no dedicated table or object store. Image
and document parts ride inside `input_messages` as
`data:…;base64,…` URLs when the request carries them.
Tool calls likewise have no table; they live inside the
same two JSON columns.

---

## Egress surfaces (not durable here, leaves the cluster)

| Surface | Location | Opt-in mechanism | Redaction |
| ------- | -------- | ---------------- | --------- |
| First-party OTLP export | `internal/observability/otel.go` `OTLPEnvelope:294` | bodies omitted; caller passes `extraAttributes` to include them | n/a — metadata only |
| Vendor plugin payload | `internal/evaluators/external/dispatcher.go` `Dispatch:127-155` | operator writes `input: "{{ .trace.input_messages }}"` in the plugin manifest | `spec.send.redact: [pii]` → `redactPayload:203-227`, **opt-in** |
| Langfuse / LangSmith OTLP | `cmd/nexus/langfuse.go` `langfuseSpanAttributes:102-124`, `cmd/nexus/langsmith.go` | payload keys `input`/`output` map to `gen_ai.input.messages` / `gen_ai.output.messages` | inherits the dispatcher's redact step |
| SLM judge | `internal/evals/judge.go:99-105` | evaluator enabled | truncated to 4000 chars |
| Remote (Python) evaluator | `internal/evals/remote.go:189-190` | evaluator enabled | truncated to 8000 chars |
| Eval score OTLP mirror | `internal/evals/otel_sink.go` `HTTPLogSink.EmitShip:86-101` | `NEXUS_OTLP_LOGS_ENDPOINT` set | scores only |

The first-party OTLP omission is the **one content
boundary in this codebase decided correctly**. It had no
test. `internal/observability/capture_boundary_test.go`
now pins both directions: bodies absent by default, and
the caller opt-in still functional — a one-sided test
would be satisfied by an exporter that had lost the
opt-in path as well.

Note the asymmetry worth an explicit decision later:
vendor egress of bodies is opt-in, but **redaction of
those bodies is also opt-in**. A manifest that templates
`input` without declaring `redact: [pii]` ships raw. That
is defensible — the operator wrote the manifest — but it
is a default, not an accident, and should be recorded as
one.

---

## Coupling the plan missed

The plan's §3.1 sets every `capture.*` flag to `false` at
boot. Applied literally to the trace bodies, that
silently disables evaluators, because they read the very
fields it would blank:

| Consumer | Location | Behaviour when bodies are empty |
| -------- | -------- | ------------------------------- |
| SLM judge | `internal/evals/judge.go:100` | returns early, no score |
| Remote evaluator | `internal/evals/remote.go:227,269` | returns early, no score |
| Completeness heuristic | `internal/evals/heuristics.go:30,77` | scores against empty text |
| Heuristic metric dispatch | `internal/evaluators/heuristic/local.go:56-60,99-102,240` | no haystack |
| PII heuristic | `internal/evaluators/heuristic/patterns.go:78-82` | returns early |

Because the recorders are siblings rather than a chain,
the resolution is to gate **persistence**, not the trace:
the eval recorder keeps receiving a populated Trace for
the request's lifetime while the ClickHouse write becomes
opt-in. Evaluation is unaffected; retention becomes a
choice. A plan that gated the trace at construction would
have traded a security default for a feature regression
and called it progress.

**Decided**: durable body capture defaults **OFF**
(D-2c.3). Existing installs lose dashboard body columns
on upgrade, so this needs a release note; evaluators are
unaffected for the reason above.

**Implemented (D-2c.3)** exactly as described:

- `observability.CaptureGate` (`internal/observability/capture.go`)
  strips `InputMessages`, `OutputMessages`, and
  `RetrievalContexts` from its own copy of the Trace.
  `EvalReference` — the fourth and last content column
  `CHRecorder.insert` writes — is kept, because it is a
  caller-supplied ground-truth label the console reads back
  to explain a score, not conversation content.
- `traceFanout` (`cmd/nexus/compose.go`) wraps only the
  ClickHouse recorder. The console hub is a live fan-out
  that retains nothing, the metrics recorder reads counters,
  the OTLP exporter already omits bodies, and the eval
  worker is deliberately left ungated.
- `config.CaptureTraceContent` defaults false, surfaced as
  `NEXUS_CAPTURE_TRACE_CONTENT` and chart
  `config.captureTraceContent` (boolean-constrained in
  `values.schema.json`). Boot logs the disabled state so
  the seam is visible without grepping env.

The two properties that make this safe are asserted rather
than reviewed, because a wiring mistake here is invisible
at runtime — the gateway serves traffic identically and the
bodies simply land in ClickHouse:

| Property | Test |
| -------- | ---- |
| Content stripped on the retaining branch, metadata intact | `TestCaptureGate_StripsContentWhenDisabled`, `TestCaptureGate_PreservesMetadataWhenDisabled` |
| One Trace, stripped for storage and intact for scoring | `TestCaptureGate_FanoutIsolation` |
| ClickHouse gated by default and never wired raw | `TestTraceFanout_GatesClickHouseByDefault`, `TestTraceFanout_ClickHouseIsNeverUngated` |
| Eval worker never gated, either setting | `TestTraceFanout_EvalWorkerIsNeverGated` |
| Bodies still reach the recorder chain | `TestCaptureTrace_InputReachesRecorderChain`, `TestCaptureTrace_OutputReachesRecorderChain` |
| A new content field on `Trace` cannot skip the gate | `TestCaptureGate_ContentFieldCountMatchesTrace` |

The drift guard in that last row is the one worth knowing
about: `Trace` will keep growing, and the next field that
carries customer text has to be stripped explicitly. The
test counts what the gate clears against
`contentFieldCount` so adding such a field without
touching the gate fails in CI rather than surfacing in a
customer's trace table.

---

## Storage reality

| Engine | Highest migration present | Next number |
| ------ | ------------------------- | ----------- |
| Postgres | `021_audit_view_indexes.sql` | 022 |
| ClickHouse | `010_eval_scores_org.sql` | 011 |

The plan proposes migrations `025`–`030`. That numbering
assumes a tree four migrations further along than this
one and must be renumbered before any of it is written.

---

## Chart reality

`networkPolicy.egress` has exactly one sub-block:

```yaml
networkPolicy:
  egress:
    proxy:
      enabled: false
      host: ""
      port: 3128
      namespace: ""
      podSelector: {}
```

`values.schema.json` marks `networkPolicyEgress` with
`additionalProperties: false`, and
`templates/networkpolicy.yaml:154-170` renders only the
proxy rule. The plan's §6.1
`networkPolicy.egress.clickhouse` and
`.objectStorage` blocks do not exist, so §6.4's
capture-gated NetworkPolicy test has nothing to assert
against yet.

Two constraints on adding them, both learned in D-2b:
`additionalProperties: false` means each new key needs a
schema entry or the chart refuses to render, and any new
namespace-valued field must reference
`#/definitions/namespaceNameStrict` or
`#/definitions/namespaceNameOrUnset` — a bare
`{"type": "string"}` is how an empty peer namespace
silently dropped an authorised peer while reporting a
successful install.

ClickHouse connectivity is currently outside every
NetworkPolicy rule. `serviceTargets.clickhouse`
(`values.yaml:762-766`) is inventory metadata, not an
egress rule.

---

## Claimed and absent

The plan's §1.3 hook table names seven decision points.
One exists. These do not exist anywhere in the tree, so
any phase that cites them is unexecutable as written:

| Plan claim | Reality |
| ---------- | ------- |
| `internal/evalprofile/store.go` `Save`, `prompt_template` | no such package; profiles are in-memory (`cmd/nexus/profile_store.go` `coreProfileStore.Save:76-97`, `internal/evals/profile_store.go` `MemoryStore.Save:138-159`) and `EvalProfile` (`internal/evals/profiles.go:91-140`) has no template field |
| `internal/evalplugin/emit.go` | absent; vendor emit is `internal/evaluators/external/dispatcher.go` plus `cmd/nexus/*.go` adapters |
| `internal/cache/cache.go` `Set` | absent; the cache is `internal/semcache/cache.go` `Store` |
| `cmd/nexus-backup` `Bundle` | absent entirely |
| `cmd/nexus-support-bundle` `Collect` | absent entirely |
| `internal/chat/store.go` `Insert` | absent; there is no per-request body table |
| `PostHook` in the gateway handler | absent; the finalize point is `h.recorder.Record(trace)` |
| tables `chat_request_response`, `chattrace_event`, `org_capture_policy`, `eval_profile` | none exist; traces live in `gateway_traces`, plugins in `eval_plugins` |
| S3/GCS object storage persistence | no object-storage client or upload path in the tree |
| body hashing (`body_sha256`) | absent; `sha256` is used for API keys, invite tokens, and migration checksums only |
| legal hold | absent |
| per-org capture policy | absent |
| CSV export wiring | `internal/core/audit_view_export.go` `ExportAudit:113-119` exists as a library with tests, reachable from no console route |
| retention sweep | ClickHouse table TTL (90d) and `nexus_audit_purge_rows()` (`migrations/postgres/020_audit_roles.sql:55`) exist; **no scheduler in application code invokes the latter** |

Partial machinery that does exist and should be reused
rather than rebuilt: PII redaction patterns
(`internal/evaluators/heuristic/patterns.go` `RedactPII:76-83`),
guardrail output redaction
(`internal/guardrails/guardrails.go` `RedactOutput:127-135`),
and the append-only audit log with an `action` column
(`migrations/postgres/001_init.sql:58-66`, written via
`internal/core/store.go:672-685`).

---

## What D-2c.1 delivers

1. This inventory, verified rather than proposed.
2. The §7.1 leak reproduction for the one surface where
   "capture off" was unrepresentable, because nothing was
   consulted. Now
   `internal/gateway/capture_content_coupling_test.go`;
   see "The one unconditional leak" for why it kept its
   assertions and changed its purpose.
3. `internal/observability/capture_boundary_test.go` —
   defence for the boundary that was already correct and
   undefended.

Both suites were verified to fail under mutation: removing
the `InputMessages` assignment fails the reproduction with
the "invert, don't delete" diagnostic, and adding body
attributes to the OTLP envelope fails the boundary test.
They run in the default `go test ./...` CI job, so no
`-tags=capture_negative` gate is needed — the plan's §7.3
tag can be dropped.

D-2c.2 cannot restate §2 from the plan; it must start from
the tables above.

---

## What D-2c.3 delivers

The gate described under "Coupling the plan missed", which
closes the one unconditional leak. Scope was deliberately
limited to that surface: it is the only one that persisted
content with no policy at all, and it is the one the
audited migration schema retains for the full trace
window.

Still open, and each needs its own decision rather than
this flag extended over it:

| Surface | Why it is not covered here |
| ------- | -------------------------- |
| Semantic cache entries | The cached body *is* the feature; gating it disables the cache rather than making it private. Needs a TTL and key-scope decision, not a boolean. |
| `eval_scores.rationale` | Judge rationales can quote the prompt. Blanking them removes the explanation the score exists to provide. |
| Vendor plugin egress | Already opt-in per manifest, and the operator who wrote the manifest chose the vendor. Worth an audit that redaction is reachable, not a gate. |
| Console reads of persisted bodies | With capture off there is nothing to read, so the inspector degrades rather than leaking. Worth a UI affordance explaining why the panel is empty. |

**Release note required.** An existing install that
upgrades and does not set `config.captureTraceContent=true`
stops persisting bodies, so the console trace inspector
shows metadata without request or response text for traces
recorded after the upgrade. Rows already written are
untouched and still age out on the existing TTL. This is
the same shape as the `NEXUS_EVAL_PLUGIN_ONLY` /
`PURGE_LEGACY_PROFILES_ON_BOOT` split: the non-destructive
default changes behaviour going forward and leaves history
alone.
