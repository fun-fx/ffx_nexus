// Package evalplugin declares the schema for external eval "plugins" —
// declarative deployments that turn a remote evaluation service (such
// as LangSmith, Langfuse, Datadog LLM Observability, Arize Phoenix, or
// a user's own HTTPS endpoint) into one of Nexus's eval evaluator
// kinds.
//
// Plugins are YAML manifests rendered either into a Helm ConfigMap
// (cluster-wide defaults) or persisted per-org through the admin REST
// API. At application boot the loader merges both sources into a
// single in-memory registry keyed by `metadata.name`.
//
// The schema is intentionally narrow. Most production deployments will
// use the closed `spec.service.type` enum (langsmith, langfuse,
// datadog, braintrust, arize, otel_collector); only power users need to
// drop down to vendor-specific flat-key mapping in `spec.collect`.
package evalplugin

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

// PluginAPIVersion is the schema version manifest authors target. Today
// only `v1alpha1` is recognised; an unknown value fails Validate().
//
//   - v1alpha1 is the original schema.
//   - v1alpha2 adds support for:
//
//   1. ServiceType "heuristic" — in-process metric evaluators that
//      score the trace locally instead of dispatching to an external
//      service (Add-A).
//   2. ServiceType "confident_ai" — DeepEval-native cloud target
//      (add-confidentai-phoenix).
//   3. ServiceType "arize_phoenix" — OTel-only target with no API key.
//   4. spec.collect.transport — replaces the otel/webhook types with a
//      single OTLP collector adapter that ships JSON or protobuf
//      payloads (collapse-otel-webhook).
//   5. spec.service.metric — a typed metric kind for heuristic-mode
//      plugins (contains, pii, exact_match, rouge_l, hf_evaluate,
//      lighteval, ragas).
//
//   Manifests carrying `apiVersion: nexus.io/v1alpha1` continue to be
//   accepted; loadV1alpha2 warnings surface in the boot logs so the
//   operator sees the new fields are usable.
const PluginAPIVersion = "nexus.io/v1alpha1"

// PluginAPIVersionV1Alpha2 is the schema version that introduces the
// new kinds listed above. Manifests declaring it gain everything in
// v1alpha1 plus the additions; a future v1beta will retire
// v1alpha1 entirely.
const PluginAPIVersionV1Alpha2 = "nexus.io/v1alpha2"

// PluginKind is the constant discriminator for EvalPlugin manifests.
const PluginKind = "EvalPlugin"

// ServiceType enumerates the supported external targets. Adding a new
// adapter means: 1) extend this enum, 2) register an Adapter in
// adapters/<vendor>.go with the corresponding encode/decode hooks, 3)
// update docs/eval-plugins.md.
//
// The closed enum protects users from typos (e.g. "langsmit") that
// would otherwise silently fail at send time.
type ServiceType string

const (
	ServiceLangSmith      ServiceType = "langsmith"
	ServiceLangfuse       ServiceType = "langfuse"
	ServiceDatadog        ServiceType = "datadog"
	ServiceBraintrust     ServiceType = "braintrust"
	ServiceArize          ServiceType = "arize"
	// ServiceOTel / ServiceWebhook are the v1alpha1 OTLP / generic
	// collectors. They continue to work in v1alpha2 manifests; new
	// code that targets v1alpha2 should use ServiceCollector with
	// the transport field set instead.
	ServiceOTel       ServiceType = "otel"
	ServiceWebhook    ServiceType = "webhook"
	// ServiceHeuristic is the in-process metric kind. Manifests with
	// this type declare `spec.service.metric` to pick the metric
	// implementation (contains, pii, exact_match, rouge_l) or
	// delegate to the Python subprocess (hf_evaluate, lighteval,
	// ragas) for the rest.
	ServiceHeuristic ServiceType = "heuristic"
	// ServiceConfidentAI is Confident AI's DeepEval-native cloud
	// target (a.k.a. DeepEval Cloud). Speaks OTLP/JSON with an
	// optional x-confident-api-key header. Self-hosting is not the
	// goal — the adapter assumes the cloud endpoint.
	ServiceConfidentAI ServiceType = "confident_ai"
	// ServiceArizePhoenix sends traces to Arize Phoenix via OTLP.
	// Optional Basic auth (user/pass) is read through the AuthSpec.
	// langfuse-judge users that want a self-hosted OTLP target can
	// point ServiceCollector at the same URL; the difference is
	// that this ServiceType has apply-on-collect filtering of
	// non-LLM spans out of the box.
	ServiceArizePhoenix ServiceType = "arize_phoenix"
	// ServiceCollector is the v1alpha2 "single OTLP collector"
	// adapter that replaces ServiceOTel and ServiceWebhook. Pick
	// the wire shape with Spec.Collect.Transport ("json" or
	// "otlp_http"); the auth block is unchanged.
	ServiceCollector ServiceType = "otel_collector"
)

// validServiceType is the lookup the validator uses. It is the source
// of truth — do not introduce a ServiceType constant without appending
// it here.
var validServiceType = map[ServiceType]struct{}{
	ServiceLangSmith:      {},
	ServiceLangfuse:       {},
	ServiceDatadog:        {},
	ServiceBraintrust:     {},
	ServiceArize:          {},
	ServiceOTel:           {},
	ServiceWebhook:        {},
	ServiceHeuristic:      {},
	ServiceConfidentAI:    {},
	ServiceArizePhoenix:   {},
	ServiceCollector:      {},
}

// ResultMapping is a flat-key → canonical field translation.
// Authors give the *source key* on the vendor's wire format (e.g.
// "comment", "traceId", "value"); the dispatcher looks up each key
// in the parsed JSON object directly. JSONPath syntax (`$.key`)
// was retired: the plan's "JSONPath-shaped pickString with
// truthful flat-lookup semantics" closeout landed here. All
// translations land in the OTel-aligned shape defined by
// `gen_ai.evaluation.result` (name, score.value, score.label,
// explanation) regardless of the upstream vendor's wording. Fields are
// optional — adapters provide defaults if a mapping is absent.
type ResultMapping struct {
	Name        string `yaml:"name" json:"name"`
	Score       string `yaml:"score" json:"score"`
	Label       string `yaml:"label" json:"label"`
	Explanation string `yaml:"explanation" json:"explanation"`
	TraceID     string `yaml:"trace_id" json:"trace_id"`
	Metric      string `yaml:"metric" json:"metric"`
}

// SendSpec describes how each trace is forwarded. Sampling is
// probability so that the dispatcher can roll dice once per trace
// without inspecting the trace payload twice (privacy). Sampling is a
// 0–1 fraction; the tolerant UnmarshalYAML on SamplingFraction also
// accepts percent strings and bare integers for convenience.
type SendSpec struct {
	Trigger  string            `yaml:"trigger" json:"trigger"`
	Sampling SamplingFraction  `yaml:"sampling" json:"sampling"`
	Payload  map[string]string `yaml:"payload" json:"payload"`
	Redact   []string          `yaml:"redact" json:"redact"`
}

// CollectSpec describes how results come back. The three modes trade
// trivial deployment (sync) for symmetry with vendor APIs (webhook is
// the recommended default; poll is the fallback when the platform
// doesn't support webhooks). All durations are YAML-tolerant — quoted
// Go strings (`60s`) and bare numbers (`60` → seconds) both work.
//
// Transport is the wire selector for the v1alpha2 ServiceCollector
// type: `transport: json` posts a JSON document, `transport: otlp_http`
// posts a binary-encoded OTLP request body. Both target the same URL
// (the manifest's spec.service.endpoint) — only the encoding differs.
// The adapter collapses ServiceOTel + ServiceWebhook into this single
// receiver.
type CollectSpec struct {
	Mode      string        `yaml:"mode" json:"mode"`
	Transport string        `yaml:"transport,omitempty" json:"transport,omitempty"`
	Interval  Duration      `yaml:"interval" json:"interval"`
	Mapping   ResultMapping `yaml:"mapping" json:"mapping"`
}

// MetricSpec is the heuristic-mode metric descriptor. Manifests of
// ServiceHeuristic declare exactly one metric (Name); Args is an open
// dict passed verbatim to the metric implementation (Python or Go).
// The set of legal Names is closed (see ValidMetricNames) so a typo
// fails fast at Validate time rather than silently scoring nothing.
type MetricSpec struct {
	Name string         `yaml:"name" json:"name"`
	Args map[string]any `yaml:"args,omitempty" json:"args,omitempty"`
}

// ServiceSpec is the connection block. Auth is referenced, never
// embedded — the Resolver performs the lookup per the existing
// evals.SecretResolver contract (org/user/inline/builtin).
//
// Metric is used only when Type == ServiceHeuristic. It is otherwise
// ignored by webhooks / OTLP / REST adapters.
type ServiceSpec struct {
	Type     ServiceType `yaml:"type" json:"type"`
	Endpoint string      `yaml:"endpoint" json:"endpoint"`
	Auth     AuthSpec    `yaml:"auth" json:"auth"`
	Metric   MetricSpec  `yaml:"metric,omitempty" json:"metric,omitempty"`
}

// AuthSpec carries only references. The secret itself lives in K8s
// Secrets or in the in-cluster eval_credentials store. A plugin with a
// non-empty Auth.InlineKey is rejected by Validate so the secret never
// appears in source-controlled YAML.
type AuthSpec struct {
	SecretRef string `yaml:"secretRef" json:"secretRef"`
	KeyRef    string `yaml:"keyRef" json:"keyRef"`
	InlineKey string `yaml:"inlineKey,omitempty" json:"inlineKey,omitempty"`
}

// PluginMetadata identifies a plugin. Only `name` is required; the
// labels are surfaced in logs and dashboards but do not affect routing.
type PluginMetadata struct {
	Name   string            `yaml:"name" json:"name"`
	Labels map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
}

// PluginSpec is the body of an EvalPlugin manifest. It is what
// user-authored YAML unmarshals into.
//
// Flags is the v1alpha2 escape hatch for deploy-time switches:
//
//   - "strict": reject unknown spec keys at Validate time. The default
//     is to ignore unknown keys so a forward-port of v1alpha1 keeps
//     loading under v1alpha2. Set strict to surface typos like
//     `trigger: onTraces` early.
//
// Future flags live in this slice to avoid a YAML schema migration
// every time we add an opt-in behaviour.
type PluginSpec struct {
	Service ServiceSpec `yaml:"service" json:"service"`
	Send    SendSpec    `yaml:"send" json:"send"`
	Collect CollectSpec `yaml:"collect" json:"collect"`
	Timeout Duration    `yaml:"timeout" json:"timeout"`
	Flags   []string    `yaml:"flags,omitempty" json:"flags,omitempty"`
}

// Plugin is a fully-decoded EvalPlugin manifest. Use Decode to obtain
// one from raw YAML.
type Plugin struct {
	APIVersion string         `yaml:"apiVersion" json:"apiVersion"`
	Kind       string         `yaml:"kind" json:"kind"`
	Metadata   PluginMetadata `yaml:"metadata" json:"metadata"`
	Spec       PluginSpec     `yaml:"spec" json:"spec"`
}

// reservedPluginNames shadows "kube-system"-style protected
// identifiers. The validator enforces the union of the set below.
var reservedPluginNames = map[string]struct{}{
	"nexus":     {},
	"system":    {},
	"plugin":    {},
	"default":   {},
	"builtin":   {},
	"heuristic": {},
	"external":  {},
}

// validPluginName matches the DNS-style identifier we surface into
// metric labels and selector identifiers so a misconfigured manifest
// fails fast at startup rather than producing garbage dashboards.
var validPluginName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}[a-z0-9]$`)

// SamplingFraction is a YAML-tolerant 0–1 probability. Manifests author
// it as `sampling: 0.1`, `sampling: "0.1"`, `sampling: "15%"`, or even
// `sampling: 15` (interpreted as a percentage). Go's yaml v3 is strict
// about float→string mismatches, so we implement UnmarshalYAML so the
// template defaults authored as quoted strings still decode.
type SamplingFraction float64

func (s *SamplingFraction) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	var raw float64
	switch node.Kind {
	case yaml.ScalarNode:
		text := strings.TrimSpace(node.Value)
		if text == "" {
			return nil
		}
		if strings.HasSuffix(text, "%") {
			pct, err := strconv.ParseFloat(strings.TrimSuffix(text, "%"), 64)
			if err != nil {
				return fmt.Errorf("sampling: invalid percent %q: %w", node.Value, err)
			}
			raw = pct / 100.0
		} else if n, err := strconv.ParseFloat(text, 64); err == nil && !strings.ContainsAny(text, "eE") {
			// float64 catch — including "0.1", "1.0", "100" (which
			// ParseFloat also accepts as 100).
			raw = n
		} else if dur, err := time.ParseDuration(text); err == nil {
			raw = dur.Seconds()
		} else {
			return fmt.Errorf("sampling: cannot parse %q as float, percent, or duration", node.Value)
		}
	default:
		return fmt.Errorf("sampling: unsupported yaml node kind %d", node.Kind)
	}
	*s = SamplingFraction(raw)
	return nil
}

func (s SamplingFraction) MarshalYAML() (any, error) {
	return float64(s), nil
}

// Duration is a YAML-tolerant time.Duration. Helm-rendered manifests
// commonly emit durations as either bare numbers (e.g. `interval:
// 60` → "60 seconds") or quoted Go-style strings (`interval: 60s`).
// time.Duration.UnmarshalText only accepts the latter, so we accept
// both forms and emit a clear error otherwise.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	var out time.Duration
	switch node.Kind {
	case yaml.ScalarNode:
		text := strings.TrimSpace(node.Value)
		if text == "" {
			return nil
		}
		if dur, err := time.ParseDuration(text); err == nil {
			out = dur
			*d = Duration(out)
			return nil
		}
		// Fall back to "bare number = seconds" so Helm's `.Files.Get`
		// style interpolation, which always produces strings, still
		// resolves.
		if n, err := strconv.ParseFloat(text, 64); err == nil {
			out = time.Duration(n * float64(time.Second))
			*d = Duration(out)
			return nil
		}
		return fmt.Errorf("duration: cannot parse %q", node.Value)
	default:
		return fmt.Errorf("duration: unsupported yaml node kind %d", node.Kind)
	}
}

func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }
