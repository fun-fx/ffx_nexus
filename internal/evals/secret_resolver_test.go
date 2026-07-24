package evals

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/observability"
)

type fakeLookup struct {
	orgResponse  string
	orgErr       error
	userResponse string
	userErr      error
	lastUserID   string
	lastUserProv string
	lastOrgProv  string
	calls        int
}

func (f *fakeLookup) Org(_ context.Context, prov string) (string, error) {
	f.calls++
	f.lastOrgProv = prov
	return f.orgResponse, f.orgErr
}

func (f *fakeLookup) User(_ context.Context, prov, userID string) (string, error) {
	f.calls++
	f.lastUserProv = prov
	f.lastUserID = userID
	return f.userResponse, f.userErr
}

func (f *fakeLookup) Inline(_ context.Context, _ string) (string, error) {
	return "", errors.New("unused")
}

func TestResolver_Org(t *testing.T) {
	f := &fakeLookup{orgResponse: "secret-key"}
	r := NewResolver(f, WithOrgProvider("openai"))
	ep := EvalEndpoint{KeySource: KeySourceOrg, BaseURL: "https://api.openai.com/v1"}
	got, err := r.Resolve(observability.Trace{}, ep)
	if err != nil {
		t.Fatal(err)
	}
	if got != "secret-key" {
		t.Fatalf("got %q want %q", got, "secret-key")
	}
	if f.lastOrgProv != "api" { // deriveProviderFromURL strips scheme → first seg before dot
		t.Fatalf("provider inference wrong: %s", f.lastOrgProv)
	}
}

func TestResolver_User(t *testing.T) {
	f := &fakeLookup{userResponse: "user-secret"}
	r := NewResolver(f)
	ep := EvalEndpoint{
		KeySource: KeySourceUser,
		BaseURL:   "https://api.anthropic.com/v1",
		KeyRef:    "u-owner-9",
	}
	got, err := r.Resolve(observability.Trace{}, ep)
	if err != nil {
		t.Fatal(err)
	}
	if got != "user-secret" {
		t.Fatalf("got %q", got)
	}
	if f.lastUserID != "u-owner-9" {
		t.Fatalf("userID chain: %q", f.lastUserID)
	}
	if f.lastUserProv != "api" {
		t.Fatalf("provider inference wrong: %s", f.lastUserProv)
	}
}

func TestResolver_BuiltinReturnsEmpty(t *testing.T) {
	r := NewResolver(&fakeLookup{})
	ep := EvalEndpoint{KeySource: KeySourceBuiltin, BaseURL: "https://api.openai.com/v1"}
	got, err := r.Resolve(observability.Trace{}, ep)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("builtin should yield empty secret, got %q", got)
	}
}

func TestResolver_InlineRegister(t *testing.T) {
	r := NewResolver(&fakeLookup{})
	r.RegisterInline("tk-1", "plain-A", time.Time{})
	ep := EvalEndpoint{KeySource: KeySourceInline, KeyRef: "tk-1"}
	got, err := r.Resolve(observability.Trace{}, ep)
	if err != nil {
		t.Fatal(err)
	}
	if got != "plain-A" {
		t.Fatalf("got %q", got)
	}
	r.RevokeInline("tk-1")
	if _, err := r.Resolve(observability.Trace{}, ep); err != ErrSecretNotFound {
		t.Fatalf("after revoke want ErrSecretNotFound, got %v", err)
	}
}

func TestResolver_InlineUnknownKeyReturnsNotFound(t *testing.T) {
	r := NewResolver(&fakeLookup{})
	ep := EvalEndpoint{KeySource: KeySourceInline, KeyRef: "missing"}
	if _, err := r.Resolve(observability.Trace{}, ep); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound, got %v", err)
	}
}

func TestResolver_InvalidKeySource(t *testing.T) {
	r := NewResolver(&fakeLookup{})
	ep := EvalEndpoint{KeySource: "weird", BaseURL: "http://x"}
	if _, err := r.Resolve(observability.Trace{}, ep); err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestResolver_OrgLookupPropagatesError(t *testing.T) {
	wantErr := errors.New("db down")
	f := &fakeLookup{orgErr: wantErr}
	r := NewResolver(f)
	ep := EvalEndpoint{KeySource: KeySourceOrg, BaseURL: "http://api.openai.com"}
	if _, err := r.Resolve(observability.Trace{}, ep); !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped org err, got %v", err)
	}
}
