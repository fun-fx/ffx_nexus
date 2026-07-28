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
	Ref string      `json:"ref,omitempty"`
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
}

// Registry is the runtime cache of plugins. Construction is
// goroutine-safe; Lookup is read-mostly and uses an RWMutex so the
// hot path doesn't serialise.
//
// The Registry is intentionally tiny: ~hundreds of entries max, each
// read once per trace cycle. There is no need for a more elaborate
// indexing strategy.
type Registry struct {
	mu  sync.RWMutex
	all map[string]*Record
}

// NewRegistry returns an empty Registry. The caller is expected to
// fill it via Merge before the eval worker asks for entries.
func NewRegistry() *Registry {
	return &Registry{all: make(map[string]*Record)}
}

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
		existing, ok := r.all[rec.Plugin.Metadata.Name]
		if !ok {
			r.all[rec.Plugin.Metadata.Name] = cloneRecord(&rec)
			continue
		}
		switch {
		case existing.Source.Kind == SourceHelm && rec.Source.Kind == SourceDatabase:
			discarded = append(discarded, rec)
		case existing.Source.Kind == SourceDatabase && rec.Source.Kind == SourceHelm:
			r.all[rec.Plugin.Metadata.Name] = cloneRecord(&rec)
		default:
			// Same source class colliding → keep the latest write.
			r.all[rec.Plugin.Metadata.Name] = cloneRecord(&rec)
		}
	}
	return discarded
}

// SetEnabled toggles a single plugin's admin switch without touching
// the source Plugin body. Returns an error if the name is unknown so
// the admin API can 404 cleanly.
func (r *Registry) SetEnabled(name string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.all[name]
	if !ok {
		return fmt.Errorf("plugin %q not found", name)
	}
	rec.Enabled = enabled
	return nil
}

// Lookup returns a snapshot copy so callers can't mutate the live
// record. Returns ok=false when the name is absent.
func (r *Registry) Lookup(name string) (Record, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.all[name]
	if !ok {
		return Record{}, false
	}
	return cloneRecordValue(rec), true
}

// Enabled lists every enabled plugin sorted by name. The dispatcher
// iterates this snapshot once per trace; sorting keeps log output
// deterministic for tests.
func (r *Registry) Enabled() []Record {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Record, 0, len(r.all))
	for _, rec := range r.all {
		if rec.Enabled {
			out = append(out, cloneRecordValue(rec))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Plugin.Metadata.Name < out[j].Plugin.Metadata.Name
	})
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
	sort.Slice(out, func(i, j int) bool {
		return out[i].Plugin.Metadata.Name < out[j].Plugin.Metadata.Name
	})
	return out
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
