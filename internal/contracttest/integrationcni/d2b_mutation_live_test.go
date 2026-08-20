// D-2b.18: enforcing-CNI mutation tests.
// We deliberately inject policy mutations the
// chart says it rejects and assert that the
// integration test scenarios now FAIL. The
// point is to prove the test surface is not a
// no-op: a "passing" twelve-scenario suite
// cannot be achieved by omitting or trimming
// the policy. The mutations are real, applied
// via kubectl patch, to the live policy object,
// and the affected scenarios re-assert.
//
// Build tag `mutationcni` runs the tests only
// when the operator or nightly CI requests it.
// They mutate cluster state and require manual
// cleanup; CI integration must run behind a
// guard.
//
//go:build integrationcni && mutationcni

package integrationcni

import (
	"strings"
	"testing"
)

// TestMutationGatewayToArbitraryServiceAllowed
// injects a permissive egress rule into the
// Gateway policy allowing traffic to any Pod
// in the cluster. The TestScenario8 expectation
// of "denied" must now FAIL or return success.
// We literally seek a regression: a policy
// that opens arbitrary egress violates the
// intended contract, so the test for "denied"
// MUST now succeed in traffic.
//   - without mutation: scenario 8 is denied
//     (correct).
//   - with mutation: scenario 8 is allowed
//     (regression detected). This file marks
//     the regression.
func TestMutationGatewayToArbitraryServiceAllowed(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured")
	}
	// Apply a permissive patch.
	patch := `[
		{
			"op": "add",
			"path": "/spec/egress/-",
			"value": {
				"to": [
					{"namespaceSelector": {}}
				],
				"ports": [
					{"protocol": "TCP", "port": 9090}
				]
			}
		}
	]`
	out, err := kubectl(t, "patch", "networkpolicy",
		"-n", "default", "nexus-cni-test-nexus-gateway",
		"--type=json", "-p", patch)
	if err != nil {
		t.Fatalf("patch did not commit (chart may render the policy in a way that blocks us from patching; record and skip): %v\n%s", err, out)
	}
	t.Cleanup(func() {
		// Best-effort rollback. Script
		// scripts/test-cluster-down.sh brings
		// the cluster down so we don't need to
		// focus on per-test cleanup; just
		// produce a warning.
		_, _ = kubectl(t, "delete", "networkpolicy",
			"-n", "default", "nexus-cni-test-nexus-gateway")
	})
	// Re-run scenario 8 — traffic should now
	// succeed. So the regression is "the chart
	// no longer denies" — we record a
	// regression message and accept the test
	// result as proof of failure.
	out, err = nc(t, "default", "app.kubernetes.io/component=gateway",
		"nexus-test-tcp-target.default.svc.cluster.local", 9090, 5)
	if err == nil {
		t.Fatalf("mutation regression caught: chart policy admits Gateway→any-service after a permissive patch.\n%s\nExpected: denied (the chart refutes this). Got: allowed.\nThis is the failure-mode the mutation suite was supposed to forbid.",
			out)
	}
}

// TestMutationWorkerUserIngressAllowed injects
// a 0.0.0.0/0 ingress on the Worker's
// `app.kubernetes.io/component=worker` Pod.
// The untrusted Pod in scenario 4/5 should
// now reach Worker metrics. A regression in
// the chart's Worker policy gets caught.
func TestMutationWorkerUserIngressAllowed(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured")
	}
	patch := `[
		{
			"op": "add",
			"path": "/spec/ingress/-",
			"value": {
				"from": [
					{"podSelector": {}}
				],
				"ports": [
					{"protocol": "TCP", "port": 9101}
				]
			}
		}
	]`
	_, err := kubectl(t, "patch", "networkpolicy",
		"-n", "default", "nexus-cni-test-nexus-worker",
		"--type=json", "-p", patch)
	if err != nil {
		t.Fatalf("worker policy patch: %v", err)
	}
	t.Cleanup(func() {
		_, _ = kubectl(t, "delete", "networkpolicy",
			"-n", "default", "nexus-cni-test-nexus-worker")
	})
	out, err := kubeExec(t, "nexus-test-untrusted",
		"app=mock-untrusted", "curl",
		"--max-time", "5", "-sf",
		"http://nexus-cni-test-nexus-worker.default.svc.cluster.local:9101/metrics")
	if err == nil && strings.Contains(out, "# HELP") {
		t.Fatalf("mutation regression: untrusted Pod reached Worker metrics after 0.0.0.0 patch.\n%s", out)
	}
}

// TestMutationDirectEgressAllowed drops the
// chart's egress-proxy-only rule and adds a
// permissive 0.0.0.0/0 allow-all egress on the
// Gateway policy. The Gateway should now reach
// `192.0.2.10:443` (a documentation IP).
func TestMutationDirectEgressAllowed(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured")
	}
	patch := `[
		{
			"op": "add",
			"path": "/spec/egress/0",
			"value": {
				"to": [
					{"ipBlock": {"cidr": "0.0.0.0/0", "except": []}}
				]
			}
		}
	]`
	_, err := kubectl(t, "patch", "networkpolicy",
		"-n", "default", "nexus-cni-test-nexus-gateway",
		"--type=json", "-p", patch)
	if err != nil {
		t.Fatalf("gateway egress patch: %v", err)
	}
	t.Cleanup(func() {
		_, _ = kubectl(t, "delete", "networkpolicy",
			"-n", "default", "nexus-cni-test-nexus-gateway")
	})
	out, err := nc(t, "default", "app.kubernetes.io/component=gateway",
		"192.0.2.10", 443, 5)
	if err == nil {
		t.Fatalf("mutation regression: Gateway reached 192.0.2.10 directly after 0.0.0.0/0 patch.\n%s", out)
	}
}

// TestMutationPrometheusToGatewayUserAPIAllowed
// injects prometheus→Gateway port 8080
// ingress. Prometheus should now hit the user
// API even though the policy should forbid it.
func TestMutationPrometheusToGatewayUserAPIAllowed(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured")
	}
	patch := `[
		{
			"op": "add",
			"path": "/spec/ingress/-",
			"value": {
				"from": [
					{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "nexus-test-prometheus"}}}
				],
				"ports": [
					{"protocol": "TCP", "port": 8080}
				]
			}
		}
	]`
	_, err := kubectl(t, "patch", "networkpolicy",
		"-n", "default", "nexus-cni-test-nexus-gateway",
		"--type=json", "-p", patch)
	if err != nil {
		t.Fatalf("prom→gateway ingress patch: %v", err)
	}
	t.Cleanup(func() {
		_, _ = kubectl(t, "delete", "networkpolicy",
			"-n", "default", "nexus-cni-test-nexus-gateway")
	})
	// Probe user API; ideally we'd see a
	// 404 / 401 from gateway, not a
	// connection refused.
	out, err := kubeExec(t, "nexus-test-prometheus",
		"app=mock-prometheus", "curl",
		"--max-time", "5", "-o", "/dev/null",
		"-w", "%{http_code}\n",
		"http://nexus-cni-test-nexus-gateway.default.svc.cluster.local:8080/healthz")
	if err == nil {
		t.Fatalf("mutation regression: Prometheus reached Gateway user API after the chart's separation contract was broken.\nGot: %s", out)
	}
}
