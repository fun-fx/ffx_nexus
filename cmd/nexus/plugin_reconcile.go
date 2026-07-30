package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/ffxnexus/nexus/internal/evalplugin"
)

// pluginReconcileInterval is how often the live plugin registry is
// re-derived from the database. One minute is short enough that an
// operator who installs a plugin and then checks the vendor dashboard
// sees traces on the next call, and long enough that the query is
// invisible next to gateway traffic.
const pluginReconcileInterval = time.Minute

// runPluginRegistryReconcile keeps the in-memory registry in step with
// the stored plugin rows for as long as ctx lives.
//
// The registry is written from two directions — boot-time load and admin
// writes — and a miss on either side fails silently: the dispatcher finds
// nothing in scope and forwards nothing, which looks exactly like a
// vendor with no results. This loop is the backstop that turns "dark
// until the next restart" into "repaired within one interval", and it is
// what makes a console write on one replica visible to the others.
func runPluginRegistryReconcile(
	ctx context.Context,
	reg *evalplugin.Registry,
	store evalplugin.PluginStore,
	interval time.Duration,
	log *slog.Logger,
) {
	if reg == nil || store == nil || interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			changed, err := reg.ReconcileFromStore(ctx, store)
			if err != nil {
				if ctx.Err() == nil && log != nil {
					log.Warn("eval plugin registry reconcile failed", "err", err)
				}
				continue
			}
			if changed > 0 && log != nil {
				// Only a drift repair reaches this line, so it is worth
				// an INFO: it names the moment a plugin started or
				// stopped receiving traces without an admin write.
				log.Info("eval plugin registry reconciled from database",
					"entries_changed", changed,
					"live_enabled", len(reg.Enabled()))
			}
		}
	}
}
