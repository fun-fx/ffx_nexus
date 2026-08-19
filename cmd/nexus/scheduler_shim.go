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
func (s *schedulerShim) FireManual(ctx context.Context, orgID, name, trigger string) (int, error) {
	plugin := pluginByNameForOrg(s.reg, orgID, name)
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
func (s *schedulerShim) FireScheduled(ctx context.Context, orgID, name, trigger string) (int, error) {
	plugin := pluginByNameForOrg(s.reg, orgID, name)
	if plugin == nil {
		return 0, nil
	}
	return s.s.FireScheduled(ctx, plugin, trigger)
}

// pluginByNameForOrg returns the *evalplugin.Plugin one org may fire under that
// name, or nil. Disabled plugins are skipped — the operator who'd be happy to
// see the answer "no, your toggled-off plugin isn't firing" is well served by
// the registry returning nil here without a typed error, which is what the shim
// relies on to render (0, nil).
//
// This previously scanned reg.Enabled(), which spans every tenant and returned
// the first name match. Firing dispatches the plugin's buffered traces to its
// configured vendor, so that scan let an admin of one org flush traffic into
// another org's vendor account. EnabledForOrg is the resolution the registry
// documents as the only correct one for dispatch: the caller's own rows, plus
// the cluster-wide rows they inherit, with their own shadowing the inherited.
func pluginByNameForOrg(reg *evalplugin.Registry, orgID, name string) *evalplugin.Plugin {
	for _, rec := range reg.EnabledForOrg(evalplugin.NormalizeOrgID(orgID)) {
		if rec.Plugin != nil && rec.Plugin.Metadata.Name == name {
			return rec.Plugin
		}
	}
	return nil
}
