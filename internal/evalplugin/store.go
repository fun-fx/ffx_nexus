package evalplugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// PluginRecord is the durable form of a plugin. The same (org_id, name)
// pair is unique so per-org customisations coexist with cluster-wide
// Helm installs (the latter use org_id = "").
type PluginRecord struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	Name      string    `json:"name"`
	SpecYAML  string    `json:"spec_yaml"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PluginStore is the durable home for PluginRecord rows. Mirror of
// evals.ProfileStore — same interface shape, swap-pg/clickhouse is
// future PR work.
//
// The Merge semantics needed by Registry (Helm wins on conflict) live
// in Registry, not here; the store simply CRUDs rows.
type PluginStore interface {
	List(ctx context.Context, orgID string) ([]PluginRecord, error)
	Get(ctx context.Context, id string) (*PluginRecord, error)
	Save(ctx context.Context, r *PluginRecord) error
	Delete(ctx context.Context, id string) error
}

// ErrPluginNotFound is returned by Get/Delete when the row is absent.
var ErrPluginNotFound = errors.New("eval plugin not found")

// MemoryStore is the in-process plugin store used in unit tests and
// for single-binary deployments where ClickHouse/Postgres are absent.
// Mirrors evals.MemoryStore so the call sites read identically.
type MemoryStore struct {
	mu      sync.RWMutex
	plugins map[string]*PluginRecord
	clock   func() time.Time
	counter uint64
}

// NewMemoryStore returns an empty in-process plugin store. Tests pass
// a custom clock when they care about CreatedAt ordering.
func NewMemoryStore(clock func() time.Time) *MemoryStore {
	if clock == nil {
		clock = time.Now
	}
	return &MemoryStore{plugins: make(map[string]*PluginRecord), clock: clock}
}

func (m *MemoryStore) nextID() string {
	return fmt.Sprintf("plg_%d_%d", m.clock().UnixNano(), atomic.AddUint64(&m.counter, 1))
}

// List returns all rows for the given org. Empty orgID returns
// cluster-wide rows only; non-empty returns both cluster-wide and
// per-org customizations.
func (m *MemoryStore) List(_ context.Context, orgID string) ([]PluginRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]PluginRecord, 0, len(m.plugins))
	for _, r := range m.plugins {
		if orgID != "" && r.OrgID != "" && r.OrgID != orgID {
			continue
		}
		out = append(out, clonePluginRecord(r))
	}
	return out, nil
}

func (m *MemoryStore) Get(_ context.Context, id string) (*PluginRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.plugins[id]
	if !ok {
		return nil, ErrPluginNotFound
	}
	cp := clonePluginRecord(r)
	return &cp, nil
}

// Save persists the row. An empty ID triggers auto-assignment. The
// spec_yaml is re-validated on read by Decode in PluginStore.Save to
// keep a malformed manifest out of the registry.
func (m *MemoryStore) Save(_ context.Context, r *PluginRecord) error {
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("plugin name is required")
	}
	if strings.TrimSpace(r.SpecYAML) == "" {
		return errors.New("plugin spec_yaml is required")
	}
	if _, err := Decode([]byte(r.SpecYAML)); err != nil {
		return fmt.Errorf("re-validate spec_yaml: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.TrimSpace(r.ID) == "" {
		r.ID = m.nextID()
	} else {
		if existing, ok := m.plugins[r.ID]; ok {
			r.CreatedAt = existing.CreatedAt
		}
	}
	now := m.clock().UTC()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	r.UpdatedAt = now
	m.plugins[r.ID] = &PluginRecord{
		ID:        r.ID,
		OrgID:     r.OrgID,
		Name:      r.Name,
		SpecYAML:  r.SpecYAML,
		Enabled:   r.Enabled,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
	return nil
}

func (m *MemoryStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.plugins, id)
	return nil
}

func clonePluginRecord(in *PluginRecord) PluginRecord {
	if in == nil {
		return PluginRecord{}
	}
	return PluginRecord{
		ID:        in.ID,
		OrgID:     in.OrgID,
		Name:      in.Name,
		SpecYAML:  in.SpecYAML,
		Enabled:   in.Enabled,
		CreatedAt: in.CreatedAt,
		UpdatedAt: in.UpdatedAt,
	}
}
