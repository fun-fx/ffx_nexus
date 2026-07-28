package evalplugin

import "bytes"

// yamlUnmarshalAndSplit decodes a YAML byte stream (possibly
// containing multiple `---`-separated documents) into one Plugin. It
// also returns the unused remainder so DecodeMany can walk through
// multi-document manifests without copying the input.
func yamlUnmarshal(raw []byte, dst any) error { return yamlUnmarshalImpl(raw, dst) }

// splitYAMLDocs scans raw YAML for `---` document separators and
// returns each non-empty byte slice. Cluster-wide ConfigMaps bundle
// multiple plugin manifests behind one Helm key.
func splitYAMLDocs(raw []byte) [][]byte {
	out := bytes.Split(raw, []byte("\n---"))
	cleaned := make([][]byte, 0, len(out))
	for _, doc := range out {
		trimmed := bytes.TrimSpace(doc)
		if len(trimmed) == 0 {
			continue
		}
		cleaned = append(cleaned, trimmed)
	}
	return cleaned
}
