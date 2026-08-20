// Helpers for the integrationcni package.
// Provides splitYAMLDocs / yamlUnmarshal /
// execCmd. They duplicate a few symbols from
// the upstream d2b_cni_scenario_test.go file
// because Go test packages are per-package and
// the helpers are small.
//
//go:build integrationcni

package integrationcni

import (
	"os/exec"
	"strings"

	"sigs.k8s.io/yaml"
)

// splitYAMLDocs splits a multi-doc YAML stream
// into chunks separated by the `^---$` line.
func splitYAMLDocs(s string) []string {
	out := []string{}
	cur := strings.Builder{}
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "---") {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteString(line)
		cur.WriteString("\n")
	}
	out = append(out, cur.String())
	return out
}

// yamlUnmarshal wraps sigs.k8s.io/yaml.Unmarshal
// so callers don't have to import the package.
func yamlUnmarshal(b []byte, v interface{}) error {
	return yaml.Unmarshal(b, v)
}

// execCmd constructs an exec.Cmd from an arg
// slice. Some callers like kubeExec build a list
// with a runtime variable prefix; this helper
// centralises the construction.
func execCmd(args ...string) *exec.Cmd {
	if len(args) == 0 {
		return exec.Command("")
	}
	return exec.Command(args[0], args[1:]...)
}

// runHelm renders the chart. args is the helm
// args after `helm`. The leading "helm" is
// added here so callers don't have to sprinkle
// the binary name across the suite.
func runHelm(args ...string) (string, error) {
	all := append([]string{"helm"}, args...)
	c := execCmd(all...)
	out, err := c.CombinedOutput()
	return string(out), err
}
