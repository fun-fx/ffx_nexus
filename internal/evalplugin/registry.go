package evalplugin

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// SourceKind tags where a plugin record came in from. The merge logic
// in Merge consumes this to flag conflicts (cluster-wide wins when the
// same name is loaded from both Helm and per-org DB).
type SourceKind string

const (
	SourceHelm     SourceKind = "helm"
	SourceDatabase SourceKind = "database"
)

// Source is the provenance pair used by the admin REST responses so
// operators can tell at a glance whether a plugin came from a
// ConfigMap or from the DB.
type Source struct {
	Kind SourceKind `json:"kind"`
	Ref  string     `json:"ref,omitempty"`
}

// Record is one entry in the runtime Registry. It carries the decoded
// Plugin plus its provenance so audit logs can answer "why is this
// plugin running".
type Record struct {
	Plugin *Plugin `json:"plugin"`
	Source Source  `json:"source"`
	// Enabled flips independently of the in-memory record because an
	// admin can disable a Helm-installed plugin without modifying the
	// source ConfigMap.
	Enabled bool `json:"enabled"`
	// OrgID scopes the record to one organisation. The empty string
	// means cluster-wide, which is what Helm-installed plugins use and
	// what every org therefore inherits. Dispatch must filter on this
	// (see EnabledForOrg) — the registry is process-wide, so without
	// the filter one tenant's traces would be forwarded to another
	// tenant's vendor account.
	OrgID string `json:"org_id,omitempty"`
}

// Registry is the runtime cache of plugins. Construction is
// goroutine-safe; Lookup is read-mostly and uses an RWMutex so the
// hot path doesn't serialise.
//
// The Registry is intentionally tiny: ~hundreds of entries max, each
// read once per trace cycle. There is no need for a more elaborate
// indexing strategy.
// The map is keyed by (OrgID, metadata.name) so two organisations can
// install a plugin under the same name without clobbering each other.
type Registry struct {
	mu  sync.RWMutex
	all map[string]*Record
}

// NewRegistry returns an empty Registry. The caller is expected to
// fill it via Merge before the eval worker asks for entries.
func NewRegistry() *Registry {
	return &Registry{all: make(map[string]*Record)}
}

// recordKey scopes a name to an organisation. NUL cannot appear in
// either half (names are validated, org ids are uuids), so the joined
// form is unambiguous.
func recordKey(orgID, name string) string { return orgID + "\x00" + name }

// Merge absorbs a list of Records, applying the cluster-wins
// precedence rule: when two records share the same metadata.name, the
// Helm source (SourceHelm) is preserved and the database source is
// discarded with a conflict record returned to the caller.
//
// The returned slice contains every conflicting stale entry; the
// caller can surface it in startup logs or an admin banner.
func (r *Registry) Merge(in []Record) (discarded []Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range in {
		if rec.Plugin == nil {
			continue
		}
		if err := Validate(rec.Plugin); err != nil {
			discarded = append(discarded, rec)
			continue
		}
		key := recordKey(rec.OrgID, rec.Plugin.Metadata.Name)
		existing, ok := r.all[key]
		if !ok {
			r.all[key] = cloneRecord(&rec)
			continue
		}
		switch {
		case existing.Source.Kind == SourceHelm && rec.Source.Kind == SourceDatabase:
			discarded = append(discarded, rec)
		case existing.Source.Kind == SourceDatabase && rec.Source.Kind == SourceHelm:
			r.all[key] = cloneRecord(&rec)
		default:
			// Same source class colliding → keep the latest write.
			r.all[key] = cloneRecord(&rec)
		}
	}
	return discarded
}

// SetEnabled toggles a single plugin's admin switch without touching
// the source Plugin body. Returns an error if the name is unknown so
// the admin API can 404 cleanly.
// It applies to every org that installed the name, because the admin
// toggle is name-addressed (the console has no per-org switch yet).
func (r *Registry) SetEnabled(name string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	found := false
	for _, rec := range r.all {
		if rec.Plugin != nil && rec.Plugin.Metadata.Name == name {
			rec.Enabled = enabled
			found = true
		}
	}
	if !found {
		return fmt.Errorf("plugin %q not found", name)
	}
	return nil
}

// Lookup returns a snapshot copy so callers can't mutate the live
// record. Returns ok=false when the name is absent.
//
// Lookup is name-only because two callers have no org in hand: the
// vendor webhook route (an inbound POST from LangSmith carries no
// Nexus tenant) and the admin test probe. Cluster-wide wins, then the
// lowest org id, so the choice is deterministic rather than map-order
// dependent. Callers that do know the org should use LookupForOrg.
func (r *Registry) Lookup(name string) (Record, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if rec, ok := r.all[recordKey("", name)]; ok {
		return cloneRecordValue(rec), true
	}
	var best *Record
	for _, rec := range r.all {
		if rec.Plugin == nil || rec.Plugin.Metadata.Name != name {
			continue
		}
		if best == nil || rec.OrgID < best.OrgID {
			best = rec
		}
	}
	if best == nil {
		return Record{}, false
	}
	return cloneRecordValue(best), true
}

// LookupForOrg resolves a name within one organisation, falling back to
// the cluster-wide record so an org inherits Helm-installed plugins.
func (r *Registry) LookupForOrg(orgID, name string) (Record, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if rec, ok := r.all[recordKey(orgID, name)]; ok {
		return cloneRecordValue(rec), true
	}
	if rec, ok := r.all[recordKey("", name)]; ok {
		return cloneRecordValue(rec), true
	}
	return Record{}, false
}

// Remove drops one (org, name) entry. Deleting a per-org row must not
// evict the cluster-wide plugin of the same name, which is why the org
// is part of the address. Reports whether anything was removed.
func (r *Registry) Remove(orgID, name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := recordKey(orgID, name)
	if _, ok := r.all[key]; !ok {
		return false
	}
	delete(r.all, key)
	return true
}

// EnabledForOrg lists the enabled plugins one organisation may use:
// its own rows plus the cluster-wide ones it inherits. An org-specific
// record shadows a cluster-wide record of the same name so an override
// replaces the inherited plugin instead of doubling the send.
//
// This is the only correct source for dispatch. Enabled() spans every
// tenant, so using it per trace would forward one org's prompts and
// completions to another org's vendor account.
func (r *Registry) EnabledForOrg(orgID string) []Record {
	r.mu.RLock()
	defer r.mu.RUnlock()
	byName := make(map[string]*Record)
	for _, rec := range r.all {
		if !rec.Enabled || rec.Plugin == nil {
			continue
		}
		if rec.OrgID != "" && rec.OrgID != orgID {
			continue
		}
		name := rec.Plugin.Metadata.Name
		// Prefer the org's own row over the inherited cluster-wide one.
		if prev, ok := byName[name]; ok && prev.OrgID != "" {
			continue
		}
		byName[name] = rec
	}
	out := make([]Record, 0, len(byName))
	for _, rec := range byName {
		out = append(out, cloneRecordValue(rec))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Plugin.Metadata.Name < out[j].Plugin.Metadata.Name
	})
	return out
}

// Enabled lists every enabled plugin across all orgs, sorted by name.
// Use it only for tenant-agnostic work such as the result poller, which
// must contact every vendor regardless of who owns the plugin. For
// per-trace dispatch use EnabledForOrg.
func (r *Registry) Enabled() []Record {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Record, 0, len(r.all))
	for _, rec := range r.all {
		if rec.Enabled {
			out = append(out, cloneRecordValue(rec))
		}
	}
	sortByNameThenOrg(out)
	return out
}

// All returns every loaded plugin without filtering.
func (r *Registry) All() []Record {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Record, 0, len(r.all))
	for _, rec := range r.all {
		out = append(out, cloneRecordValue(rec))
	}
	sortByNameThenOrg(out)
	return out
}

// sortByNameThenOrg keeps output stable for the two listings that span
// organisations, where one name can legitimately appear more than once.
func sortByNameThenOrg(out []Record) {
	sort.Slice(out, func(i, j int) bool {
		li, lj := out[i].Plugin.Metadata.Name, out[j].Plugin.Metadata.Name
		if li != lj {
			return li < lj
		}
		return out[i].OrgID < out[j].OrgID
	})
}

// ErrEmptyRegistry is exposed so callers can detect the "no plugins
// configured" state without coupling to the literal panic message.
var ErrEmptyRegistry = errors.New("registry is empty")

func cloneRecord(in *Record) *Record {
	if in == nil {
		return nil
	}
	cp := *in
	if in.Plugin != nil {
		p := *in.Plugin
		if in.Plugin.Metadata.Labels != nil {
			p.Metadata.Labels = make(map[string]string, len(in.Plugin.Metadata.Labels))
			for k, v := range in.Plugin.Metadata.Labels {
				p.Metadata.Labels[k] = v
			}
		}
		if in.Plugin.Spec.Send.Payload != nil {
			p.Spec.Send.Payload = make(map[string]string, len(in.Plugin.Spec.Send.Payload))
			for k, v := range in.Plugin.Spec.Send.Payload {
				p.Spec.Send.Payload[k] = v
			}
		}
		if in.Plugin.Spec.Send.Redact != nil {
			p.Spec.Send.Redact = append([]string(nil), in.Plugin.Spec.Send.Redact...)
		}
		cp.Plugin = &p
	}
	return &cp
}

func cloneRecordValue(in *Record) Record {
	if in == nil {
		return Record{}
	}
	return *cloneRecord(in)
}
