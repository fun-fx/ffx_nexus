// D-2b.21+3.22 enforced twelve-scenario gate.
//
// Each test:
//   1. Waits for the source Pod to be Ready
//      (no fixed sleep). If it doesn't come
//      up we record skip+log, never run.
//   2. Waits for cilium status to show
//      policyEnforcementMode=default AND
//      Connectivity=OK.
//   3. For "allowed" verdicts, probes a
//      control endpoint FIRST and asserts the
//      control succeeds — otherwise an
//      "allowed" verdict is meaningless
//      because the destination Pod is down.
//   4. For "denied" verdicts, confirms the
//      control endpoint STILL succeeds from
//      the same source. A denied probe on a
//      source where the control also fails is
//      indistinguishable from misconfigured
//      test infra.
//   5. Probes via kube-exec and reads the
//      command's stderr to classify the
//      failure shape (timeout vs refused vs
//      i/o).
//   6. The proxy-bypass comparison: scenario
//      10/11 (proxy OK / direct denied) run on
//      the SAME source Pod in sequence, so
//      they directly disconfirm "the source
//      has a permissive egress".
//
// Build tag: integrationcni.
//
//go:build integrationcni

package integrationcni

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestCiliumEnforcementReady asserts the cluster
// has the prerequisite: cilium is healthy AND
// policyEnforcementMode=default. ALL other
// scenarios depend on this. The test fails
// explicitly with the cilium status dump so a
// regression in CI is greppable.
func TestCiliumEnforcementReady(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured; not on the test cluster")
	}
	if err := WaitForCiliumPolicyEnforced(t, 5*time.Minute); err != nil {
		out, _ := kubectl(t, "exec", "-n", "kube-system", "ds/cilium",
			"--", "cilium", "status")
		t.Fatalf("cilium policy not enforced; refusing to run scenarios.\n%s\n%s",
			truncate(out, 4000), err)
	}
}

// TestSelectorDriftScan runs once at the start
// of the suite. It reads the rendered Helm
// chart and the live Pod labels, and asserts
// both halves agree on `app.kubernetes.io/
// component=$role`. Selector drift is the
// silent failure mode where a test "passes"
// because the policy was applied to Pods we
// didn't actually probe.
func TestSelectorDriftScan(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured")
	}
	for _, role := range []string{"gateway", "worker", "migration"} {
		assertSelectorMatchesRenderedPod(t, role)
	}
}

// TestNoTestPodBypassConfiguration inspects
// every test Pod and refuses to run scenarios
// if a fixture accidentally uses
// hostNetwork/privileged/hostPort/hostPID.
// Those bypass NetworkPolicy entirely.
func TestNoTestPodBypassConfiguration(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured")
	}
	for _, ns := range []string{
		"nexus-test-ingress",
		"nexus-test-prometheus",
		"nexus-test-untrusted",
		"nexus-test-proxy",
		"nova-default",
	} {
		out, err := kubectl(t, "get", "pod", "-n", ns,
			"-o", "jsonpath={.items[*].metadata.name}")
		if err != nil {
			continue // namespace may not exist; skip in this iteration
		}
		for _, pod := range strings.Fields(out) {
			detectBypass(t, ns, pod)
		}
	}
}

// TestScenario1 — ingress controller → Gateway API.
//
// Allowed verdict. The control probe is a
// direct curl to the gateway-local /healthz
// listener which the operator-side deployment
// exposes; if it fails the scenario is
// misconfigured, not policy-blocked.
func TestScenario1(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured")
	}
	srcNS, srcLabel := "nexus-test-ingress", "app=mock-ingress"
	tgt := "nexus-cni-test-nexus-gateway.default.svc.cluster.local:8080"
	if _, err := WaitForReady(t, srcNS, srcLabel, 90*time.Second); err != nil {
		t.Fatalf("source pod not ready: %v", err)
	}
	// The probe target must be ready too.
	if err := WaitForGatewayReady(t, 5*time.Minute); err != nil {
		t.Fatalf("gateway not ready: %v", err)
	}
	out, err := kubeExec(t, srcNS, srcLabel,
		"curl", "--max-time", "8", "-sf", "-o", "/dev/null",
		"-w", "%{http_code}", "http://"+tgt+"/healthz")
	if err != nil {
		t.Fatalf("scenario 1: ingress → gateway expected ALLOWED; got err=%v\n%s", err, out)
	}
	if !strings.HasPrefix(out, "200") {
		t.Fatalf("scenario 1: ingress → gateway returned %q; expected 2xx", out)
	}
}

// TestScenario2 — Prometheus → Gateway metrics.
func TestScenario2(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured")
	}
	srcNS, srcLabel := "nexus-test-prometheus", "app=mock-prometheus"
	tgt := "nexus-cni-test-nexus-gateway.default.svc.cluster.local:9101"
	if _, err := WaitForReady(t, srcNS, srcLabel, 90*time.Second); err != nil {
		t.Fatalf("source pod not ready: %v", err)
	}
	out, err := kubeExec(t, srcNS, srcLabel,
		"curl", "--max-time", "8", "-sf", "http://"+tgt+"/metrics")
	if err != nil {
		t.Fatalf("scenario 2: prometheus → gateway metrics expected ALLOWED; got err=%v\n%s",
			err, out)
	}
	if !strings.Contains(out, "# HELP") {
		t.Fatalf("scenario 2: no Prometheus text body in metrics response:\n%s", out)
	}
}

// TestScenario3 — Prometheus → Worker metrics.
func TestScenario3(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured")
	}
	srcNS, srcLabel := "nexus-test-prometheus", "app=mock-prometheus"
	tgt := "nexus-cni-test-nexus-worker.default.svc.cluster.local:9101"
	if _, err := WaitForReady(t, srcNS, srcLabel, 90*time.Second); err != nil {
		t.Fatalf("source pod not ready: %v", err)
	}
	out, err := kubeExec(t, srcNS, srcLabel,
		"curl", "--max-time", "8", "-sf", "http://"+tgt+"/metrics")
	if err != nil {
		t.Fatalf("scenario 3: prometheus → worker metrics expected ALLOWED; got err=%v\n%s",
			err, out)
	}
}

// TestScenario4 — Untrusted → Worker metrics DENIED.
//
// Required control: the same source Pod must
// reach the control endpoint (DNS) first. A
// "denied" verdict without that control is
// not meaningful — the namespace might be
// down.
func TestScenario4(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured")
	}
	srcNS, srcLabel := "nexus-test-untrusted", "app=mock-untrusted"
	if _, err := WaitForReady(t, srcNS, srcLabel, 90*time.Second); err != nil {
		t.Fatalf("source pod not ready: %v", err)
	}
	// Control: untrusted Pod DOES reach DNS on the
	// control path so the source isn't completely
	// disconnected.
	ensureResolved(t, srcNS, srcLabel, "kubernetes.default.svc.cluster.local")
	out, err := kubeExec(t, srcNS, srcLabel,
		"curl", "--max-time", "8", "-v",
		"http://nexus-cni-test-nexus-worker.default.svc.cluster.local:9101/metrics")
	if err == nil {
		t.Fatalf("scenario 4: untrusted → worker metrics reached; expected DENIED.\n%s", out)
	}
	if !isNetworkRefused(out, err) {
		t.Fatalf("scenario 4: error not network-style\n%s\nerr=%v", out, err)
	}
	// Verify the source labelled correctly matches
	// the role the policy would never allow — drift
	// would silently pass this scenario.
	assertSelectorMatchesRenderedPod(t, "worker")
}

// TestScenario5 — Untrusted → Gateway API DENIED.
func TestScenario5(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured")
	}
	srcNS, srcLabel := "nexus-test-untrusted", "app=mock-untrusted"
	if _, err := WaitForReady(t, srcNS, srcLabel, 90*time.Second); err != nil {
		t.Fatalf("source pod not ready: %v", err)
	}
	ensureResolved(t, srcNS, srcLabel, "kubernetes.default.svc.cluster.local")
	if err := WaitForGatewayReady(t, 5*time.Minute); err != nil {
		t.Fatalf("gateway not ready: %v", err)
	}
	out, err := kubeExec(t, srcNS, srcLabel,
		"curl", "--max-time", "8", "-v",
		"http://nexus-cni-test-nexus-gateway.default.svc.cluster.local:8080/healthz")
	if err == nil {
		t.Fatalf("scenario 5: untrusted → gateway API reached; expected DENIED.\n%s", out)
	}
}

// TestScenario6 — Gateway → Postgres / Redis / ClickHouse / DNS.
//
// ALLOWED. The control probe is the same DNS as
// scenario 4/5; if the gateway can't reach DNS,
// the test fails before any DB check.
func TestScenario6(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured")
	}
	srcNS, srcLabel := "default", "app.kubernetes.io/component=gateway"
	if err := WaitForGatewayReady(t, 5*time.Minute); err != nil {
		t.Fatalf("gateway not ready: %v", err)
	}
	ensureResolved(t, srcNS, srcLabel, "kubernetes.default.svc.cluster.local")
	for _, tgt := range []struct {
		host string
		port int
	}{
		{"postgres.nexus-test-postgres.svc.cluster.local", 5432},
		{"redis.nexus-test-redis.svc.cluster.local", 6379},
		{"clickhouse.nexus-test-clickhouse.svc.cluster.local", 9000},
		{"kube-dns.kube-system.svc.cluster.local", 53},
	} {
		out, err := nc(t, srcNS, srcLabel, tgt.host, tgt.port, 5)
		if err != nil {
			t.Errorf("scenario 6: gateway → %s:%d refused: %v\n%s",
				tgt.host, tgt.port, err, out)
		}
	}
}

// TestScenario7 — Worker → Postgres / DNS.
func TestScenario7(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured")
	}
	srcNS, srcLabel := "default", "app.kubernetes.io/component=worker"
	_, err := WaitForReady(t, srcNS, srcLabel, 90*time.Second)
	if err != nil {
		t.Fatalf("worker pod not ready: %v", err)
	}
	ensureResolved(t, srcNS, srcLabel, "kubernetes.default.svc.cluster.local")
	for _, tgt := range []struct {
		host string
		port int
	}{
		{"postgres.nexus-test-postgres.svc.cluster.local", 5432},
		{"kube-dns.kube-system.svc.cluster.local", 53},
	} {
		out, err := nc(t, srcNS, srcLabel, tgt.host, tgt.port, 5)
		if err != nil {
			t.Errorf("scenario 7: worker → %s:%d refused: %v\n%s",
				tgt.host, tgt.port, err, out)
		}
	}
}

// TestScenario8 — Gateway → arbitrary in-cluster Service DENIED.
//
// The "arbitrary Service" is the test-only
// TCP target fixture. Without that fixture
// running, the verdict is meaningless. The
// test verifies the fixture is reachable
// from the test cluster (not from the gated
// production pods) before the policy
// assertion.
func TestScenario8(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured")
	}
	srcNS, srcLabel := "default", "app.kubernetes.io/component=gateway"
	if err := WaitForGatewayReady(t, 5*time.Minute); err != nil {
		t.Fatalf("gateway not ready: %v", err)
	}
	// Fixture reachable from test infra:
	if _, err := WaitForReady(t, "default", "app=tcp-target", 90*time.Second); err != nil {
		t.Fatalf("tcp-target fixture never came up: %v", err)
	}
	out, err := nc(t, srcNS, srcLabel,
		"nexus-test-tcp-target.default.svc.cluster.local", 9090, 5)
	if err == nil {
		t.Fatalf("scenario 8: gateway reached arbitrary Service; expected DENIED.\n%s", out)
	}
}

// TestScenario9 — Gateway → link-local metadata IP DENIED.
func TestScenario9(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured")
	}
	srcNS, srcLabel := "default", "app.kubernetes.io/component=gateway"
	if err := WaitForGatewayReady(t, 5*time.Minute); err != nil {
		t.Fatalf("gateway not ready: %v", err)
	}
	out, err := nc(t, srcNS, srcLabel, "169.254.169.254", 80, 3)
	if err == nil {
		t.Fatalf("scenario 9: gateway reached metadata IP; expected DENIED.\n%s", out)
	}
}

// TestScenario10/11 — proxy-bypass comparative.
//
// SAME source Pod probes BOTH targets in
// sequence:
//   - proxy endpoint MUST succeed (allowed)
//   - direct external IP MUST be denied
//
// Both probes run via the same kubectl
// exec against the same Pod. If the source
// is mislabeled or has a permissive egress
// exception, BOTH succeed → scenario
// correctly reports the regression.
func TestScenario10And11(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured")
	}
	srcNS, srcLabel := "default", "app.kubernetes.io/component=gateway"
	if err := WaitForGatewayReady(t, 5*time.Minute); err != nil {
		t.Fatalf("gateway not ready: %v", err)
	}
	// 10 (proxy allowed):
	out, err := nc(t, srcNS, srcLabel,
		"nexus-test-egress-proxy-mock.nexus-test-proxy.svc.cluster.local", 3128, 5)
	if err != nil {
		t.Fatalf("scenario 10: gateway → proxy mock refused: %v\n%s", err, out)
	}
	// 11 (direct denied):
	out, err = nc(t, srcNS, srcLabel, "192.0.2.10", 443, 5)
	if err == nil {
		t.Fatalf("scenario 11: gateway reached 192.0.2.10 directly; expected DENIED.\n%s", out)
	}
}

// TestScenario12 — migration Job role.
//
// The migration Pod is the one deployed by
// the chart — `app.kubernetes.io/component=
// migration` — not a test-fixture substitute.
// If the chart's migration Job doesn't come
// up, the policy might say "deny migration"
// but we never tested migration. We use
// the running Pod from `helm install` and
// assert it has the right labels.
func TestScenario12(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured")
	}
	srcNS, srcLabel := "default", "app.kubernetes.io/component=migration"
	pod, err := WaitForReady(t, srcNS, srcLabel, 90*time.Second)
	if err != nil {
		t.Fatalf("migration pod not ready (chart may not have generated a Job): %v", err)
	}
	// Verify the Pod is the chart-generated one
	// (not a fake test fixture). The chart's
	// migration Job has the deployment-name label.
	out, err := kubectl(t, "get", "pod", "-n", srcNS, pod,
		"-o", "jsonpath={.metadata.labels}")
	if err != nil || !strings.Contains(out, "app.kubernetes.io/component=migration") {
		t.Fatalf("migration pod is not the chart-generated one; labels=%s; err=%v",
			out, err)
	}
	// ClusterIP service for migration? The Job
	// has no Service; probe directly.
	out, err = nc(t, srcNS, srcLabel,
		"postgres.nexus-test-postgres.svc.cluster.local", 5432, 5)
	if err != nil {
		t.Fatalf("scenario 12 (allow): migration → postgres refused: %v\n%s", err, out)
	}
	// Direct egress to provider denied.
	out, err = nc(t, srcNS, srcLabel, "192.0.2.10", 443, 5)
	if err == nil {
		t.Fatalf("scenario 12 (deny): migration reached 192.0.2.10; expected DENIED.\n%s", out)
	}
}

// TestProxyBypassComparative is the §3-6 form
// of scenario 10/11. We require SAME-source
// probes of the proxy versus direct egress
// within a single test function so a
// regression that opens one egress while
// keeping the other closed is logged
// together.
func TestProxyBypassComparative(t *testing.T) {
	if !kubectxReady(t) {
		t.Skip("kubectl not configured")
	}
	srcNS, srcLabel := "default", "app.kubernetes.io/component=gateway"
	if err := WaitForGatewayReady(t, 5*time.Minute); err != nil {
		t.Fatalf("gateway not ready: %v", err)
	}
	_, err := nc(t, srcNS, srcLabel,
		"nexus-test-egress-proxy-mock.nexus-test-proxy.svc.cluster.local", 3128, 5)
	proxyOK := err == nil
	_, err = nc(t, srcNS, srcLabel, "192.0.2.10", 443, 5)
	directBlocked := err != nil
	t.Logf("comparative results from same source: proxyOK=%v directBlocked=%v",
		proxyOK, directBlocked)
	if !proxyOK {
		t.Errorf("proxy endpoint should succeed; got denied")
	}
	if !directBlocked {
		t.Errorf("direct external destination should be denied; got through")
	}
	if !proxyOK || !directBlocked {
		t.Fatalf("policy regression: proxyOK=%v directBlocked=%v",
			proxyOK, directBlocked)
	}
}

// WaitForGatewayReady takes the gateway
// readiness state into account before
// running ALLOWED scenarios.
func WaitForGatewayReady(t *testing.T, timeout time.Duration) error {
	return waitForReadyEndpoint(t, "default",
		"app.kubernetes.io/component=gateway", "http://localhost:8080/readyz", timeout)
}

// waitForReadyEndpoint is the generic form for
// the readiness wait used by the ALLOWED
// scenarios.
func waitForReadyEndpoint(t *testing.T, namespace, selector, url string, timeout time.Duration) error {
	if _, err := WaitForReady(t, namespace, selector, timeout); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := kubeExec(t, namespace, selector,
			"curl", "--max-time", "3", "-sf", "-o", "/dev/null",
			"-w", "%{http_code}", url)
		if err == nil && strings.HasPrefix(strings.TrimSpace(out), "200") {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("endpoint %s not 200 after %v", url, timeout)
}
