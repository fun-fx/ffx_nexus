package evals

import (
	"context"
	"testing"
	"time"
)

func TestProfileValidate(t *testing.T) {
	tests := []struct {
		name string
		mut  func(p *EvalProfile)
		ok   bool
		want string
	}{
		{
			name: "heuristic org profile",
			mut: func(p *EvalProfile) {
				p.Name = "PII strict"
				p.Kind = ProfileHeuristicPII
				p.Scope = ScopeOrg
				p.SampleRate = 1.0
				p.Enabled = true
			},
			ok: true,
		},
		{
			name: "user scope requires owner_user_id",
			mut: func(p *EvalProfile) {
				p.Name = "user judge"
				p.Kind = ProfileSLMJudge
				p.Scope = ScopeUser
				p.Endpoint = EvalEndpoint{
					BaseURL: "http://o/v1", Model: "gpt-4o-mini",
					KeySource: KeySourceInline, KeyRef: "token_123",
				}
				p.SampleRate = 0.5
				p.Enabled = true
			},
			ok:   false,
			want: "scope=user requires owner_user_id",
		},
		{
			name: "non-heuristic requires base_url",
			mut: func(p *EvalProfile) {
				p.Name = "judge"
				p.Kind = ProfileSLMJudge
				p.Scope = ScopeOrg
				p.Endpoint = EvalEndpoint{Model: "gpt-4o-mini", KeySource: KeySourceBuiltin}
				p.SampleRate = 1.0
				p.Enabled = true
			},
			ok:   false,
			want: "non-heuristic profiles require endpoint.base_url",
		},
		{
			name: "inline key_source requires key_ref",
			mut: func(p *EvalProfile) {
				p.Name = "inline judge"
				p.Kind = ProfileSLMJudge
				p.Scope = ScopeUser
				p.OwnerUserID = "u-1"
				p.Endpoint = EvalEndpoint{BaseURL: "http://o/v1", Model: "gpt-4o-mini", KeySource: KeySourceInline}
				p.SampleRate = 1.0
				p.Enabled = true
			},
			ok:   false,
			want: "key_source=inline requires a server-side key_ref token",
		},
		{
			name: "heuristic with non-builtin key_source rejected",
			mut: func(p *EvalProfile) {
				p.Name = "pii"
				p.Kind = ProfileHeuristicPII
				p.Scope = ScopeOrg
				p.Endpoint = EvalEndpoint{KeySource: KeySourceInline}
				p.SampleRate = 1.0
				p.Enabled = true
			},
			ok:   false,
			want: "heuristic profiles must use key_source=builtin",
		},
		{
			name: "negative sample rate rejected",
			mut: func(p *EvalProfile) {
				p.Name = "x"
				p.Kind = ProfileHeuristicPII
				p.Scope = ScopeOrg
				p.SampleRate = -0.1
			},
			ok:   false,
			want: "sample_rate must be in [0,1]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &EvalProfile{}
			tt.mut(p)
			err := p.Validate()
			if tt.ok && err != nil {
				t.Fatalf("expected ok, got: %v", err)
			}
			if !tt.ok {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tt.want != "" && err.Error() != tt.want {
					t.Fatalf("err=%q; want=%q", err.Error(), tt.want)
				}
			}
		})
	}
}

func TestMemoryStoreSaveAndGet(t *testing.T) {
	// Stable clock so CreatedAt is deterministic in tests.
	t0 := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore(func() time.Time { return t0 })

	p := &EvalProfile{
		Name: "PII strict", Kind: ProfileHeuristicPII, Scope: ScopeOrg,
		SampleRate: 1.0, Enabled: true,
	}
	if err := store.Save(context.Background(), p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if p.ID == "" {
		t.Fatalf("Save should have assigned an ID")
	}
	if p.CreatedAt.IsZero() || !p.CreatedAt.Equal(t0) {
		t.Fatalf("CreatedAt unset: %v", p.CreatedAt)
	}
	got, err := store.Get(context.Background(), p.ID)
	if err != nil || got.Name != "PII strict" {
		t.Fatalf("Get: got=%+v err=%v", got, err)
	}
	all, err := store.List(context.Background(), "u-other")
	if err != nil || len(all) != 1 {
		t.Fatalf("org profile visible to non-owner: got=%v err=%v", all, err)
	}
}

func TestMemoryStoreUserScopeFilter(t *testing.T) {
	store := NewMemoryStore(nil)
	mine := &EvalProfile{Name: "mine", Kind: ProfileSLMJudge, Scope: ScopeUser, OwnerUserID: "u-me",
		Endpoint: EvalEndpoint{BaseURL: "http://o", Model: "m", KeySource: KeySourceBuiltin}, SampleRate: 1.0, Enabled: true}
	other := &EvalProfile{Name: "theirs", Kind: ProfileSLMJudge, Scope: ScopeUser, OwnerUserID: "u-else",
		Endpoint: EvalEndpoint{BaseURL: "http://o", Model: "m", KeySource: KeySourceBuiltin}, SampleRate: 1.0, Enabled: true}
	org := &EvalProfile{Name: "org", Kind: ProfileHeuristicPII, Scope: ScopeOrg, SampleRate: 1.0, Enabled: true}
	for _, p := range []*EvalProfile{mine, other, org} {
		if err := store.Save(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	// Asking as u-me → org + own user scope only.
	got, _ := store.List(context.Background(), "u-me")
	if len(got) != 2 {
		t.Fatalf("u-me should see 2 profiles (own+org), got %d", len(got))
	}
}

func TestMemoryStoreDeletion(t *testing.T) {
	store := NewMemoryStore(nil)
	p := &EvalProfile{Name: "x", Kind: ProfileHeuristicPII, Scope: ScopeOrg, SampleRate: 1.0, Enabled: true}
	if err := store.Save(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), p.ID); err != ErrProfileNotFound {
		t.Fatalf("want ErrProfileNotFound, got %v", err)
	}
}
