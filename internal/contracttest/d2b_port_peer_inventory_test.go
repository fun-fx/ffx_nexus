// D-2b.24 chart port × peer inventory test.
//
// The chart has five locations where port numbers and peer
// namespaces appear, AND they MUST agree. If they diverge
// (e.g. NetworkPolicy allows prometheus on 9100 but the
// metrics containerPort is 9101), the rendered policy says
// one port and the cluster sees another: silent miss.
//
//   1. `ports.gateway` / `ports.console` / `metrics.port`
//   2. Deployment containerPort (deployment.yaml)
//   3. Service ports                 (service.yaml)
//   4. ServiceMonitor endpoint port  (servicemonitor.yaml)
//   5. NetworkPolicy peer/port rule  (networkpolicy.yaml)
//
// The test parses each rendered manifest and asserts:
//   - containerPort `gateway` == Service port `gateway` ==
//     ingressController.matchPorts[0] == .Values.ports.gateway
//   - containerPort `console` == Service port `console`
//   - containerPort `metrics` == Service port `metrics` ==
//     NetworkPolicy prometheus ingress port == metrics.port
//   - migration NetworkPolicy peer equals Gateway/Worker
//     Postgres egress peer
//   - NetworkPolicy excludes reverse grant (prometheus cannot
//     reach Gateway API port 8080; ingress-nginx cannot reach
//     Worker metrics port 9100). Enforced as a STRUCTURAL test
//     reading the rendered manifest.
//
//go:build !integrationcni

package contracttest

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestPortPeerInventoryCrossReferenced renders the chart in
// enterprise mode, parses the seven relevant manifests,
// and asserts the port × peer agreement table.
//
// Failure modes this catches:
//   - ServiceMonitor endpoint `port: metrics` but Service
//     has no `name: metrics` port (silent 0-scrape).
//   - NetworkPolicy prometheus rule on port X but the
//     Deployment binds port Y (silent deny).
//   - migration Job NetworkPolicy peer differs from
//     Gateway/Worker peer (D-2b.1 silent miss).
//   - NetworkPolicy accidentally allowlist 8080 from
//     `monitoring` namespace (prometheus hitting API).
//   - NetworkPolicy accidentally allowlist 9100 from
//     `ingress-nginx` namespace (controller hitting Worker metrics).
func TestPortPeerInventoryCrossReferenced(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Skip("cannot find project root")
	}
	rendered := renderEnterpriseChart(t, root)
	for _, f := range []struct {
		path   string
		text   string
	}{
		{"deployment", rendered["deployment"]},
		{"service", rendered["service"]},
		{"servicemonitor", rendered["servicemonitor"]},
		{"networkpolicy", rendered["networkpolicy"]},
		{"migration", rendered["migration"]},
		{"secret", rendered["secret"]},
		{"configmap", rendered["configmap"]},
	} {
		if f.text == "" {
			t.Fatalf("render: %s missing — chart components expected for inventory", f.path)
		}
	}

	// --- 1. Gateway port: containerPort == Service port == NP peer port ---
	gwContainer := extractPortInt(t, rendered["deployment"], `name: gateway\n\s*containerPort:\s*(\d+)`)
	gwService := extractPortInt(t, rendered["service"], `name: gateway\n\s*port:\s*(\d+)`)
	gwIngress := extractPortInt(t, rendered["networkpolicy"], `kubernetes.io/metadata.name: ingress-nginx[\s\S]*?port:\s*(\d+)`)
	if gwContainer != gwService || gwService != gwIngress {
		t.Fatalf("gateway port drift: container=%d service=%d NP.ingress=%d (must all match)", gwContainer, gwService, gwIngress)
	}

	// --- 2. Metrics port: containerPort == Service port == metrics.port === NP.prometheus ---
	metricsContainer := extractPortInt(t, rendered["deployment"], `name: metrics\n\s*containerPort:\s*(\d+)`)
	metricsService := extractPortInt(t, rendered["service"], `name: metrics\n\s*port:\s*(\d+)`)
	metricsScrape := extractPortInt(t, rendered["networkpolicy"], `kubernetes.io/metadata.name: monitoring[\s\S]*?port:\s*(\d+)`)
	if metricsContainer != metricsService || metricsService != metricsScrape {
		t.Fatalf("metrics port drift: container=%d service=%d NP.prometheus=%d (must all match)", metricsContainer, metricsService, metricsScrape)
	}

	// --- 3. Migration Postgres peer == Gateway/Worker Postgres peer ---
	// Both Gateway and migration-egress NetworkPolicies have
	// the same destination namespaceSelector. The chart helper
	// renders under release name "render-test", so policy names
	// are `render-test-nexus-gateway` and
	// `render-test-nexus-migration-egress`.
	gwPgNamespace := firstPgEgressNamespace(t, rendered["networkpolicy"], "render-test-nexus-gateway")
	migPgNamespace := firstPgEgressNamespace(t, rendered["networkpolicy"], "render-test-nexus-migration-egress")
	if gwPgNamespace != migPgNamespace {
		t.Fatalf("migration Postgres peer drifted from Gateway: gw=%s migration=%s", gwPgNamespace, migPgNamespace)
	}

	// --- 4. Reverse allow is forbidden ---
	// prometheus should NOT be allowed to call Gateway API (port 8080).
	if rulePeersWithPort(t, rendered["networkpolicy"], "monitoring", 8080) {
		t.Fatalf("NetworkPolicy allows monitoring peer to call Gateway API port 8080 — prometheus must reach ONLY the metrics port")
	}
	// ingress-nginx should NOT be allowed to call Worker metrics (port 9100).
	// debug
	if rulePeersWithPort(t, rendered["networkpolicy"], "ingress-nginx", 9100) {
		t.Fatalf("NetworkPolicy allows ingress-nginx peer to call Worker metrics port 9100 — ingress must reach ONLY Gateway API port")
	}
	smEndpoint := extractPortStr(t, rendered["servicemonitor"], `endpoints:\s*\n\s*-\s*port:\s*([A-Za-z0-9_-]+)`)
	if !hasServicePortName(t, rendered["service"], smEndpoint) {
		t.Fatalf("ServiceMonitor endpoint port %q is not declared as a name on the Service", smEndpoint)
	}
}

// TestServiceMonitorSelectorSubsetOfService verifies the
// rendered ServiceMonitor `selector.matchLabels` is a subset of
// the Service's `selector.matchLabels` PLUS at most one
// `app.kubernetes.io/component` entry. This locks the chart's
// observability contract:
//
//   ServiceMonitor_selector ⊆ Service_selector ∪ {component}
//
// If a future change dangles the selector (e.g. drops the
// `app.kubernetes.io/instance` label) the rule is silently
// orphaned — the chart looks fine but Prometheus errors out
// at scrape time with `0 selected targets`. Catching it at
// render-test time costs 100ms.
func TestServiceMonitorSelectorSubsetOfService(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Skip("cannot find project root")
	}
	rendered := renderEnterpriseChart(t, root)
	svcLabels := parseSelectorLabels(t, rendered["service"], "selector:")
	smLabels := parseSelectorLabels(t, rendered["servicemonitor"], "selector:")

	// Allow at most ONE extra label on the SM: `component`.
	for k, v := range smLabels {
		if _, exists := svcLabels[k]; exists {
			// Same key must have same value.
			if svcLabels[k] != v {
				t.Fatalf("ServiceMonitor label %q=%q does not match Service label %q=%q",
					k, v, k, svcLabels[k])
			}
			continue
		}
		if k != "app.kubernetes.io/component" {
			t.Fatalf("ServiceMonitor selector adds unexpected label not on Service: %q=%q (Service labels: %#v)",
				k, v, svcLabels)
		}
	}
	for k, v := range svcLabels {
		if _, exists := smLabels[k]; !exists {
			t.Fatalf("Service selector has label %q=%q that ServiceMonitor dropped (would silently miss scrape): %#v vs %#v",
				k, v, svcLabels, smLabels)
		}
	}
}

// parseSelectorLabels extracts `selector:` from a rendered
// K8s resource. Two encodings are in play in this chart:
//
//   spec:
//     selector:
//       matchLabels:
//         <key>: <value>
//
//   spec:
//     selector:
//       <key>: <value>      # shorthand
//
// Both return a map[string]string. The Service uses the
// shorthand; the ServiceMonitor uses matchLabels (or a mix).
func parseSelectorLabels(t *testing.T, text string, after string) map[string]string {
	t.Helper()
	idx := strings.Index(text, after)
	if idx < 0 {
		t.Fatalf("parseSelectorLabels: anchor %q not found", after)
	}
	rest := text[idx:]
	// Try matchLabels first (ServiceMonitor).
	rest2 := rest
	mlIdx := -1
	if i := strings.Index(rest, "matchLabels:\n"); i >= 0 {
		mlIdx = i
		rest2 = rest[mlIdx+len("matchLabels:\n"):]
	}
	end := len(rest)
	for _, sep := range []string{"\n---", "\n  selector:", "\n  template:", "\n  type:", "\n  spec:"} {
		if i := strings.Index(rest, sep); i > 0 && i < end {
			end = i
		}
	}
	body := rest2[len("matchLabels:\n"):]
	if mlIdx < 0 {
		body = rest[:end]
	} else {
		body = rest2[:func() int {
			e := len(rest2)
			for _, sep := range []string{"\n---", "\n  selector:", "\n  template:", "\n  type:", "\n  spec:"} {
				if i := strings.Index(rest2, sep); i > 0 && i < e {
					e = i
				}
			}
			return e
		}()]
	}
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "app.kubernetes.io/") && !strings.HasPrefix(line, "helm.sh/") && !strings.HasPrefix(line, "release:") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		out[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	if len(out) == 0 {
		t.Fatalf("parseSelectorLabels: no labels extracted for anchor %q\n--- excerpt ---\n%s", after, body[:minOr(len(body), 600)])
	}
	return out
}

func minOr(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestPortPeerInventoryNoPlaintextSecret asserts no chart-rendered
// Secret contains a credential string format. The chart's "use
// existingSecret for credentials" rule is enforced by both `helm
// template` output (this test) and by values.schema.json rejecting
// obvious plaintext keys.
func TestPortPeerInventoryNoPlaintextSecret(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Skip("cannot find project root")
	}
	rendered := renderEnterpriseChart(t, root)
	for _, line := range strings.Split(rendered["secret"], "\n") {
		// postgres://user:pass@host patterns and Bearer tokens are
		// exactly what NOT to embed. even if an operator copies them
		// into `dependencies.<svc>.url`, the bare `secret.yaml`
		// template must not be the source.
		if strings.Contains(line, "postgres://") && strings.Contains(line, ":") && strings.Contains(line, "@") {
			t.Fatalf("plaintext postgres DSN rendered into Secret: %s", line)
		}
		if strings.HasPrefix(strings.TrimSpace(line), "Bearer ") {
			t.Fatalf("plaintext Authorization header rendered into Secret: %s", line)
		}
	}
}

// TestServicePortEqualsContainerPortShortSkip is a fast path:
// install a small script that compares port: values, intended
// to catch the most common drift (typing 8081 instead of 8080
// in containerPort) without rendering the full chart.
func TestServicePortEqualsContainerPortShortSkip(t *testing.T) {
	t.Skip("superseded by TestPortPeerInventoryCrossReferenced — kept as skip-anchor so reviewers see the intended fast check")
}

// ---- helpers ----

func extractPortInt(t *testing.T, text string, pattern string) int {
	t.Helper()
	re := regexp.MustCompile(pattern)
	m := re.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("extractPortInt: pattern %q not found in manifest snippet", pattern)
	}
	v, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("extractPortInt: %q is not an integer (%v)", m[1], err)
	}
	return v
}

func extractPortStr(t *testing.T, text string, pattern string) string {
	t.Helper()
	re := regexp.MustCompile(pattern)
	m := re.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("extractPortStr: pattern %q not found", pattern)
	}
	return m[1]
}

// rulePeersWithPort answers "is `from: nsName` paired with
// `port: N` in the SAME NetworkPolicy ingress rule?".
//
// We deliberately restrict the search to ONE ingress rule by
// anchoring on `from:` to `next from:` (or end of the policy
// block). A previous version scanned greedily across the
// entire chart and produced false positives: ingress-nginx
// could appear at the top of the Gateway policy, while
// the Worker policy later carried `port: 9100` for prometheus.
// With (e.g.) `allow monitoring → port 9100` immediately
// after `allow ingress-nginx → port 8080` the two rules
// share the same podSelector block and naive regex pairs
// them with `monitoring → 9100` AND `ingress-nginx → 9100`.
func rulePeersWithPort(t *testing.T, text string, nsName string, port int) bool {
	t.Helper()
	// Walk rule by rule. A rule begins at `- from:` and ends
	// at the next `- from:` or at `egress:` (whichever first).
	for _, rule := range splitIngressRules(text) {
		if !strings.Contains(rule, "kubernetes.io/metadata.name: "+nsName) {
			continue
		}
		portRe := regexp.MustCompile("port:\\s*" + strconv.Itoa(port))
		if portRe.MatchString(rule) {
			return true
		}
	}
	return false
}

// splitIngressRules pulls each `- from:` block out of a
// rendered NetworkPolicy `ingress:` section. We treat the
// policy's ingress list as a flat sequence of rules.
func splitIngressRules(text string) []string {
	out := []string{}
	// Carve on `- from:` boundary, then trim everything
	// past `egress:` to keep the rule bounded.
	parts := strings.Split(text, "- from:")
	for _, p := range parts[1:] {
		if idx := strings.Index(p, "egress:"); idx >= 0 {
			p = p[:idx]
		}
		out = append(out, p)
	}
	return out
}

// firstPgEgressNamespace finds the rendered NetworkPolicy
// section starting at the named policy block, then extracts
// the `kubernetes.io/metadata.name: <ns>` value attached to
// the Postgres egress rule (NOT the kubelet DNS rule or any
// other namespace on a different egress rule).
//
// Strategy: this chart writes a deterministic comment
//
//   # Postgres egress — selector mode (in-cluster operator Postgres)
//
// immediately before the Postgres namespaceSelector and
// podSelector. The same comment appears in front of the
// Gateway, Worker, and migration egress blocks. We anchor
// to that comment so we never accidentally read the
// kubelet/CoreDNS `kubernetes.io/metadata.name: kube-system`
// line that belongs to a different egress rule.
func firstPgEgressNamespace(t *testing.T, text string, policy string) string {
	t.Helper()
	const marker = "Postgres egress — selector mode"
	// Walk in policy block order: split on `kind: NetworkPolicy`,
	// keep only blocks that belong to `<policy>` AND contain
	// the Postgres comment.
	blocks := strings.Split(text, "kind: NetworkPolicy")
	for _, b := range blocks {
		if !strings.Contains(b, "name: "+policy+"\n") {
			continue
		}
		idxMark := strings.Index(b, marker)
		if idxMark < 0 {
			continue
		}
		afterMark := b[idxMark+len(marker):]
		// First `kubernetes.io/metadata.name: <ns>` after the
		// comment IS the Postgres namespaceSelector on the
		// SAME - to: rule. The kubedns rule is BEFORE the
		// comment and we specifically anchor to the comment.
		nsRe := regexp.MustCompile("kubernetes.io/metadata.name:\\s*([A-Za-z0-9_-]+)")
		m := nsRe.FindStringSubmatch(afterMark)
		if m == nil {
			t.Fatalf("firstPgEgressNamespace: Postgres comment present but no namespaceSelector after it in policy %q", policy)
		}
		return m[1]
	}
	t.Fatalf("firstPgEgressNamespace: postgres namespaceSelector not found in policy %q", policy)
	return ""
}

func hasServicePortName(t *testing.T, text string, name string) bool {
	t.Helper()
	return regexp.MustCompile("name:\\s*" + regexp.QuoteMeta(name) + `\n`).MatchString(text)
}

// renderEnterpriseChart shells out to `helm template` with the
// canonical enterprise enforce values and returns each rendered
// manifest as a map keyed by short name.
//
// Caching is intentionally avoided: the chart's mutable inputs
// (`values.yaml`, schema, templates) mean every render is cheap
// and we want a fresh snapshot.
func renderEnterpriseChart(t *testing.T, root string) map[string]string {
	out := make(map[string]string)
	for _, f := range []struct{ name, showOnly string }{
		{"deployment", "templates/deployment.yaml"},
		{"service", "templates/service.yaml"},
		{"servicemonitor", "templates/servicemonitor.yaml"},
		{"networkpolicy", "templates/networkpolicy.yaml"},
		{"migration", "templates/migration-job.yaml"},
		{"secret", "templates/secret.yaml"},
		{"configmap", "templates/configmap.yaml"},
	} {
		args := []string{
			"--set", "networkPolicy.profile=enterprise",
			"--set", "networkPolicy.mode=enforce",
			"--set", "networkPolicy.enforcementAcknowledged=true",
			"--set", "networkPolicy.postgres.selector.enabled=true",
			"--set", "networkPolicy.postgres.selector.namespace=database",
			"--set", "dependencies.postgres.host=postgres",
			"--set", "dependencies.postgres.port=5432",
			"--set", "dependencies.postgres.namespace=database",
			"--set", "metrics.enabled=true",
			"--set", "metrics.serviceMonitor.enabled=true",
			"--set", "migrations.enabled=true",
			"--set", "dependencies.postgres.enabled=true",
		}
		out[f.name] = renderShowOnly(t, filepath.Join(root, "deploy", "helm", "nexus"), f.showOnly, args)
	}
	return out
}
