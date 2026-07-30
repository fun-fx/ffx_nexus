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
	d.SetSecretResolver(stubResolver{creds: Credentials{Values: []string{"pk", "sk"}}})
	d.Register(evalplugin.ServiceLangfuse, func(_ context.Context, tgt Target, _ map[string]any) error {
		mu.Lock()
		defer mu.Unlock()
		contacted = append(contacted, tgt.Endpoint)
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

// TestEvaluateDispatchesClusterWidePluginToEveryOrg pins the scope that
// a console install now gets. A plugin stored without an explicit org is
// cluster-wide, so it has to fire for a trace whose org is the virtual
// key's real id. Stamping such a row with the console's "default"
// placeholder instead made it match no traffic at all, and because the
// dispatch loop simply found nothing to do, the failure was invisible:
// the plugin read as enabled, Test passed, and the vendor dashboard
// stayed empty.
func TestEvaluateDispatchesClusterWidePluginToEveryOrg(t *testing.T) {
	reg := evalplugin.NewRegistry()
	reg.Merge([]evalplugin.Record{{
		Plugin:  pluginFor(t, "langfuse-judge", "us.cloud.langfuse.com"),
		Source:  evalplugin.Source{Kind: evalplugin.SourceDatabase},
		Enabled: true,
		OrgID:   evalplugin.NormalizeOrgID("default"),
	}})

	var mu sync.Mutex
	var contacted []string
	d := NewDispatcher(reg, nil)
	d.SetSecretResolver(stubResolver{creds: Credentials{Values: []string{"pk", "sk"}}})
	d.Register(evalplugin.ServiceLangfuse, func(_ context.Context, tgt Target, _ map[string]any) error {
		mu.Lock()
		defer mu.Unlock()
		contacted = append(contacted, tgt.Endpoint)
		return nil
	})

	m := NewMultiEvaluator(reg, d)
	// An org id shaped like the one a virtual key carries in production.
	trace := observability.Trace{OrgID: "3f9c1e6a-0f21-4c1e-9b7a-1d2e3f4a5b6c"}
	if _, err := m.Evaluate(context.Background(), trace); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), contacted...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "https://us.cloud.langfuse.com" {
		t.Fatalf("cluster-wide plugin did not receive the trace, contacted=%v", got)
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
