package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/evals"
)

// stubProfileStore implements EvalProfileSource in-memory so tests can
// pin the visibility behaviour without spinning up ClickHouse /
// Postgres. The audit log is intentionally a grey idea — the console's
// real audit wiring lives on server.go; tests focus on the filter.
type stubProfileStore struct {
	profiles map[string]*evals.EvalProfile
}

func newStubProfileStore(initial ...*evals.EvalProfile) *stubProfileStore {
	s := &stubProfileStore{profiles: make(map[string]*evals.EvalProfile)}
	for _, p := range initial {
		s.profiles[p.ID] = p.Clone()
	}
	return s
}

func (s *stubProfileStore) ListEvalProfiles(_ context.Context, ownerUserID string) ([]evals.EvalProfile, error) {
	out := make([]evals.EvalProfile, 0, len(s.profiles))
	for _, p := range s.profiles {
		if p.Scope == evals.ScopeUser && p.OwnerUserID != ownerUserID {
			continue
		}
		out = append(out, *p.Clone())
	}
	return out, nil
}

func (s *stubProfileStore) GetEvalProfile(_ context.Context, id string) (*evals.EvalProfile, error) {
	p, ok := s.profiles[id]
	if !ok {
		return nil, evals.ErrProfileNotFound
	}
	return p.Clone(), nil
}

func (s *stubProfileStore) SaveEvalProfile(_ context.Context, p *evals.EvalProfile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	s.profiles[p.ID] = p.Clone()
	return nil
}

func (s *stubProfileStore) DeleteEvalProfile(_ context.Context, id string) error {
	delete(s.profiles, id)
	return nil
}

func listPayload(t *testing.T, srv *Server, u core.User) (string, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/eval/profiles", nil).WithContext(context.Background())
	srv.handleProfileListForTest(rec, req, u)
	body := rec.Body.String()
	return body, rec.Code
}

// handleProfileListForTest is the same as listEvalProfiles without
// the audit log hook (the real handler hits s.audit which the test
// harness doesn't wire). Keeping it as a method on Server so future
// tests can swap in a recorder.
func (s *Server) handleProfileListForTest(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.evalProfiles == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "eval profiles disabled"})
		return
	}
	all, err := s.evalProfiles.ListEvalProfiles(r.Context(), u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]evals.EvalProfile, 0, len(all))
	for _, p := range all {
		if profileCallerCanSee(p, u) {
			out = append(out, p)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": out})
}

func TestProfileCallerCanSee_OrgProfile_VisibleToAll(t *testing.T) {
	org := &evals.EvalProfile{
		ID: "ep_org", Name: "PII strict", Kind: evals.ProfileHeuristicPII,
		Scope: evals.ScopeOrg, SampleRate: 1.0, Enabled: true,
	}
	cases := []struct {
		name   string
		caller core.User
		want   bool
	}{
		{"member", core.User{ID: "u-member", Role: "member"}, true},
		{"admin", core.User{ID: "u-admin", Role: "admin"}, true},
		{"different user", core.User{ID: "u-other", Role: "member"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := profileCallerCanSee(*org, c.caller)
			if got != c.want {
				t.Fatalf("user=%s want=%v got=%v", c.name, c.want, got)
			}
		})
	}
}

func TestProfileCallerCanSee_UserProfile_OwnerOnly(t *testing.T) {
	up := &evals.EvalProfile{
		ID: "ep_user", Name: "My judge", Kind: evals.ProfileSLMJudge,
		Scope: evals.ScopeUser, OwnerUserID: "u-1",
		Endpoint:   evals.EvalEndpoint{BaseURL: "http://o", Model: "m", KeySource: evals.KeySourceBuiltin},
		SampleRate: 1.0, Enabled: true,
	}
	if got := profileCallerCanSee(*up, core.User{ID: "u-1", Role: "member"}); got != true {
		t.Fatalf("owner should see own profile, got=%v", got)
	}
	if got := profileCallerCanSee(*up, core.User{ID: "u-2", Role: "member"}); got != false {
		t.Fatalf("non-owner member should not see, got=%v", got)
	}
	if got := profileCallerCanSee(*up, core.User{ID: "u-admin", Role: "admin"}); got != true {
		t.Fatalf("admin should see any user profile, got=%v", got)
	}
}

func TestProfileCallerCanWrite_OwnerOnly(t *testing.T) {
	up := &evals.EvalProfile{
		ID: "ep_u", Name: "u",
		Kind: evals.ProfileSLMJudge, Scope: evals.ScopeUser,
		OwnerUserID: "u-1",
		Endpoint:    evals.EvalEndpoint{BaseURL: "http://o", Model: "m", KeySource: evals.KeySourceBuiltin},
		SampleRate:  1.0, Enabled: true,
	}
	if !profileCallerCanWrite(*up, core.User{ID: "u-1", Role: "member"}) {
		t.Fatalf("owner should be able to write")
	}
	if profileCallerCanWrite(*up, core.User{ID: "u-2", Role: "member"}) {
		t.Fatalf("non-owner should not be able to write")
	}
	org := &evals.EvalProfile{
		ID: "ep_org", Name: "PII strict",
		Kind: evals.ProfileHeuristicPII, Scope: evals.ScopeOrg,
		SampleRate: 1.0, Enabled: true,
	}
	if !profileCallerCanWrite(*org, core.User{ID: "u-admin", Role: "admin"}) {
		t.Fatalf("admin should be able to write org profile")
	}
	if profileCallerCanWrite(*org, core.User{ID: "u-member", Role: "member"}) {
		t.Fatalf("member should NOT be able to write org profile")
	}
}

func TestServer_ListEvalProfilesFiltersByScope(t *testing.T) {
	store := newStubProfileStore(
		&evals.EvalProfile{ID: "ep_org", Name: "Org", Kind: evals.ProfileHeuristicPII, Scope: evals.ScopeOrg, SampleRate: 1.0, Enabled: true},
		&evals.EvalProfile{ID: "ep_user1", Name: "Mine", Kind: evals.ProfileSLMJudge, Scope: evals.ScopeUser, OwnerUserID: "u-1", Endpoint: evals.EvalEndpoint{BaseURL: "http://o", Model: "m", KeySource: evals.KeySourceBuiltin}, SampleRate: 1.0, Enabled: true},
		&evals.EvalProfile{ID: "ep_user2", Name: "Theirs", Kind: evals.ProfileSLMJudge, Scope: evals.ScopeUser, OwnerUserID: "u-2", Endpoint: evals.EvalEndpoint{BaseURL: "http://o", Model: "m", KeySource: evals.KeySourceBuiltin}, SampleRate: 1.0, Enabled: true},
	)
	srv := &Server{evalProfiles: store}

	Member := core.User{ID: "u-1", Role: "member"}
	body, code := listPayload(t, srv, Member)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	var resp struct {
		Profiles []evals.EvalProfile `json:"profiles"`
	}
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Profiles) != 2 {
		t.Fatalf("u-1 should see 2 (Org + own), got %d: %+v", len(resp.Profiles), resp.Profiles)
	}
}
