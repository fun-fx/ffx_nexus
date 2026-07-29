package external

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/evalplugin"
)

const pollManifest = `apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: PLUGIN_NAME
spec:
  service:
    type: langfuse
    endpoint: https://cloud.langfuse.com
    auth:
      secretRef: langfuse-creds
      keyRef: public_key|secret_key
  send:
    trigger: on_trace
    sampling: 1
    payload:
      input: "{{ .trace.RequestModel }}"
  collect:
    mode: poll
    interval: 10ms
    mapping:
      name: name
      score: value
      trace_id: traceId
`

func pollPlugin(t *testing.T, name string) *evalplugin.Plugin {
	t.Helper()
	p, err := evalplugin.Decode([]byte(strings.Replace(pollManifest, "PLUGIN_NAME", name, 1)))
	if err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return p
}

// A plugin installed through the console lands in the registry after
// Run() has already started. A one-shot scan meant it never polled
// until the pod restarted.
func TestCollectorStartsPollerForPluginAddedAfterRun(t *testing.T) {
	reg := evalplugin.NewRegistry()
	c := NewCollector(reg, nil, nil)
	c.SetSecretResolver(stubResolver{creds: Credentials{Values: []string{"pk", "sk"}}})

	var mu sync.Mutex
	polled := map[string]int{}
	c.Register(evalplugin.ServiceLangfuse, func(_ context.Context, tgt Target) ([]json.RawMessage, error) {
		mu.Lock()
		defer mu.Unlock()
		polled[tgt.PluginName()]++
		return nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	// Installed after Run started, exactly like a console create.
	reg.Merge([]evalplugin.Record{{
		Plugin:  pollPlugin(t, "langfuse-judge"),
		Source:  evalplugin.Source{Kind: evalplugin.SourceDatabase},
		Enabled: true,
	}})
	c.syncPollers(ctx)

	if !waitFor(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return polled["langfuse-judge"] > 0
	}) {
		t.Fatal("plugin created after Run never polled")
	}
}

// Deleting a plugin must stop its poller, otherwise Nexus keeps calling
// a vendor for a plugin the operator removed.
func TestCollectorStopsPollerWhenPluginRemoved(t *testing.T) {
	reg := evalplugin.NewRegistry()
	reg.Merge([]evalplugin.Record{{
		Plugin:  pollPlugin(t, "langfuse-judge"),
		Source:  evalplugin.Source{Kind: evalplugin.SourceDatabase},
		Enabled: true,
	}})
	c := NewCollector(reg, nil, nil)
	c.SetSecretResolver(stubResolver{creds: Credentials{Values: []string{"pk", "sk"}}})

	var mu sync.Mutex
	calls := 0
	c.Register(evalplugin.ServiceLangfuse, func(_ context.Context, tgt Target) ([]json.RawMessage, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.syncPollers(ctx)
	if !waitFor(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls > 0
	}) {
		t.Fatal("poller never ran")
	}

	reg.Remove("", "langfuse-judge")
	c.syncPollers(ctx)

	mu.Lock()
	baseline := calls
	mu.Unlock()
	// The interval is 10ms, so several ticks would have fired by now if
	// the goroutine were still alive.
	time.Sleep(120 * time.Millisecond)
	mu.Lock()
	after := calls
	mu.Unlock()
	if after != baseline {
		t.Errorf("poller kept running after removal: %d -> %d", baseline, after)
	}
}

func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}
