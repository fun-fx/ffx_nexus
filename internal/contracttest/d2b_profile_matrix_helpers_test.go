// Package contracttest — D-2b test helpers for the
// profile-matrix scenarios. Keeps the call sites in the
// scenario tests short and the shell-out logic in one
// place.
package contracttest

import (
	"os/exec"
	"testing"
)

// renderNP shells out to `helm template` for a slim,
// NP-only render. Returns the rendered docs for the
// networkpolicy.yaml template as a single string.
func renderNP(chart string, args []string) string {
	preArgs := []string{"template", "render-test", chart, "--show-only", "templates/networkpolicy.yaml"}
	all := append(preArgs, args...)
	cmd := exec.Command("helm", all...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// The matrix tests want a non-error path so we
		// never want them to hit helm fail. If they do,
		// failure is clear because the test body asserts
		// a specific shape on the text.
		return ""
	}
	return string(out)
}

// renderNPError returns true when `helm template` with the
// given args produced a non-zero exit status. Used by
// profile-matrix fail-closed tests.
func renderNPError(chart string, args []string) bool {
	preArgs := []string{"template", "render-test", chart, "--show-only", "templates/networkpolicy.yaml"}
	all := append(preArgs, args...)
	cmd := exec.Command("helm", all...)
	if err := cmd.Run(); err != nil {
		return true
	}
	return false
}

// renderTopTwo returns the deployment + networkpolicy
// combined render. Future tests that need a
// deployment-tier click-to-debug UI can ask for it.
func renderTopTwo(t *testing.T, chart string, args []string) string {
	t.Helper()
	preArgs := []string{"template", "render-test", chart}
	all := append(preArgs, args...)
	cmd := exec.Command("helm", all...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, string(out))
	}
	return string(out)
}
