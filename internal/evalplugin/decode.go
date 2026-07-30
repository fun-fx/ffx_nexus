package evalplugin

import (
	"fmt"

	yaml "go.yaml.in/yaml/v3"
)

// Decode renders a raw YAML byte slice into a Plugin and runs the full
// validator chain. The error is always a typed *Error so callers can
// attach it to a 400 response without losing the YAML line context.
func Decode(raw []byte) (*Plugin, error) {
	var p Plugin
	if err := yamlUnmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode plugin manifest: %w", err)
	}
	if err := Validate(&p); err != nil {
		return nil, fmt.Errorf("decode plugin manifest: %w", err)
	}
	return &p, nil
}

// DecodeMany applies Decode to each item in a multi-document stream.
// Empty documents are skipped; a single broken document fails the
// whole batch (no partial-acceptance).
func DecodeMany(raw []byte) ([]*Plugin, error) {
	docs := splitYAMLDocs(raw)
	out := make([]*Plugin, 0, len(docs))
	for i, doc := range docs {
		if len(doc) == 0 {
			continue
		}
		p, err := Decode(doc)
		if err != nil {
			return nil, fmt.Errorf("plugin #%d: %w", i, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// DecodeStrict behaves like Decode but inspects the raw YAML for
// unknown top-level `spec.<key>` entries and pipes them into
// PluginSpec.UnknownFields. Validate's strict-mode warning path
// then routes those entries to the operator via StrictFieldSink.
//
// We don't set yaml.Decoder.KnownFields(true) because that
// would also reject nested unknowns (e.g. spec.service.metric.args
// keys). The strict semantic we want is "typo at top level".
func DecodeStrict(raw []byte) (*Plugin, error) {
	p, err := Decode(raw)
	if err != nil {
		return nil, err
	}
	knownSpecs := []string{"service", "send", "collect", "timeout", "flags"}
	p.Spec.UnknownFields = unknownTopSpecKeys(raw, knownSpecs)
	// Re-run Validate so the strict-flag hook fires with the
	// freshly-decoded UnknownFields. This is idempotent — the
	// validation rules have already been checked once.
	if hasFlag(p.Spec.Flags, flagStrict) {
		reportUnknownSpecFields(p.Metadata.Name, p.Spec)
	}
	return p, nil
}

// DecodeManyStrict applies DecodeStrict to each document in a
// multi-document stream. Same skip-empty, fail-on-first-broken
// semantics as DecodeMany but emits unknown-fields warnings
// during Validate.
func DecodeManyStrict(raw []byte) ([]*Plugin, error) {
	docs := splitYAMLDocs(raw)
	out := make([]*Plugin, 0, len(docs))
	for i, doc := range docs {
		if len(doc) == 0 {
			continue
		}
		p, err := DecodeStrict(doc)
		if err != nil {
			return nil, fmt.Errorf("plugin #%d: %w", i, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// unknownTopSpecKeys walks the YAML doc tree and returns every
// value key under `spec:` whose key is not in `known`. The walk
// stops at the first spec block — plugins don't ship multi-doc
// manifests, and if they do the loader rejects them upstream so
// we don't need to thread through `---` boundaries.
func unknownTopSpecKeys(raw []byte, known []string) []string {
	knownSet := make(map[string]struct{}, len(known))
	for _, k := range known {
		knownSet[k] = struct{}{}
	}
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return nil
	}
	for _, n := range node.Content {
		if n.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(n.Content); j += 2 {
			if n.Content[j].Value != "spec" {
				continue
			}
			spec := n.Content[j+1]
			if spec.Kind != yaml.MappingNode {
				return nil
			}
			var unknown []string
			for kk := 0; kk+1 < len(spec.Content); kk += 2 {
				fk := spec.Content[kk].Value
				if _, ok := knownSet[fk]; !ok {
					unknown = append(unknown, fk)
				}
			}
			return unknown
		}
	}
	return nil
}
