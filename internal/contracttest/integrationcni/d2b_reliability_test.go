// D-2b.21 reliability hardening. The probe
// helpers enforce:
//   - readiness wait (no fixed-time sleeps)
//   - server-side reachability confirmation
//   - control endpoint comparison
//   - hostNetwork / privileged / hostPort /
//     hostPID bypass detection on test Pods
//   - selector match verification between
//     rendered chart and test Pod labels
//   - Cilium policy enforcement readiness
//     gate
//
//go:build integrationcni

package integrationcni

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// WaitForReady polls the readiness probe of a
// Pod until it becomes ready or the timeout
// expires. We never sleep a fixed interval
// because recently-booted servers are slow.
func WaitForReady(t *testing.T, namespace, selector string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var podName string
	for time.Now().Before(deadline) {
		out, err := kubectl(t, "get", "pod", "-n", namespace,
			"-l", selector, "-o", "jsonpath={.items[0].metadata.name}")
		if err == nil && strings.TrimSpace(out) != "" {
			podName = strings.TrimSpace(out)
			ready, _ := kubectl(t, "get", "pod", "-n", namespace,
				podName, "-o",
				"jsonpath={.status.conditions[?(@.type=='Ready')].status}")
			if strings.Contains(ready, "True") {
				return podName, nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return podName, fmt.Errorf("pod %s/%s not ready after %v", namespace, selector, timeout)
}

// WaitForCiliumPolicyEnforced waits for the
// Cilium agent to confirm both connectivity
// and policyEnforcementMode=default. Without
// this gate, scenario tests run before any
// eBPF program has loaded and would falsely
// pass.
func WaitForCiliumPolicyEnforced(t *testing.T, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_ = kubectl // log path
		out, err := kubectl(t, "exec", "-n", "kube-system", "ds/cilium",
			"--", "cilium", "status")
		if err == nil &&
			strings.Contains(out, "Policy enforcement mode: default") &&
			strings.Contains(out, "Connectivity: OK") {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	out, _ := kubectl(t, "exec", "-n", "kube-system", "ds/cilium",
		"--", "cilium", "status")
	return fmt.Errorf("cilium policy not enforced within %v; status tail: %s",
		timeout, truncate(out, 800))
}

// probeControlEndpoint hits an endpoint
// KNOWN to be permitted by the policy
// (Postgres / DNS / mock proxy). The probe
// must succeed before any "denied" assertion
// in scenario tests can be trusted. Without
// this control, a "denied" verdict becomes
// indistinguishable from "the server was
// still booting".
func probeControlEndpoint(t *testing.T, namespace, selector, host string, port int) error {
	_, err := WaitForReady(t, namespace, selector, 90*time.Second)
	if err != nil {
		return fmt.Errorf("control pod not ready: %w", err)
	}
	_, err = nc(t, namespace, selector, host, port, 5)
	return err
}

// ensureResolved is a one-liner that
// specifically marks DNS UDP/TCP 53 behavior:
//   - dig +short +timeout=3 +tries=1
//     returns an A record for a name the
//     cluster knows (kubernetes.default).
//   - dig +tcp +short +timeout=3 +tries=1
//     forces TCP fallback. Both must succeed.
func ensureResolved(t *testing.T, namespace, selector, name string) {
	if _, err := WaitForReady(t, namespace, selector, 90*time.Second); err != nil {
		t.Fatalf("dns probe pod not ready: %v", err)
	}
	udp, err := kubeExec(t, namespace, selector,
		"dig", "+short", "+timeout=3", "+tries=1",
		"@"+"kube-dns.kube-system.svc.cluster.local", name)
	if err != nil {
		t.Fatalf("dns udp: %v\n%s", err, udp)
	}
	tcp, err := kubeExec(t, namespace, selector,
		"dig", "+tcp", "+short", "+timeout=3", "+tries=1",
		"@"+"kube-dns.kube-system.svc.cluster.local", name)
	if err != nil {
		t.Fatalf("dns tcp: %v\n%s", err, tcp)
	}
	if !strings.Contains(udp, ".") || !strings.Contains(tcp, ".") {
		t.Fatalf("dns resolution returned empty:\nudp=%s\ntcp=%s", udp, tcp)
	}
}

// nsenterDetectionResult describes whether a
// Pod's runtime-config opaquely circumvents
// NetworkPolicy. hostNetwork/privileged/hostPort
// /hostPID are the canonical bypass; we fail
// any Pod that uses them in fixture manifests.
type nsenterDetectionResult struct {
	HostNetwork bool     `json:"hostNetwork"`
	HostPID     bool     `json:"hostPID"`
	HostIPC     bool     `json:"hostIPC"`
	Privileged  bool     `json:"privileged"`
	HostPorts   []string `json:"hostPorts"`
	BypassRisk  bool     `json:"bypassRisk"`
}

// detectBypass inspects a Pod's spec at the
// test boundary. We refuse to run scenarios
// that target a Pod that has been set up to
// bypass policy. A future test-fixture bug
// where `hostNetwork: true` slips in is
// caught here before corrupting the policy
// verdict.
func detectBypass(t *testing.T, namespace, podName string) nsenterDetectionResult {
	out, err := kubectl(t, "get", "pod", "-n", namespace, podName, "-o", "json")
	if err != nil {
		t.Fatalf("get pod spec: %v", err)
	}
	var spec struct {
		Spec struct {
			HostNetwork    bool          `json:"hostNetwork"`
			HostPID        bool          `json:"hostPID"`
			HostIPC        bool          `json:"hostIPC"`
			Containers     []containerSP `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(out), &spec); err != nil {
		t.Fatalf("parse pod spec: %v", err)
	}
	res := nsenterDetectionResult{
		HostNetwork: spec.Spec.HostNetwork,
		HostPID:     spec.Spec.HostPID,
		HostIPC:     spec.Spec.HostIPC,
	}
	for _, c := range spec.Spec.Containers {
		if c.SecurityContext != nil && c.SecurityContext.Privileged != nil &&
			*c.SecurityContext.Privileged {
			res.Privileged = true
		}
		for _, p := range c.Ports {
			if p.HostPort > 0 {
				res.HostPorts = append(res.HostPorts,
					fmt.Sprintf("%d/%s", p.HostPort, p.Protocol))
			}
		}
	}
	res.BypassRisk = res.HostNetwork || res.HostPID || res.HostIPC || res.Privileged ||
		len(res.HostPorts) > 0
	if res.BypassRisk {
		t.Errorf("POD BYPASS: %s/%s uses hostNetwork/hostPID/hostIPC/privileged/hostPort; scenarios against this Pod do not exercise NetworkPolicy.\n  %+v",
			namespace, podName, res)
	}
	return res
}

type containerSP struct {
	SecurityContext *struct {
		Privileged *bool `json:"privileged"`
	} `json:"securityContext"`
	Ports []struct {
		HostPort int    `json:"hostPort"`
		Protocol string `json:"protocol"`
	} `json:"ports"`
}

// assertSelectorMatchesRenderedPod verifies
// that the rendered chart's podSelector for a
// role exactly matches the labels of the
// running Pod. This catches the drift class
// "test fixture uses app.kubernetes.io/component=workerX but the chart's
// podSelector matches worker" — a selector
// drift makes scenarios pass for the wrong
// reason.
func assertSelectorMatchesRenderedPod(t *testing.T, role string) {
	rendered, err := renderChart()
	if err != nil {
		t.Fatalf("renderChart: %v", err)
	}
	selector := extractPodSelectorFromRender(rendered, role)
	if selector == nil {
		t.Fatalf("no podSelector for role=%q in rendered chart", role)
	}
	// Walk the rendered selector for the role's required key.
	want := map[string]string{
		"app.kubernetes.io/component": role,
		"app.kubernetes.io/part-of":   "nexus",
	}
	for k, v := range want {
		if got, ok := selector[k]; !ok || got != v {
			t.Errorf("rendered selector drift: role=%s missing %s=%s in %v",
				role, k, v, selector)
		}
	}
	out, err := kubectl(t, "get", "pod", "-n", "default",
		"-l", fmt.Sprintf("app.kubernetes.io/component=%s", role),
		"-o", "jsonpath={.items[0].metadata.labels}")
	if err != nil {
		t.Errorf("get pod labels for role=%s: %v", role, err)
		return
	}
	var live map[string]string
	if err := json.Unmarshal([]byte(out), &live); err != nil {
		t.Fatalf("parse pod labels: %v\n%s", err, out)
	}
	for k, v := range want {
		if live[k] != v {
			t.Errorf("live pod selector drift: role=%s expected %s=%s; got %s=%s",
				role, k, v, k, live[k])
		}
	}
}

// extractPodSelectorFromRender reads the
// helm-rendered chart for a role's
// NetworkPolicy podSelector.matchLabels.
func extractPodSelectorFromRender(rendered, role string) map[string]string {
	docs := splitYAMLDocs(rendered)
	for _, doc := range docs {
		var obj map[string]interface{}
		if err := yamlUnmarshal([]byte(doc), &obj); err != nil {
			continue
		}
		if obj["kind"] != "NetworkPolicy" {
			continue
		}
		spec, ok := obj["spec"].(map[string]interface{})
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
			out := make(map[string]string, len(ml))
			for k, v := range ml {
				if s, ok := v.(string); ok {
					out[k] = s
				}
			}
			return out
		}
	}
	return nil
}

// truncate caps a multi-line string at the
// first N bytes and adds a "..." marker. Used
// in failure logs so we don't dump megabytes.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...[truncated]"
}

// _ = context ensures go vet keeps the import
// even if a future commit removes out-of-band
// kubectl calls.
var _ = context.Background
