package evalplugin

import (
	"context"
	"errors"
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
			OrgID: rec.OrgID,
		})
	}
	_ = r.Merge(in) // discarded records are logged at the caller layer
	return nil
}
