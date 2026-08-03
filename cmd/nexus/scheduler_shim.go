package main

import (
	"context"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evaluators/external"
)

// schedulerShim adapts the *external.Scheduler to the
// console.PluginManualFirer interface. The scheduler can't index
// plugins by metadata.name on its own — its internal maps only
// carry plugins that registered a flush goroutine, which is exactly
// the set that needs `scheduled`, not `manual`. Bridging here keeps
// the scheduler small and keeps the FireManual contract explicit at
// the admin REST boundary.
type schedulerShim struct {
	s   *external.Scheduler
	reg *evalplugin.Registry
}

// FireManual resolves the plugin by metadata.name (or returns a
// nil plugin on miss) and hands the request to the underlying
// scheduler. The unknown-plugin case maps to (0, nil) — the admin
// REST handler renders this as "no traces collected yet" rather
// than a 5xx, which has been a common operator-mistake path
// (typo'd plugin names).
func (s *schedulerShim) FireManual(ctx context.Context, name, trigger string) (int, error) {
	plugin := pluginByName(s.reg, name)
	if plugin == nil {
		return 0, nil
	}
	return s.s.FireManual(ctx, plugin, trigger)
}

// FireScheduled mirrors FireManual but drains a scheduled-trigger
// plugin's buffer immediately. The same nil-plugin-no-error policy
// applies so the REST handler can render the count as 0 rather than
// surfacing a "plugin not found" error to the operator who mistyped
// the name from the chip click.
func (s *schedulerShim) FireScheduled(ctx context.Context, name, trigger string) (int, error) {
	plugin := pluginByName(s.reg, name)
	if plugin == nil {
		return 0, nil
	}
	return s.s.FireScheduled(ctx, plugin, trigger)
}

// pluginByName returns the *evalplugin.Plugin for metadata.name or
// nil. Disabled plugins are skipped — the operator who'd be happy
// to see the answer "no, your toggled-off plugin isn't firing" is
// well served by the registry returning nil here without a typed
// error, which is what the shim relies on to render (0, nil).
func pluginByName(reg *evalplugin.Registry, name string) *evalplugin.Plugin {
	for _, rec := range reg.Enabled() {
		if rec.Plugin.Metadata.Name == name {
			return rec.Plugin
		}
	}
	return nil
}
