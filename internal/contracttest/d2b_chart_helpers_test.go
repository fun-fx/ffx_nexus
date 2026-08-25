// Package contracttest — D-2b chart inventory helpers.
//
// Tiny helpers reused from the d2b_chart_inventory_test.go
// suite and shared with the existing d2b_*.go files.
package contracttest

import (
	"os/exec"
	"strings"
)

func renderTop(t testingT, chart string, args []string) string {
	t.Helper()
	preArgs := []string{
		"template", "render-test", chart,
		"--show-only", "templates/networkpolicy.yaml",
	}
	all := append(preArgs, args...)
	cmd := exec.Command("helm", all...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, string(out))
	}
	return string(out)
}

// renderShowOnly renders a single template file. Used by the
// port × peer inventory test which needs to compare five
// manifests side by side.
func renderShowOnly(t testingT, chart string, showOnly string, args []string) string {
	t.Helper()
	preArgs := []string{"template", "render-test", chart, "--show-only", showOnly}
	all := append(preArgs, args...)
	cmd := exec.Command("helm", all...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template --show-only %s: %v\n%s", showOnly, err, string(out))
	}
	return string(out)
}

func countNP(s string) int {
	return strings.Count(s, "kind: NetworkPolicy")
}

// testingT is the minimal interface renderTop needs so that
// either *testing.T or a *bevah fixture can satisfy it. We
// avoid importing testing in shared helper files because that
// pulls the full test framework into non-test code.
type testingT interface {
	Helper()
	Fatalf(format string, args ...interface{})
}
