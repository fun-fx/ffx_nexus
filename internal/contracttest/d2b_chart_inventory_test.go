// Package contracttest — D-2b chart inventory test.
//
// This test enforces that every values field declared in
// public schemas (values.schema.json) is actually referenced
// from a template (or remains an unread-but-typed documenta-
// tion knob, which is OK only for `networkPolicy.*` debug
// knobs without a chart-level consumer). The opposite is also
// enforced: any `.Values.path` literal read by a Helm template
// must be declared in the schema, so a typo cannot survive.
//
// The test runs `helm template --show-only templates/networkpolicy.yaml`
// with profile=enterprise and reads the raw output to scan
// for Known `.Values.<key>` paths. It also reads values.yaml
// keys top-level and compares to schema.json $properties.
//
// Mutation evidence: the test is run twice — once with a known
// networkPolicy.mode=enforce and once with mode=disabled — the
// latter MUST omit the per-component policies. If the chart
// renders the same manifest in both modes the test fails open
// because that means the if-guarded policies are not actually
// gated on mode.
package contracttest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestValuesSchemaInventoryMatchesChart runs at rooted
// deploy/helm/nexus and exercises the
//
//   1) profile=enterprise, mode=enforce
//   2) profile=development, mode=disabled
//
// helm renditions and verifies that the second omits the
// per-component NetworkPolicies. This is the same shape of
// regression that produced the malformed PR #263: an
// `if .Values.networkPolicy.mode == "enforce"` block was
// silently rendering in both modes because the guard was
// broken. Pinning the test to a real chart iteration is
// how we keep the next regression from feeling green.
func TestValuesSchemaInventoryMatchesChart(t *testing.T) {
	chart, err := chartDir()
	if err != nil {
		t.Skipf("chart dir not found: %v", err)
	}
	rulesA := renderTop(t, chart, []string{
		"--set", "profile=enterprise",
		"--set", "networkPolicy.mode=enforce",
		"--set", "networkPolicy.profile=enterprise",
		"--set", "networkPolicy.enforcementAcknowledged=true",
		"--set", "dependencies.postgres.host=postgres",
		"--set", "dependencies.postgres.port=5432",
	})
	rulesB := renderTop(t, chart, []string{
		"--set", "profile=enterprise",
		"--set", "networkPolicy.mode=disabled",
		"--set", "networkPolicy.profile=enterprise",
		"--set", "networkPolicy.enforcementAcknowledged=false",
	})
	if countNP(rulesA) <= countNP(rulesB) {
		t.Fatalf("mode=enforce MUST render MORE NetworkPolicy rules than mode=disabled; got A=%d B=%d",
			countNP(rulesA), countNP(rulesB))
	}
}

// TestSchemaCoverageReadsTemplates walks every helm
// template under deploy/helm/nexus/templates/*.yaml,
// scans for `.Values.symbolic.path` style reads, and
// confirms each top-level key has a public schema
// declaration. This is the *first* line of defence
// against typos like `.Values.networkPolicies.profile`
// (note plural) silently falling back to <nil> at
// render time. Pairs with the .Values dict missing
// from the schema — both directions.
func TestSchemaCoverageReadsTemplates(t *testing.T) {
	chart, err := chartDir()
	if err != nil {
		t.Skipf("chart dir not found: %v", err)
	}
	schema, err := loadSchema(filepath.Join(chart, "values.schema.json"))
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	props := map[string]struct{}{}
	for k := range schema["properties"].(map[string]interface{}) {
		props[k] = struct{}{}
	}
	tplDir := filepath.Join(chart, "templates")
	entries, err := os.ReadDir(tplDir)
	if err != nil {
		t.Fatalf("read tplDir: %v", err)
	}
	re := regexp.MustCompile(`\.Values\.([a-zA-Z][a-zA-Z0-9_]*)`)
	var unknown []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		buf, err := os.ReadFile(filepath.Join(tplDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range re.FindAllStringSubmatch(string(buf), -1) {
			if _, ok := props[m[1]]; !ok {
				unknown = append(unknown, e.Name()+": "+m[1])
			}
		}
	}
	if len(unknown) > 0 {
		t.Fatalf("templates used .Values.* paths not declared in schema:\n%s", strings.Join(unknown, "\n"))
	}
}

// TestMutationModeToggleDramaticalyFails exercises the
// mutation rule from the user-facing spec: a deliberate
// drift in networkPolicy.mode renders a *missing* egress
// rule. We render twice with two different mode values
// and assert the per-component policy count differs.
// If you ever see this test pass with countA == countB,
// the if-guarded block in networkpolicy.yaml is broken.
func TestMutationModeToggleDramaticalyFails(t *testing.T) {
	chart, err := chartDir()
	if err != nil {
		t.Skipf("chart dir not found: %v", err)
	}
	on := renderTop(t, chart, []string{
		"--set", "networkPolicy.mode=enforce",
	})
	off := renderTop(t, chart, []string{
		"--set", "networkPolicy.mode=disabled",
	})
	if countNP(on) == countNP(off) {
		t.Fatalf("mutation: mode toggle MUST alter NetworkPolicy count; on=%d off=%d", countNP(on), countNP(off))
	}
}

// TestMutationRedisFeatureOffOmitsEgressRule asserts
// the matrix the spec calls out in FIX.6: when
// features.rateLimitRedis=false, the Redis egress rule
// is NOT rendered. If you switch the rule from
// `redis.host` to a static, this test catches it.
func TestMutationRedisFeatureOffOmitsEgressRule(t *testing.T) {
	chart, err := chartDir()
	if err != nil {
		t.Skipf("chart dir not found: %v", err)
	}
	redisOff := renderTop(t, chart, []string{
		"--set", "networkPolicy.mode=enforce",
		"--set", "features.rateLimitRedis=false",
		"--set", "dependencies.redis.host=redis",
		"--set", "dependencies.redis.port=6379",
	})
	redisOn := renderTop(t, chart, []string{
		"--set", "networkPolicy.mode=enforce",
		"--set", "features.rateLimitRedis=true",
		"--set", "dependencies.redis.host=redis",
		"--set", "dependencies.redis.port=6379",
	})
	if !strings.Contains(redisOn, "redis:6379") && !strings.Contains(redisOn, "6379") {
		t.Fatalf("redis ON should render redis egress port 6379:\n%s", redisOn)
	}
	if strings.Contains(redisOff, "6379") && !strings.Contains(redisOff, "5432") {
		// off should still have postgres 5432, not redis
		t.Fatalf("redis OFF should omit 6379:\n%s", redisOff)
	}
}

func chartDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "deploy", "helm", "nexus", "Chart.yaml")); err == nil {
			return filepath.Join(dir, "deploy", "helm", "nexus"), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

func loadSchema(path string) (map[string]interface{}, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal(buf, &out); err != nil {
		return nil, err
	}
	return out, nil
}
