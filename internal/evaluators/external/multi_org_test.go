package external

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/observability"
)

const orgScopedManifest = `apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: PLUGIN_NAME
spec:
  service:
    type: langfuse
    endpoint: https://ENDPOINT_HOST
    auth:
      secretRef: langfuse-creds
      keyRef: public_key|secret_key
  send:
    trigger: on_trace
    sampling: 1
  collect:
    mode: webhook
    interval: 60s
  timeout: 30s
`

// TestEvaluateStaysInsideTenantBoundary is the end-to-end guard on the
// leak: the registry is process-wide, so a trace belonging to one org
// must not be transmitted to a plugin another org installed. Sampling
// is 1 so every enabled plugin would otherwise be contacted.
func TestEvaluateStaysInsideTenantBoundary(t *testing.T) {
	reg := evalplugin.NewRegistry()
	reg.Merge([]evalplugin.Record{
		{
			Plugin:  pluginFor(t, "acme-judge", "acme.example"),
			Source:  evalplugin.Source{Kind: evalplugin.SourceDatabase},
			Enabled: true,
			OrgID:   "acme",
		},
		{
			Plugin:  pluginFor(t, "globex-judge", "globex.example"),
			Source:  evalplugin.Source{Kind: evalplugin.SourceDatabase},
			Enabled: true,
			OrgID:   "globex",
		},
		{
			Plugin:  pluginFor(t, "shared-judge", "shared.example"),
			Source:  evalplugin.Source{Kind: evalplugin.SourceHelm},
			Enabled: true,
		},
	})

	var mu sync.Mutex
	var contacted []string
	d := NewDispatcher(reg, nil)
	d.Register(evalplugin.ServiceLangfuse, func(_ context.Context, endpoint string, _ map[string]any) error {
		mu.Lock()
		defer mu.Unlock()
		contacted = append(contacted, endpoint)
		return nil
	})

	m := NewMultiEvaluator(reg, d)
	if _, err := m.Evaluate(context.Background(), observability.Trace{OrgID: "acme"}); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), contacted...)
	mu.Unlock()

	for _, endpoint := range got {
		if endpoint == "https://globex.example" {
			t.Fatalf("acme's trace was sent to globex's vendor endpoint: %v", got)
		}
	}
	// The org's own plugin plus the cluster-wide one it inherits.
	if len(got) != 2 {
		t.Fatalf("expected 2 endpoints (own + cluster-wide), got %v", got)
	}
}

func pluginFor(t *testing.T, name, host string) *evalplugin.Plugin {
	t.Helper()
	raw := strings.Replace(orgScopedManifest, "PLUGIN_NAME", name, 1)
	raw = strings.Replace(raw, "ENDPOINT_HOST", host, 1)
	p, err := evalplugin.Decode([]byte(raw))
	if err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return p
}
