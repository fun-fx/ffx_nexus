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
	if err := a.store.Save(ctx, r); err != nil {
		return err
	}
	a.mergeIntoRegistry(r)
	return nil
}

func (a pluginSourceAdapter) Delete(ctx context.Context, id string) error {
	if a.store == nil {
		return nil
	}
	// Read the row before deleting it: afterwards there is no way to
	// learn which (org, name) entry to evict, and an entry left behind
	// would keep receiving traces for a plugin the operator removed.
	orgID, name := a.identify(ctx, id)
	if err := a.store.Delete(ctx, id); err != nil {
		return err
	}
	if a.reg != nil && name != "" {
		a.reg.Remove(orgID, name)
	}
	return nil
}

// mergeIntoRegistry folds a just-written row into the live registry so
// the dispatcher picks the plugin up on the next trace rather than at
// the next pod restart. Without this, creating a plugin in the console
// left it inert — and pressing Test reported it as missing — because
// the registry was only ever filled at boot.
//
// Merge is keyed by (org, name), so this replaces the previous revision
// of the same plugin and cannot disturb another tenant's entry.
func (a pluginSourceAdapter) mergeIntoRegistry(r *evalplugin.PluginRecord) {
	if a.reg == nil || r == nil {
		return
	}
	p, err := evalplugin.Decode([]byte(r.SpecYAML))
	if err != nil {
		// Handlers validate the manifest before Save, so a decode
		// failure here means the row predates validation. Leave the
		// registry untouched rather than dropping a good entry.
		return
	}
	_ = a.reg.Merge([]evalplugin.Record{{
		Plugin:  p,
		Source:  evalplugin.Source{Kind: evalplugin.SourceDatabase, Ref: r.ID},
		Enabled: r.Enabled,
		OrgID:   r.OrgID,
	}})
}

// identify resolves the registry address of a stored row. The registry
// keys on metadata.name from the manifest, which is authoritative, so
// we prefer it over the row's name column and fall back only when the
// manifest cannot be decoded.
func (a pluginSourceAdapter) identify(ctx context.Context, id string) (orgID, name string) {
	rec, err := a.store.Get(ctx, id)
	if err != nil || rec == nil {
		return "", ""
	}
	if p, err := evalplugin.Decode([]byte(rec.SpecYAML)); err == nil && p != nil {
		return rec.OrgID, p.Metadata.Name
	}
	return rec.OrgID, rec.Name
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
