package main

import (
	"context"

	"github.com/ffxnexus/nexus/internal/console"
	"github.com/ffxnexus/nexus/internal/evalplugin"
)

// pluginSourceAdapter bridges the merge-aware Registry with the
// console.EvalPluginSource interface so admin REST routes can use
// the same record shape the loader reads at boot.
type pluginSourceAdapter struct {
	reg   *evalplugin.Registry
	store evalplugin.PluginStore
}

func (a pluginSourceAdapter) List(ctx context.Context, orgID string) ([]evalplugin.PluginRecord, error) {
	if a.store != nil {
		return a.store.List(ctx, orgID)
	}
	return nil, nil
}

func (a pluginSourceAdapter) Get(ctx context.Context, id string) (*evalplugin.PluginRecord, error) {
	if a.store != nil {
		return a.store.Get(ctx, id)
	}
	return nil, evalplugin.ErrPluginNotFound
}

func (a pluginSourceAdapter) Save(ctx context.Context, r *evalplugin.PluginRecord) error {
	if a.store == nil {
		return nil
	}
	return a.store.Save(ctx, r)
}

func (a pluginSourceAdapter) Delete(ctx context.Context, id string) error {
	if a.store == nil {
		return nil
	}
	return a.store.Delete(ctx, id)
}

// Lookup resolves a plugin by metadata.name. The registry is the
// canonical source because it has already merged Helm + DB records;
// the DB store is consulted as a fallback so per-org rows that
// haven't yet been merged still resolve.
func (a pluginSourceAdapter) Lookup(ctx context.Context, name string) (*evalplugin.PluginRecord, error) {
	if a.reg != nil {
		if rec, ok := a.reg.Lookup(name); ok {
			return &evalplugin.PluginRecord{
				Name:     rec.Plugin.Metadata.Name,
				SpecYAML: pluginToYAML(rec.Plugin),
				Enabled:  rec.Enabled,
			}, nil
		}
	}
	if a.store != nil {
		all, err := a.store.List(ctx, "")
		if err == nil {
			for _, rec := range all {
				if rec.Name == name {
					return &rec, nil
				}
			}
		}
	}
	return nil, evalplugin.ErrPluginNotFound
}

// compile-time assertion that pluginSourceAdapter satisfies the
// console interface.
var _ console.EvalPluginSource = pluginSourceAdapter{}
