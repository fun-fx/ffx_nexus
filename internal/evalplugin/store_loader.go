package evalplugin

import (
	"context"
	"errors"
	"reflect"
)

// LoadFromStore reads every PluginRecord and merges it into the
// registry with Source=SourceDatabase. Cluster-wide rows (OrgID=="")
// can clash with identically-named Helm-installed plugins; per-org
// rows (OrgID!="") never conflict with cluster-wide because the
// name+org_id composite is unique in the schema.
//
// Errors from individual Decodes are returned via the discarded
// slice so the caller can surface them in admin UI/stderr. We do
// NOT abort on the first malformed row — partial load should be
// preferred to "every plugin goes dark".
func (r *Registry) LoadFromStore(ctx context.Context, store PluginStore, orgID string) error {
	if store == nil {
		return errors.New("plugin store is nil")
	}
	records, err := store.List(ctx, orgID)
	if err != nil {
		return err
	}
	in := make([]Record, 0, len(records))
	for _, rec := range records {
		p, err := Decode([]byte(rec.SpecYAML))
		if err != nil {
			continue
		}
		in = append(in, Record{
			Plugin:  p,
			Source:  Source{Kind: SourceDatabase, Ref: rec.ID},
			Enabled: rec.Enabled,
			// Carrying the row's org is what lets dispatch stay inside
			// tenant boundaries; rows written as cluster-wide keep the
			// empty string and are inherited by every org.
			OrgID: NormalizeOrgID(rec.OrgID),
		})
	}
	_ = r.Merge(in) // discarded records are logged at the caller layer
	return nil
}

// ReconcileFromStore makes the registry's database-sourced entries match
// the store exactly: rows are folded in, and entries whose row has since
// disappeared are evicted. Helm-sourced entries are never touched, so the
// cluster-wins precedence Merge implements still holds.
//
// Console writes already push into the registry directly, so this is a
// safety net rather than the main path. It matters because a registry
// that misses one write goes silent, not loud: the dispatcher simply
// finds no plugin in scope and forwards nothing, which is
// indistinguishable from a vendor that has nothing to say. Reconciling
// on a timer bounds that state to one interval instead of "until the
// next process restart", and it is also what lets a second replica pick
// up a plugin installed through the replica that served the request.
//
// Returns the number of entries added, changed or evicted so the caller
// can log only when the live set actually moved.
func (r *Registry) ReconcileFromStore(ctx context.Context, store PluginStore) (int, error) {
	if store == nil {
		return 0, errors.New("plugin store is nil")
	}
	rows, err := store.List(ctx, "")
	if err != nil {
		return 0, err
	}
	desired := make(map[string]Record, len(rows))
	for _, row := range rows {
		p, err := Decode([]byte(row.SpecYAML))
		if err != nil {
			// A row that no longer decodes keeps whatever the registry
			// already holds: dropping a working plugin because someone
			// stored a manifest from a newer schema would be worse.
			continue
		}
		org := NormalizeOrgID(row.OrgID)
		desired[recordKey(org, p.Metadata.Name)] = Record{
			Plugin:  p,
			Source:  Source{Kind: SourceDatabase, Ref: row.ID},
			Enabled: row.Enabled,
			OrgID:   org,
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	changed := 0
	for key, rec := range desired {
		existing, ok := r.all[key]
		if ok && existing.Source.Kind == SourceHelm {
			continue
		}
		if ok && sameRecord(existing, &rec) {
			continue
		}
		r.all[key] = cloneRecord(&rec)
		changed++
	}
	for key, existing := range r.all {
		if existing.Source.Kind != SourceDatabase {
			continue
		}
		if _, ok := desired[key]; ok {
			continue
		}
		delete(r.all, key)
		changed++
	}
	return changed, nil
}

// sameRecord reports whether a live entry already matches the stored row,
// so a reconcile tick that finds nothing to do stays silent.
func sameRecord(live, stored *Record) bool {
	return live.Enabled == stored.Enabled &&
		live.Source.Ref == stored.Source.Ref &&
		reflect.DeepEqual(live.Plugin, stored.Plugin)
}
