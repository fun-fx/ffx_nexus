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
	"on_trace": {},
	"scheduled": {},
	"manual":    {},
}

// validCollectMode enumerates the legal Collect.Mode values. Sync is
// only legal when the upstream service actually returns a JSON body
// inline (helicones/v1request-score shape) — the adapter enforces the
// constraint at dispatch time.
var validCollectMode = map[string]struct{}{
	"sync":    {},
	"webhook": {},
	"poll":    {},
}

var validRedact = map[string]struct{}{
	"pii": {},
}

// Validate enforces the v1alpha1 schema. The error set is stable
// (string match) so failing tests can pin against it.
//
// Validation order matters: cheap checks first, expensive JSONPath
// checks last. We do not JSONPath-parse mapping expressions here —
// adapters decode them lazily — to avoid pulling an extra dependency
// into the hot path.
func Validate(p *Plugin) error {
	if p == nil {
		return errors.New("plugin is nil")
	}
	if p.APIVersion != PluginAPIVersion {
		return fmt.Errorf("apiVersion must be %q (got %q)", PluginAPIVersion, p.APIVersion)
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
	if strings.TrimSpace(p.Spec.Service.Endpoint) == "" {
		return errors.New("spec.service.endpoint is required")
	}
	if err := validateAuth(&p.Spec.Service.Auth); err != nil {
		return err
	}

	if _, ok := validTrigger[p.Spec.Send.Trigger]; !ok {
		return fmt.Errorf("spec.send.trigger %q is not supported", p.Spec.Send.Trigger)
	}
	if p.Spec.Send.Sampling < 0 || p.Spec.Send.Sampling > 1 {
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
	if p.Spec.Collect.Mode == "poll" && p.Spec.Collect.Interval <= 0 {
		return errors.New("spec.collect.interval is required when mode=poll")
	}
	return nil
}

func validateAuth(a *AuthSpec) error {
	if a.InlineKey != "" {
		return errors.New("spec.service.auth.inlineKey is forbidden; use secretRef or keyRef")
	}
	hasSecret := strings.TrimSpace(a.SecretRef) != ""
	hasKeyRef := strings.TrimSpace(a.KeyRef) != ""
	if !hasSecret && !hasKeyRef {
		return errors.New("spec.service.auth requires either secretRef or keyRef")
	}
	return nil
}
