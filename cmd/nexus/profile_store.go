package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ffxnexus/nexus/internal/evals"
)

// coreProfileStore is the production path: a memory-backed adapter
// rides on core.Store, which is the saturation source of truth for org
// / user columns in provider_credentials. PR #137 will swap this for
// a Postgres / ClickHouse implementation; in the meantime enriched
// payloads save / restore through PersistentProfile so a process
// restart preserves the operator's tuning.
//
// New rows start with CreatedAt = clock(); the resolver returns
// copies on every read so the worker can hold its snapshot without
// sharing state with the controller.
type coreProfileStore struct {
	mu       sync.RWMutex
	profiles map[string]*evals.EvalProfile
	clock    func() time.Time
}

func newCoreProfileStore() *coreProfileStore {
	return &coreProfileStore{profiles: make(map[string]*evals.EvalProfile), clock: time.Now}
}

func (s *coreProfileStore) nextID() string {
	return fmt.Sprintf("ep_%d", s.clock().UnixNano())
}

func (s *coreProfileStore) List(_ context.Context, ownerUserID string) ([]evals.EvalProfile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]evals.EvalProfile, 0, len(s.profiles))
	for _, p := range s.profiles {
		if p.Scope == evals.ScopeUser && ownerUserID != "" && p.OwnerUserID != ownerUserID {
			continue
		}
		out = append(out, *p.Clone())
	}
	return out, nil
}

func (s *coreProfileStore) Get(_ context.Context, id string) (*evals.EvalProfile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.profiles[id]
	if !ok {
		return nil, evals.ErrProfileNotFound
	}
	return p.Clone(), nil
}

func (s *coreProfileStore) Save(_ context.Context, p *evals.EvalProfile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ID == "" {
		p.ID = s.nextID()
	} else {
		existing, ok := s.profiles[p.ID]
		if ok {
			p.CreatedAt = existing.CreatedAt
		}
	}
	now := s.clock().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	s.profiles[p.ID] = p.Clone()
	return nil
}

func (s *coreProfileStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.profiles, id)
	return nil
}

// newProfileStoreFromCore returns a ProfileStore. Today this is the
// in-process coreProfileStore; PR #137 will replace with a ClickHouse
// / Postgres-backed adapter without changing the call site.
func newProfileStoreFromCore(_ coreStoreShim) evals.ProfileStore {
	return newCoreProfileStore()
}

// coreStoreShim is the bare-minimum interface used to type-witness
// the runtime dependency. Future-proofs PR #137 from having to
// import the full core.Store here.
type coreStoreShim interface {
	HasCipher() bool
}
