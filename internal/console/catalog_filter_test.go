package console

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/gateway"
)

// fakeCatalog lets us pin the per-provider scope list the test asserts
// against, without spinning up the real gateway/registry stack.
type fakeCatalog struct {
	p    []gateway.ConsoleUserProvider
	chat []string
	emb  []string
}

func (f *fakeCatalog) ChatModels() []string      { return f.chat }
func (f *fakeCatalog) EmbeddingModels() []string { return f.emb }
func (f *fakeCatalog) UserProviders() []gateway.ConsoleUserProvider {
	return f.p
}

func TestCallerCanSee(t *testing.T) {
	member := core.User{ID: "u-alice", Role: "member"}
	admin := core.User{ID: "u-admin", Role: "admin"}
	other := core.User{ID: "u-bob", Role: "member"}

	tests := []struct {
		name string
		p    gateway.ConsoleUserProvider
		c    core.User
		want bool
	}{
		{"public visible to anyone", gateway.ConsoleUserProvider{Provider: "publicprov", Scope: gateway.ScopePublic}, member, true},
		{"empty scope defaults to public", gateway.ConsoleUserProvider{Provider: "legacyprov"}, member, true},
		{"user scope visible to owner", gateway.ConsoleUserProvider{Provider: "aliceprov", Scope: gateway.ScopeUser, OwnerID: "u-alice"}, member, true},
		{"user scope hidden from non-owner", gateway.ConsoleUserProvider{Provider: "aliceprov", Scope: gateway.ScopeUser, OwnerID: "u-alice"}, other, false},
		{"user scope visible to admin even if not owner", gateway.ConsoleUserProvider{Provider: "aliceprov", Scope: gateway.ScopeUser, OwnerID: "u-alice"}, admin, true},
		{"orphan user-scope (no OwnerID) never leaks", gateway.ConsoleUserProvider{Provider: "ghostprov", Scope: gateway.ScopeUser}, admin, false},
		{"org scope always shown today", gateway.ConsoleUserProvider{Provider: "teamprov", Scope: gateway.ScopeOrg}, other, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := callerCanSee(tt.p, tt.c); got != tt.want {
				t.Fatalf("callerCanSee(%v, %v) = %v; want %v", tt.p, tt.c, got, tt.want)
			}
		})
	}
}

func TestPlaygroundCatalogFiltersByCaller(t *testing.T) {
	srv := NewServer(nil, nil, nil, slog.Default())
	srv.SetCatalog(&fakeCatalog{
		p: []gateway.ConsoleUserProvider{
			{Provider: "openai", Models: []string{"gpt-4o"}, Scope: gateway.ScopePublic},
			{Provider: "teamprov", Models: []string{"gpt-5"}, Scope: gateway.ScopeOrg},
			{Provider: "aliceprov", Models: []string{"llama3"}, Scope: gateway.ScopeUser, OwnerID: "u-alice"},
			{Provider: "bobprov", Models: []string{"mistral"}, Scope: gateway.ScopeUser, OwnerID: "u-bob"},
		},
		chat: []string{"openai/gpt-4o", "teamprov/gpt-5"},
		emb:  []string{"openai/text-embed-3-small"},
	})

	cases := []struct {
		name      string
		caller    core.User
		wantNames []string
	}{
		{
			name:      "alice sees her own router but not Bob's",
			caller:    core.User{ID: "u-alice", Role: "member"},
			wantNames: []string{"openai", "teamprov", "aliceprov"},
		},
		{
			name:      "admin sees every router",
			caller:    core.User{ID: "u-admin", Role: "admin"},
			wantNames: []string{"openai", "teamprov", "aliceprov", "bobprov"},
		},
		{
			name:      "nonce member sees public + team, no personal routers",
			caller:    core.User{ID: "u-mallory", Role: "member"},
			wantNames: []string{"openai", "teamprov"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.playgroundCatalog(rec, httptest.NewRequest("GET", "/api/me/playground/catalog", nil).WithContext(context.Background()), tc.caller)
			if rec.Code != 200 {
				t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
			}
			var body struct {
				Chat []string         `json:"chat"`
				User []map[string]any `json:"user"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			gotNames := make([]string, 0, len(body.User))
			for _, u := range body.User {
				p, _ := u["provider"].(string)
				gotNames = append(gotNames, p)
			}
			slices.Sort(gotNames)
			slices.Sort(tc.wantNames)
			if !slices.Equal(gotNames, tc.wantNames) {
				t.Fatalf("providers = %v; want %v", gotNames, tc.wantNames)
			}
		})
	}
}
