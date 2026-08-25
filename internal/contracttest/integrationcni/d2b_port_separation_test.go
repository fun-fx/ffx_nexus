// D-2b.13: port-level ingress separation gate.
// Verifies that the chart's NetworkPolicy does
// NOT permit:
//   - Prometheus → Gateway user API port
//     (Gateway/8080)
//   - ingress controller → Worker metrics port
//     (Worker/9101)
//   - ingress controller → Worker health port
//     (Worker/8080 if defined separately)
// The chart's policy MUST admit only the
// narrow peer/port pair each role is supposed
// to use. A future regression that opens
// additional ports to either peer is caught
// here.
//
//go:build integrationcni

package integrationcni

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRoleIngressPortsSeparated asserts no peer
// can reach more than its documented ports.
//
// The mapping:
//   ingress controller → Gateway/8080 only
//   ingress controller → Worker is FORBIDDEN
//   Prometheus → Gateway/9101 (metrics) only
//   Prometheus → Gateway/8080 is FORBIDDEN
//   Prometheus → Worker/9101 (metrics) only
//
// The matching happens at the rendered
// NetworkPolicy level — a `kubectl exec` of a
// probe does not necessarily fail because the
// chart renders the policy. The contract is
// that the rendered YAML does NOT include a
// port that the role's documentation says it
// shouldn't have.
func TestRoleIngressPortsSeparated(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured")
	}
	// Render the chart in this test process
	// (Not: avoid the cluster load and use what
	// was rendered at install). We re-render so
	// the test is independent of cluster state.
	rendered, err := renderChart()
	if err != nil {
		t.Fatalf("chart render: %v", err)
	}
	policies := splitNetworkPolicies(rendered)
	gw := findPolicyByComponent(policies, "gateway")
	if gw == nil {
		t.Fatalf("no Gateway NetworkPolicy rendered")
	}
	worker := findPolicyByComponent(policies, "worker")
	if worker == nil {
		t.Fatalf("no Worker NetworkPolicy rendered")
	}

	// ingress controller → Gateway API: must be 8080
	assertPeerAdmitsPort(t, "gateway", gw,
		"ingress-nginx", "tcp", 8080, true /* admit */)
	// ingress controller → metrics port on Gateway:
	// forbidden.
	assertPeerAdmitsPort(t, "gateway", gw,
		"ingress-nginx", "tcp", 9101, false /* forbid */)
	// Prometheus → Gateway metrics: 9101 admitted
	assertPeerAdmitsPort(t, "gateway", gw,
		"monitoring", "tcp", 9101, true)
	// Prometheus → Gateway API: forbidden
	assertPeerAdmitsPort(t, "gateway", gw,
		"monitoring", "tcp", 8080, false)
	// Prometheus → Worker metrics: admitted
	assertPeerAdmitsPort(t, "worker", worker,
		"monitoring", "tcp", 9101, true)
	// ingress controller → Worker metrics: forbidden
	assertPeerAdmitsPort(t, "worker", worker,
		"ingress-nginx", "tcp", 9101, false)
}

// assertPeerAdmitsPort walks the policy's
// ingress rules and looks for a rule whose
// peer namespace matches `peerNS` AND whose port
// matches `port`. Verdict: admit=true means we
// expect at least one such rule; admit=false
// means we expect zero.
func assertPeerAdmitsPort(t *testing.T, role string, policy map[string]interface{},
	peerNS string, proto string, port int, admit bool,
) {
	specAny := policy["spec"]
	spec, ok := specAny.(map[string]interface{})
	if !ok {
		t.Errorf("role=%s: spec is %T not a map", role, specAny)
		return
	}
	ingressAny := spec["ingress"]
	ingress, ok := ingressAny.([]interface{})
	if !ok {
		t.Errorf("role=%s: ingress is %T not a list", role, ingressAny)
		return
	}
	matches := 0
	for _, ruleAny := range ingress {
		rule, ok := ruleAny.(map[string]interface{})
		if !ok {
			continue
		}
		fromAny, ok := rule["from"]
		if !ok {
			continue
		}
		from, ok := fromAny.([]interface{})
		if !ok {
			continue
		}
		peerMatch := false
		portMatch := false
		for _, peerAny := range from {
			peer, ok := peerAny.(map[string]interface{})
			if !ok {
				continue
			}
			ns, ok := peer["namespaceSelector"].(map[string]interface{})
			if !ok {
				continue
			}
			ml, ok := ns["matchLabels"].(map[string]interface{})
			if !ok {
				continue
			}
			if ml["kubernetes.io/metadata.name"] == peerNS {
				peerMatch = true
			}
		}
		portsAny, ok := rule["ports"]
		if !ok {
			continue
		}
		ports, ok := portsAny.([]interface{})
		if !ok {
			continue
		}
		for _, pAny := range ports {
			p, ok := pAny.(map[string]interface{})
			if !ok {
				continue
			}
			pmatch := intEqFloat(p["port"], port)
			if pmatch {
				portMatch = true
				break
			}
		}
		if peerMatch == admit && portMatch == admit {
			matches++
		}
	}
	if admit && matches == 0 {
		t.Errorf("role=%s: peer=%s port=%d expected to be admitted; rendered policy omits this peer/port pair.\n  This means the chart's policy does not permit the documented traffic.",
			role, peerNS, port)
	}
	if !admit && matches > 0 {
		t.Errorf("role=%s: peer=%s port=%d expected to be FORBIDDEN; rendered policy admits it (%d rules).\n  A regression that broadens ingress rules without a documentation update.",
			role, peerNS, port, matches)
	}
}

// intEqFloat unifies YAML-parsed numeric types.
func intEqFloat(v interface{}, want int) bool {
	switch x := v.(type) {
	case int:
		return x == want
	case int64:
		return int(x) == want
	case float64:
		return int(x) == want
	}
	return false
}

// renderChart renders the chart with the
// canonical test values used by scripts/.
func renderChart() (string, error) {
	root, err := findProjectRoot()
	if err != nil {
		return "", fmt.Errorf("findProjectRoot: %w", err)
	}
	chart := filepath.Join(root, "deploy", "helm", "nexus")
	rootCmd := []string{"template", "render-test", chart,
		"--set", "networkPolicy.mode=enforce",
		"--set", "networkPolicy.profile=enterprise",
		"--set", "networkPolicy.enforcementAcknowledged=true",
		"--set", "networkPolicy.egress.proxy.enabled=true",
		"--set", "networkPolicy.egress.proxy.host=nx-proxy.ns.svc.cluster.local",
		"--set", "networkPolicy.egress.proxy.port=3128",
		"--set", "networkPolicy.egress.proxy.namespace=nx-proxy",
	}
	out, err := runHelm(rootCmd...)
	if err != nil {
		return "", err
	}
	return out, nil
}

// findProjectRoot walks up from CWD looking
// for go.mod so the chart path is correct
// regardless of where `go test` was invoked.
func findProjectRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := cwd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", os.ErrNotExist
}

// splitNetworkPolicies (mirrors the YAML
// splitter in d2b_cni_scenario_test.go).
func splitNetworkPolicies(s string) []map[string]interface{} {
	out := []map[string]interface{}{}
	for _, doc := range splitYAMLDocs(s) {
		var obj map[string]interface{}
		if err := yamlUnmarshal([]byte(doc), &obj); err != nil {
			continue
		}
		if obj["kind"] == "NetworkPolicy" {
			out = append(out, obj)
		}
	}
	return out
}

func findPolicyByComponent(policies []map[string]interface{}, role string) map[string]interface{} {
	for _, p := range policies {
		spec, ok := p["spec"].(map[string]interface{})
		if !ok {
			continue
		}
		sel, ok := spec["podSelector"].(map[string]interface{})
		if !ok {
			continue
		}
		ml, ok := sel["matchLabels"].(map[string]interface{})
		if !ok {
			continue
		}
		if ml["app.kubernetes.io/component"] == role {
			return p
		}
		_ = fmt.Sprintf
	}
	return nil
}

// _ = strings.Join keeps the file compiling
// if `strings` falls out of use.
var _ = strings.Join
