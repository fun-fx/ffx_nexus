package evalplugin

import (
	yaml "go.yaml.in/yaml/v3"
)

// yamlUnmarshalImpl is the single place that imports go.yaml.in. By
// isolating the third-party dep behind one symbol we can swap libraries
// later (e.g. sigs.k8s.io/yaml) without touching every caller.
func yamlUnmarshalImpl(raw []byte, dst any) error { return yaml.Unmarshal(raw, dst) }
