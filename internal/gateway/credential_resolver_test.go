package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeCredSource struct {
	mu    sync.Mutex
	calls int
	cred  ResolvedCredential
	found bool
	err   error
}

func (f *fakeCredSource) ResolveCredential(_ context.Context, _, _, _ string) (ResolvedCredential, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.cred, f.found, f.err
}

func TestParseKeyMode(t *testing.T) {
	cases := map[string]KeyMode{
		"":            KeyModeShared,
		"shared":      KeyModeShared,
		"byok":        KeyModeBYOK,
		"strict_byok": KeyModeStrictBYOK,
		"strict-byok": KeyModeStrictBYOK,
		"nonsense":    KeyModeShared,
	}
	for in, want := range cases {
		if got := ParseKeyMode(in); got != want {
			t.Errorf("ParseKeyMode(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCredentialResolverCaches(t *testing.T) {
	src := &fakeCredSource{cred: ResolvedCredential{Secret: "sk-user", Source: "user"}, found: true}
	cr := NewCredentialResolver(src, time.Minute)

	for i := 0; i < 3; i++ {
		cred, found, err := cr.Resolve(context.Background(), "default", "u1", "openai")
		if err != nil || !found || cred.Secret != "sk-user" {
			t.Fatalf("resolve %d: got (%+v, %v, %v)", i, cred, found, err)
		}
	}
	if src.calls != 1 {
		t.Fatalf("expected 1 source call (cached), got %d", src.calls)
	}

	// Different user must not share the cache entry.
	if _, _, err := cr.Resolve(context.Background(), "default", "u2", "openai"); err != nil {
		t.Fatal(err)
	}
	if src.calls != 2 {
		t.Fatalf("expected 2 source calls for distinct users, got %d", src.calls)
	}

	// Invalidation forces re-resolution.
	cr.Invalidate()
	if _, _, err := cr.Resolve(context.Background(), "default", "u1", "openai"); err != nil {
		t.Fatal(err)
	}
	if src.calls != 3 {
		t.Fatalf("expected re-resolution after Invalidate, got %d calls", src.calls)
	}
}

func TestCredentialResolverNoCacheTTL(t *testing.T) {
	src := &fakeCredSource{cred: ResolvedCredential{Secret: "sk"}, found: true}
	cr := NewCredentialResolver(src, 0) // caching disabled
	for i := 0; i < 3; i++ {
		if _, _, err := cr.Resolve(context.Background(), "o", "u", "openai"); err != nil {
			t.Fatal(err)
		}
	}
	if src.calls != 3 {
		t.Fatalf("expected 3 uncached calls, got %d", src.calls)
	}
}

func TestCredentialResolverError(t *testing.T) {
	src := &fakeCredSource{err: errors.New("db down")}
	cr := NewCredentialResolver(src, time.Minute)
	if _, _, err := cr.Resolve(context.Background(), "o", "u", "openai"); err == nil {
		t.Fatal("expected error to propagate")
	}
}

func TestCallerCredentialContext(t *testing.T) {
	ctx := context.Background()
	if _, ok := CallerCredentialFrom(ctx); ok {
		t.Fatal("expected no credential in empty context")
	}
	ctx = WithCallerCredential(ctx, CallerCredential{Secret: "sk-abc", BaseURL: "https://x", Source: "user"})
	c, ok := CallerCredentialFrom(ctx)
	if !ok || c.Secret != "sk-abc" || c.BaseURL != "https://x" || c.Source != "user" {
		t.Fatalf("unexpected credential: %+v ok=%v", c, ok)
	}
	// Empty secret is treated as "no override".
	ctx2 := WithCallerCredential(context.Background(), CallerCredential{Secret: ""})
	if _, ok := CallerCredentialFrom(ctx2); ok {
		t.Fatal("empty secret should not be treated as an override")
	}
}
