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
// datadog, braintrust, arize, otel, webhook); only power users need to
// drop down to vendor-specific JSONPath mapping in `spec.collect`.
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
const PluginAPIVersion = "nexus.io/v1alpha1"

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
	ServiceLangSmith  ServiceType = "langsmith"
	ServiceLangfuse   ServiceType = "langfuse"
	ServiceDatadog    ServiceType = "datadog"
	ServiceBraintrust ServiceType = "braintrust"
	ServiceArize      ServiceType = "arize"
	ServiceOTel       ServiceType = "otel"
	ServiceWebhook    ServiceType = "webhook"
)

// validServiceType is the lookup the validator uses. It is the source
// of truth — do not introduce a ServiceType constant without appending
// it here.
var validServiceType = map[ServiceType]struct{}{
	ServiceLangSmith:  {},
	ServiceLangfuse:   {},
	ServiceDatadog:    {},
	ServiceBraintrust: {},
	ServiceArize:      {},
	ServiceOTel:       {},
	ServiceWebhook:    {},
}

// ResultMapping is a single JSONPath → canonical field translation.
// All translations land in the OTel-aligned shape defined by
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
type CollectSpec struct {
	Mode     string        `yaml:"mode" json:"mode"`
	Interval Duration      `yaml:"interval" json:"interval"`
	Mapping  ResultMapping `yaml:"mapping" json:"mapping"`
}

// ServiceSpec is the connection block. Auth is referenced, never
// embedded — the Resolver performs the lookup per the existing
// evals.SecretResolver contract (org/user/inline/builtin).
type ServiceSpec struct {
	Type     ServiceType `yaml:"type" json:"type"`
	Endpoint string      `yaml:"endpoint" json:"endpoint"`
	Auth     AuthSpec    `yaml:"auth" json:"auth"`
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
type PluginSpec struct {
	Service ServiceSpec `yaml:"service" json:"service"`
	Send    SendSpec    `yaml:"send" json:"send"`
	Collect CollectSpec `yaml:"collect" json:"collect"`
	Timeout Duration    `yaml:"timeout" json:"timeout"`
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
