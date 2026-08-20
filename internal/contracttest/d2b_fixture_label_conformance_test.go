// D-2b.0.2 fixture-label conformance catcher.
//
// The chart's networkpolicy.yaml classifies
// gateway / worker / migration Pods using a
// selector derived from include "nexus.fullname" .
// — which we pin to the value the fixture uses
// (`--set fullnameOverride=<releaseName>`).
//
// This test fails-fast if the fixture Pods drift
// from the chart's selector. Without this test
// the heavy CNI integration gate could pass with
// every "DENY_OK" actually being a "deny for the
// wrong reason" because `podSelector.matchLabels`
// would match no Pod at all.
//
//go:build !integrationcni

package contracttest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// executeHelmTemplate renders networkpolicy.yaml
// for the given release; the rendered selector
// reference is what fixture Pods must match.
func executeHelmTemplate(t *testing.T, releaseName string) string {
	t.Helper()
	chart := filepath.Join(moduleRootOrFatal(t), "deploy", "helm", "nexus")
	out, err := exec.Command("helm", "template", releaseName, chart,
		"--set", "networkPolicy.profile=enterprise",
		"--set", "networkPolicy.enforcementAcknowledged=true",
		"--set", "fullnameOverride="+releaseName,
		"--show-only", "templates/networkpolicy.yaml",
	).CombinedOutput()
	if err != nil {
		// We deliberately do NOT skip on helm
		// failures. Helm rejecting `networkPolicy:`
		// in values.yaml, or helm refusing to render
		// templates/networkpolicy.yaml because the
		// file is missing, is exactly the case this
		// test was written to catch. A skip would
		// silently remove the chart/fixture label
		// conformance invariant and let a future PR
		// delete the chart scaffolding unnoticed.
		//
		// The test MUST fail open with full helm
		// diagnostic output so a reviewer can fix
		// the chart, not paper over the gap.
		t.Fatalf("helm template failed: %v\n--- output ---\n%s", err, string(out))
	}
	return string(out)
}

// TestFixtureLabelsConformToChart gates the
// cross-product: every selector key the chart
// draws must appear in the fixture with exactly
// the same value. We read the rendered manifest
// (not the template), which gives the actual
// values that Cilium will key off.
func TestFixtureLabelsConformToChart(t *testing.T) {
	const releaseName = "nexus-cni-test"

	rendered := executeHelmTemplate(t, releaseName)

	// Real selector labels lexed from the
	// rendered manifest. We expect a stable
	// subset of the form `key: "val"` in
	// podSelector.matchLabels. The chart draws
	// one NetworkPolicy per role, each selects
	// by a different value of
	// `app.kubernetes.io/component`.
	podSels := extractRenderedPodSelectorLabels(t, rendered)

	fixturePath := filepath.Join(moduleRootOrFatal(t),
		"scripts", "fixtures", "integrationcni", "01-test-pods.yaml")
	dataBytes, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	data := string(dataBytes)

	// For each rendered Chart selector key, the
	// fixture must use the same value in any pod
	// template labelled role=gateway or
	// role=worker. We perform line-by-line
	// block-aligned grep: starting at every
	// `role: gateway` or `role: worker` match,
	// look ±30 lines for `key: "value"`. Missing
	// is a fail.
	var lines []string
	for _, l := range strings.Split(data, "\n") {
		lines = append(lines, l)
	}

	checkBlock := func(role string, miss []string) []string {
		expect := map[string]string{}
		for _, sel := range podSels {
			c, ok := sel["app.kubernetes.io/component"]
			if !ok {
				continue
			}
			if c == role {
				expect = sel
				break
			}
		}
		if len(expect) == 0 {
			// No chart policy for this fixture role.
			// That's a chart-side decision (e.g.
			// prod values don't render a
			// worker-policy). This test does not
			// fail; the heavy CNI gate will.
			return miss
		}
		for i := 0; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) != "role: "+role {
				continue
			}
			from := max(0, i-3)
			to := min(len(lines), i+30)
			block := strings.Join(lines[from:to], "\n")
			for k, v := range expect {
				if !strings.Contains(block, k+": "+v) {
					miss = append(miss,
						"role="+role+" (line "+itoa(i+1)+
							"): chart expects `"+k+": "+v+
							"` but this fixture block does not declare it.")
				}
			}
		}
		return miss
	}

	missing := []string{}
	missing = checkBlock("gateway", missing)
	missing = checkBlock("worker", missing)

	if len(missing) > 0 {
		t.Fatalf("fixture labels drift from chart NetworkPolicy selectors:\n  • %s\n\n"+
			"the heavy CNI gate would still pass while enforcing NO rules.\n"+
			"fix the fixture (or the chart) so every chart selector key is\n"+
			"mirrored exactly in the fixture pod template(s).",
			strings.Join(missing, "\n  • "))
	}
}

// extractRenderedPodSelectorLabels returns, for
// each rendered NetworkPolicy object (delimited
// by `kind: NetworkPolicy`), a map of selector
// labels that fixture Pods must mirror.
//
// We deliberately do NOT union selector labels
// across policies: chart draws one policy per
// role (gateway / worker / migration /
// netpolicy) and each policy selects by a
// different value of `app.kubernetes.io/component`.
// A naïve union would erroneously require every
// role to satisfy every component value.
func extractRenderedPodSelectorLabels(t *testing.T, rendered string) []map[string]string {
	t.Helper()
	var policies []map[string]string
	// Split by `kind: NetworkPolicy` anchor
	const kindAnchor = "kind: NetworkPolicy"
	idx := 0
	for {
		at := strings.Index(rendered[idx:], kindAnchor)
		if at < 0 {
			break
		}
		start := idx + at
		// next doc boundary (next kind anchor)
		next := strings.Index(rendered[start+len(kindAnchor):], kindAnchor)
		var doc string
		if next < 0 {
			doc = rendered[start:]
		} else {
			doc = rendered[start : start+len(kindAnchor)+next]
		}
		idx = start + len(kindAnchor) + 1

		// in this doc, find podSelector.matchLabels with at least one key
		sel := map[string]string{}
		const sel0 = "podSelector:"
		s0 := strings.Index(doc, sel0)
		if s0 < 0 {
			continue
		}
		s1 := strings.Index(doc[s0:], "matchLabels:")
		if s1 < 0 {
			continue
		}
		after := doc[s0+s1+len("matchLabels:"):]
		lines := strings.SplitN(after, "\n", 30)
		for i := 0; i < min(20, len(lines)); i++ {
			line := lines[i]
			if line != "" && !strings.HasPrefix(line, "      ") {
				break
			}
			trim := strings.TrimSpace(line)
			colon := strings.Index(trim, ":")
			if colon < 0 {
				continue
			}
			key := strings.TrimSpace(trim[:colon])
			val := strings.Trim(strings.TrimSpace(trim[colon+1:]), `"`)
			if val == "" || strings.HasPrefix(val, "{") {
				continue
			}
			// skip non-label keys
			if !strings.HasPrefix(key, "app.kubernetes.io/") {
				// but legacy "app: ..." labels may also exist
				if !strings.HasPrefix(key, "app:") &&
					!strings.HasPrefix(key, "role:") {
					continue
				}
			}
			sel[key] = val
		}
		if len(sel) > 0 {
			policies = append(policies, sel)
		}
	}
	if len(policies) == 0 {
		t.Fatalf("could not extract any NetworkPolicy podSelector from rendered manifest")
	}
	return policies
}

func moduleRootOrFatal(t *testing.T) string {
	t.Helper()
	root, err := moduleRoot()
	if err != nil {
		t.Fatalf("cannot find project root: %v", err)
	}
	return root
}

// moduleRoot resolves the project root by walking
// up from the source file of the calling test
// (or, if that's not in the repo, from os.Getwd()).
// Why: `go test` does not always honour the
// invocation cwd — `_test.go` files run from
// `$GOCACHE` paths and `exec` does not restore
// cwd. The previous implementation used
//
//	wd := "."
//	wd = filepath.Dir(filepath.Dir(wd))
//
// which is broken when `wd == "."` (filepath.Dir
// returns ".", the loop never advances, we
// iterate 8 times and fail).
// resolveModuleRoot walks up from the test
// source file (and from os.Getwd()) until it
// finds a directory containing go.mod.
//
// Why this exists:
//   - `go test` does not honour the invocation
//     cwd reliably — `_test.go` files run from
//     the build cache, and any child
//     `exec.Command(...)` reads /tmp first.
//   - the original `moduleRoot()` factory
//     (kept in d2b_migration_label_test.go
//     before this fix) used
//     wd = filepath.Dir(filepath.Dir(wd))
//     and that returns "." when wd == ".",
//     so the loop never advanced off ".";
//     every test that called moduleRoot was
//     either silently skipping or blowing up
//     far from its source.
//
// Reset by: deleting this helper and pointing
// callers at runtime.Caller / os.Getwd-based
// walking in each test.
func resolveModuleRoot() (string, error) {
	candidates := []string{}
	if _, thisFile, _, ok := runtime.Caller(0); ok {
		dir := filepath.Dir(thisFile)
		for i := 0; i < 8; i++ {
			candidates = append(candidates, dir)
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if wd, err := os.Getwd(); err == nil {
		dir := wd
		for i := 0; i < 8; i++ {
			candidates = append(candidates, dir)
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "go.mod")); err == nil {
			if abs, err := filepath.Abs(c); err == nil {
				return abs, nil
			}
			return c, nil
		}
	}
	return "", errModuleRootNotFound
}

var errModuleRootNotFound = errors.New("go.mod not found in any candidate")

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var buf []byte
	for i > 0 {
		buf = append([]byte{digits[i%10]}, buf...)
		i /= 10
	}
	return string(buf)
}
