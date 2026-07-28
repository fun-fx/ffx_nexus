package evalplugin

import (
	"fmt"
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
