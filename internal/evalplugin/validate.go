package evalplugin

import (
	"errors"
	"fmt"
	"strings"
)

// pluginKindExpected is a fixed string so mistyped "Kind" values in a
// user manifest fail fast. We don't allow a registry here on purpose
// — EvalPlugin is the only legal kind. Anything else means the YAML
// was authored for a different system.
const pluginKindExpected = "EvalPlugin"

// validTrigger enumerates the legal Send.Trigger values. The
// dispatcher implements on_trace today; scheduled/manual are reserved
// for follow-up work so users can write YAML now without breakage.
var validTrigger = map[string]struct{}{
	"on_trace":  {},
	"scheduled": {},
	"manual":    {},
}

// validCollectMode enumerates the legal Collect.Mode values.
//
// Sync mode was retired: it described an inline response from the
// vendor's POST. No first-class vendor on the supported set returns
// eval scores inline — LangSmith, Langfuse, Datadog, Braintrust,
// Arize Phoenix, Confident AI, OTLP collectors, and webhooks all
// need either a polling reverse channel or a webhook push. The
// "inline" ingest path is the OTLP-native `gen_ai.evaluation.result`
// event emit (Add-C), which is platform-side and does not depend on
// `collect.mode: sync`. Authors who previously wrote `mode: sync`
// should switch to `mode: webhook` and configure the vendor's
// webhook push — see docs/eval-plugins.md.
var validCollectMode = map[string]struct{}{
	"webhook": {},
	"poll":    {},
}

var validRedact = map[string]struct{}{
	"pii": {},
}

// validMetric enumerates the legal MetricSpec.Name values for a
// ServiceHeuristic plugin. The split between Go-native metric
// implementations (contains/pii/exact_match/rouge_l) and the Python
// subprocess (hf_evaluate/lighteval/ragas) decisions happen during
// dispatch — only the names matter here.
var validMetric = map[string]struct{}{
	"contains":     {},
	"pii":          {},
	"exact_match":  {},
	"rouge_l":      {},
	"hf_evaluate":  {},
	"lighteval":    {},
	"ragas":        {},
}

// validFlag enumerates the strings that may appear in spec.flags.
// Strict today; future opt-ins should append (and not repurpose).
var validFlag = map[string]struct{}{
	"strict": {},
}

// validTransport enumerates the legal spec.collect.transport values
// for ServiceCollector and (for backwards compat) ServiceOTel /
// ServiceWebhook. "" is treated as "json" by the dispatcher.
var validTransport = map[string]struct{}{
	"":        {},
	"json":    {},
	"otlp_http": {},
}

// knownAPIVersions is the lookup for the apiVersion check below.
// Closed keep it that way until v1alpha1 and v1alpha2 are both
// still supported; once v1alpha1 retires, change Validate to fail
// rather than warn on it.
var knownAPIVersions = map[string]struct{}{
	PluginAPIVersion:      {}, // v1alpha1 — original schema
	PluginAPIVersionV1Alpha2: {}, // v1alpha2 — heu, conf, etc.
}

// Validate enforces the schema. Supports v1alpha1 and v1alpha2.
//
// API version handling
//
//   - empty:        fail (helps authoring — operators see "apiVersion
//                   required" instead of "service.type unsupported").
//   - v1alpha1:     recognised, validated against the v1alpha1 schema.
//                   Heuristic metric kinds are NOT permitted.
//   - v1alpha2:     recognised, validated against the v1alpha2 schema.
//                   v1alpha1 fields continue to work; new fields
//                   are interpretation-explained.
//   - anything else: fail.
//
// Validation order matters: cheap checks first, expensive mapping
// checks last. We do not parse mapping expressions here — adapters
// decode them lazily — to avoid pulling an extra dependency
// into the hot path.
func Validate(p *Plugin) error {
	if p == nil {
		return errors.New("plugin is nil")
	}
	if strings.TrimSpace(p.APIVersion) == "" {
		return errors.New("apiVersion is required (use nexus.io/v1alpha1 or nexus.io/v1alpha2)")
	}
	if _, ok := knownAPIVersions[p.APIVersion]; !ok {
		return fmt.Errorf("apiVersion %q is not supported (use %s or %s)",
			p.APIVersion, PluginAPIVersion, PluginAPIVersionV1Alpha2)
	}
	if p.Kind != pluginKindExpected {
		return fmt.Errorf("kind must be %q (got %q)", pluginKindExpected, p.Kind)
	}
	if strings.TrimSpace(p.Metadata.Name) == "" {
		return errors.New("metadata.name is required")
	}
	if _, reserved := reservedPluginNames[p.Metadata.Name]; reserved {
		return fmt.Errorf("metadata.name %q is reserved", p.Metadata.Name)
	}
	if !validPluginName.MatchString(p.Metadata.Name) {
		return fmt.Errorf("metadata.name %q does not match %s", p.Metadata.Name, validPluginName)
	}

	if _, ok := validServiceType[p.Spec.Service.Type]; !ok {
		return fmt.Errorf("spec.service.type %q is not a supported type", p.Spec.Service.Type)
	}
	// Heuristic-mode plugins do not require an endpoint (the metric
	// runs in-process). All other types do.
	if p.Spec.Service.Type != ServiceHeuristic {
		if strings.TrimSpace(p.Spec.Service.Endpoint) == "" {
			return errors.New("spec.service.endpoint is required")
		}
	}
	if p.Spec.Service.Type == ServiceHeuristic {
		if err := validateMetric(&p.Spec.Service.Metric, p.APIVersion); err != nil {
			return err
		}
	}
	if p.Spec.Service.Type == ServiceCollector {
		if _, ok := validTransport[p.Spec.Collect.Transport]; !ok {
			return fmt.Errorf("spec.collect.transport %q is not supported (use \"json\" or \"otlp_http\")",
				p.Spec.Collect.Transport)
		}
	}
	if err := validateAuth(&p.Spec.Service.Auth); err != nil {
		// Heuristic plugins don't talk to a vendor; allow the auth
		// block to be empty. Other ServiceTypes still fail fast.
		if p.Spec.Service.Type != ServiceHeuristic {
			return err
		}
	}

	if _, ok := validTrigger[p.Spec.Send.Trigger]; !ok {
		return fmt.Errorf("spec.send.trigger %q is not supported", p.Spec.Send.Trigger)
	}
	if v := float64(p.Spec.Send.Sampling); v < 0 || v > 1 {
		return errors.New("spec.send.sampling must be in [0,1]")
	}
	for _, r := range p.Spec.Send.Redact {
		if _, ok := validRedact[r]; !ok {
			return fmt.Errorf("spec.send.redact entry %q is not supported (only \"pii\" today)", r)
		}
	}

	if _, ok := validCollectMode[p.Spec.Collect.Mode]; !ok {
		return fmt.Errorf("spec.collect.mode %q is not supported", p.Spec.Collect.Mode)
	}
	if p.Spec.Collect.Mode == "poll" && p.Spec.Collect.Interval.Std() <= 0 {
		return errors.New("spec.collect.interval is required when mode=poll")
	}

	// v1alpha1 manifests may NOT use the new ServiceHeuristic / Metric
	// fields. This guards operators from authoring what looks like a
	// working manifest but is silently ignored.
	if p.APIVersion != PluginAPIVersionV1Alpha2 {
		if p.Spec.Service.Type == ServiceHeuristic ||
			p.Spec.Service.Type == ServiceConfidentAI ||
			p.Spec.Service.Type == ServiceArizePhoenix ||
			p.Spec.Service.Type == ServiceCollector {
			return fmt.Errorf("spec.service.type %q requires apiVersion %s",
				p.Spec.Service.Type, PluginAPIVersionV1Alpha2)
		}
	}
	// Flag validation. Future flags should append to `validFlag` and
	// also apply their check here.
	for _, f := range p.Spec.Flags {
		if _, ok := validFlag[f]; !ok {
			return fmt.Errorf("spec.flags entry %q is not supported", f)
		}
	}
	return nil
}

// validateMetric enforces the MetricSpec contents. Strict on Name so
// a typo can't silently select the wrong metric implementation.
func validateMetric(m *MetricSpec, apiVersion string) error {
	if m == nil {
		return errors.New("spec.service.metric is required when service.type=heuristic")
	}
	if apiVersion != PluginAPIVersionV1Alpha2 {
		return errors.New("spec.service.metric is v1alpha2 only; set apiVersion accordingly")
	}
	if _, ok := validMetric[m.Name]; !ok {
		return fmt.Errorf("spec.service.metric.name %q is not supported", m.Name)
	}
	return nil
}

func validateAuth(a *AuthSpec) error {
	if a.InlineKey != "" {
		return errors.New("spec.service.auth.inlineKey is forbidden; use secretRef or keyRef")
	}
	hasSecret := strings.TrimSpace(a.SecretRef) != ""
	hasKeyRef := strings.TrimSpace(a.KeyRef) != ""
	// Heuristic plugins don't talk to a vendor; an absent auth is the
	// expected shape. All other ServiceTypes either accept an auth
	// block or reject.
	if !hasSecret && !hasKeyRef {
		// Allow-but-do-not-require is checked at the call site per
		// ServiceType. We default to requiring it here so every other
		// type still fails fast on the v1alpha1 path.
		return errors.New("spec.service.auth requires either secretRef or keyRef")
	}
	return nil
}
