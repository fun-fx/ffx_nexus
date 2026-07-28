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
the plugin contract.

## Quickstart — install the LangSmith plugin in 5 minutes

1. Create a LangSmith API key in your LangChain account.
2. Store it in K8s:

   ```bash
   kubectl create secret generic langsmith-api-key \
     --from-literal=key=$LANGSMITH_API_KEY
   ```

3. Apply the bundled ConfigMap:

   ```bash
   kubectl apply -f deploy/helm/nexus/templates/eval-plugins/langsmith-judge.yaml
   ```

4. In the Nexus console → **Admin → Eval → Plugins**, enable the
   `langsmith-judge` row.

Traces sampled at 10% (= `spec.send.sampling`) are forwarded to
LangSmith; evaluation results come back via webhook (`mode: webhook`)
or 60s polling and are persisted to `eval_scores`.

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
    mode: webhook                 # sync | webhook | poll
    interval: 60s                 # only when mode=poll
    mapping:                      # JSONPath → OTel-aligned attrs
      name:        "$.key"
      score:       "$.score"
      label:       "$.value"
      explanation: "$.comment"
      trace_id:    "$.trace_id"
      metric:      "$.metric"
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
| `spec.service.auth.secretRef` | one of two | Reference to K8s Secret or `eval_credentials` key ref.        |
| `spec.service.auth.inlineKey` | never    | Forbidden; secrets never appear in source-controlled YAML.     |
| `spec.send.trigger`        | yes       | `on_trace` today. `scheduled`/`manual` are reserved.            |
| `spec.send.sampling`       | yes       | `[0, 1]`. Use ≤ 0.1 by default to keep egress bounded.          |
| `spec.send.payload`        | yes       | Map of strings; each value is a Go-text/template.               |
| `spec.send.redact`         | no        | Only `pii` is accepted today; runs existing heuristic, replaces hits with `[REDACTED:<kind>]`. |
| `spec.collect.mode`        | yes       | `webhook` recommended for LangSmith/Langfuse/Datadog.           |
| `spec.collect.interval`    | when poll | Polling cadence when webhook is unavailable.                    |
| `spec.collect.mapping`     | yes       | JSONPath map. Adapters provide defaults; you only override when the vendor uses nonstandard keys. |
| `spec.timeout`             | no        | Default 30s.                                                    |

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

1. **Cluster-wide** (this ConfigMap approach). Admin installs once;
   every org sees the plugin.
2. **Per-org** (subject for Phase B). Admin REST endpoint
   `POST /api/eval/plugins` records a plugin in `eval_plugins` and
   the loader absorbs both sources. When the same `metadata.name`
   appears in both, **Helm (cluster-wide) wins** so admins can
   guarantee a baseline.

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

## Adapters currently shipped

| `service.type` | Adapter status | Notes |
|-----------------|----------------|-------|
| `langsmith`     | Stable (v0.7)  | First reference plugin. |
| `langfuse`      | Stable (v0.7.1)| OSS / self-host friendly; recommended default for self-hosted customers. |
| `datadog`       | Beta           | Hex→decimal trace_id rewrite required (see adapter docs). |
| `braintrust`    | Beta           | OTLP traces accepted. |
| `arize`         | Beta           | AX remote-evaluator endpoints. |
| `otel`          | Beta           | Direct OTLP `gen_ai.evaluation.result` to an OTel collector. |
| `webhook`       | Stable         | Generic JSON forwarder to the admin-defined URL. |

DeepEval/Confident AI, WhyLabs, RagaAI are **not** supported as
drop-in adapters (their SDKs are Python-only and don't expose a
generic "attach a score to a trace id" HTTP endpoint).
