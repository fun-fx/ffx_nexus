package main

import (
	"context"
	"errors"
	"net/http"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evaluators/external"
)

// PluginTester is the admin-REST "test send" backend. One Tester
// covers every installed plugin — we pick the appropriate vendor
// probe by routing on the plugin's service.type. A missing adapter
// yields a 502 SDK-unavailable error; Admins see the same message
// in the UI so they know a real vendor integration is the next
// step.
type PluginTester struct {
	reg *evalplugin.Registry
	d   *external.Dispatcher
	c   *external.Collector
}

// Test implements console.EvalPluginTester. The signature is
// deliberately narrow (just the plugin name) so the route can be
// reused regardless of which source (Helm or DB) the plugin came
// from. We pull the plugin record from the registry, then dispatch
// a service-type-specific probe.
func (t *PluginTester) Test(name string) error {
	if t == nil || t.reg == nil {
		return errors.New("plugin registry not initialised")
	}
	rec, ok := t.reg.Lookup(name)
	if !ok {
		return errors.New("plugin not found")
	}
	switch rec.Plugin.Spec.Service.Type {
	case evalplugin.ServiceLangSmith:
		return PingLangsmith(context.Background(), rec.Plugin.Spec.Service.Endpoint)
	default:
		// Generic fallback: HTTP HEAD against the endpoint so the
		// admin sees *something* for plugins we haven't profiled
		// specifically. Cheap and harmless.
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
			rec.Plugin.Spec.Service.Endpoint, nil)
		if err != nil {
			return err
		}
		resp, err := httpClientForPlugins().Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 500 {
			return errors.New("endpoint returned server error")
		}
		return nil
	}
}

func newTester(reg *evalplugin.Registry,
	d *external.Dispatcher,
	c *external.Collector,
) *PluginTester {
	return &PluginTester{reg: reg, d: d, c: c}
}
