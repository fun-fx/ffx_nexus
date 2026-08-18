package evals

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ProfileStore is the durable home for EvalProfile rows. The interface
// intentionally exposes only what the console + worker need today so
// we can swap ClickHouse ⇄ Postgres without changing call sites.
//
// Implementations live in pg.go / clickhouse.go (added in PR #135 along
// with the score history tables). There is also the ephemeral
// MemoryStore below used in tests.
type ProfileStore interface {
	// List returns profiles visible to one tenant.
	//
	// orgID selects the tenant: rows belonging to that org plus cluster-wide
	// rows (OrgID == "", installed by the operator through env/Helm seeding).
	// Passing "" means "every row, no tenant filter" and is reserved for the
	// worker's dispatch snapshot and for boot-time seeding, both of which
	// filter per trace afterwards. A request-serving caller must always pass
	// a concrete org, or it hands one tenant another tenant's configuration.
	//
	// When ownerUserID is empty, org-scoped profiles are returned; otherwise
	// the caller also gets their own user-scoped rows. Admin widening happens
	// at the profileCallerCanSee layer (PR #136).
	List(ctx context.Context, orgID, ownerUserID string) ([]EvalProfile, error)
	// Get returns a single profile by id (admin / owner only; caller
	// enforces this).
	Get(ctx context.Context, id string) (*EvalProfile, error)
	// Save persists a profile. The implementation is responsible for
	// ID assignment on create; updates use the same call with the
	// existing ID.
	Save(ctx context.Context, p *EvalProfile) error
	// Delete removes a profile by id. Missing is not an error (idempotent).
	Delete(ctx context.Context, id string) error
}

// ErrProfileNotFound is returned by Get / Delete when the row is
// absent. Call sites should distinguish "not yours to see" from "really
// missing" via permission checks upstream.
var ErrProfileNotFound = errors.New("eval profile not found")

// LegacyDefaultOrgID mirrors evalplugin.LegacyDefaultOrgID: the placeholder the
// console stamps when a request carries no org. Defined here rather than
// imported to keep evals free of a dependency on evalplugin.
const LegacyDefaultOrgID = "default"

// NormalizeProfileOrgID folds the legacy "default" placeholder onto the
// cluster-wide empty string.
//
// Both spellings existed in the wild before profiles carried an org at all, and
// a comparison that treated them as different orgs would silently stop applying
// an operator's profiles to their own traffic — the same bug evalplugin already
// fixed for plugin dispatch.
func NormalizeProfileOrgID(orgID string) string {
	if orgID == LegacyDefaultOrgID {
		return ""
	}
	return orgID
}

// VisibleToOrg reports whether a profile belongs to orgID or is cluster-wide.
//
// This is the single definition of the tenant boundary for profiles, used by
// both the store's list filter and the worker's dispatch filter so the two
// cannot drift into disagreeing about who owns a row.
func (p EvalProfile) VisibleToOrg(orgID string) bool {
	own := NormalizeProfileOrgID(p.OrgID)
	if own == "" {
		// Cluster-wide: the operator's own seeded configuration, deliberately
		// applied to every tenant in the installation.
		return true
	}
	return own == NormalizeProfileOrgID(orgID)
}

// MemoryStore holds profiles in-process and is intended for unit tests
// where spinning up Postgres/ClickHouse is overkill. Mirrors the
// semantics expected of the durable stores so production code can be
// tested against it.
type MemoryStore struct {
	mu       sync.RWMutex
	profiles map[string]*EvalProfile
	clock    func() time.Time
	counter  uint64
}

// NewMemoryStore creates a deterministic, in-memory ProfileStore. Tests
// pass a custom clock when they care about CreatedAt ordering; otherwise
// it uses time.Now.
func NewMemoryStore(clock func() time.Time) *MemoryStore {
	if clock == nil {
		clock = time.Now
	}
	return &MemoryStore{profiles: make(map[string]*EvalProfile), clock: clock}
}

// nextID produces a sortable id without depending on rand —
// sufficiently unique for an in-memory store. Database-backed
// stores replace this with UUIDv7 once PR #137 lands.
func (m *MemoryStore) nextID() string {
	return fmt.Sprintf("ep_%d_%d", m.clock().UnixNano(), atomic.AddUint64(&m.counter, 1))
}

func (m *MemoryStore) List(_ context.Context, orgID, ownerUserID string) ([]EvalProfile, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]EvalProfile, 0, len(m.profiles))
	for _, p := range m.profiles {
		if orgID != "" && !p.VisibleToOrg(orgID) {
			continue
		}
		if p.Scope == ScopeUser && ownerUserID != "" && p.OwnerUserID != ownerUserID {
			continue
		}
		out = append(out, *p.Clone())
	}
	return out, nil
}

func (m *MemoryStore) Get(_ context.Context, id string) (*EvalProfile, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.profiles[id]
	if !ok {
		return nil, ErrProfileNotFound
	}
	return p.Clone(), nil
}

func (m *MemoryStore) Save(_ context.Context, p *EvalProfile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.TrimSpace(p.ID) == "" {
		p.ID = m.nextID()
	} else {
		existing, ok := m.profiles[p.ID]
		if ok {
			p.CreatedAt = existing.CreatedAt
		}
	}
	now := m.clock().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	m.profiles[p.ID] = p.Clone()
	return nil
}

func (m *MemoryStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.profiles, id)
	return nil
}
