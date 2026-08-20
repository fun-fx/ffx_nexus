package gateway

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// httptestTLSServer starts a TLS https server on a loopback that
// the urlpolicy HTTPS check will accept. Used by
// TestResolveNonEmptyBaseURLIsValidated.
func httptestTLSServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	return srv
}

// TestResolveEmptyBaseURLPassesValidaton pins the contract that
// an empty BaseURL on a resolved credential passes the resolver's
// dial-time validator. The save-time validator (Store.CreateCredential)
// is responsible for catching a non-empty bad value at the moment
// the operator saves it. Empty BaseURL is the canonical shape for
// "use the process-wide provider defaults" and must reach the
// upstream dial unchanged.
//
// Reversal: if a future change starts rejecting empty BaseURL,
// this test fails and prompts review. We have to do that
// consciously because rejecting empty would break shared env keys
// (the simplest install path).
func TestResolveEmptyBaseURLPassesValidaton(t *testing.T) {
	src := &fakeCredSource{
		cred: ResolvedCredential{
			Secret:  "sk-shared",
			BaseURL: "",
			Source:  "shared",
		},
		found: true,
	}
	cr := NewCredentialResolver(src, time.Minute, "")
	cred, found, err := cr.Resolve(context.Background(), "default", "u1", "openai")
	if err != nil {
		t.Fatalf("empty BaseURL should not trigger error: %v", err)
	}
	if !found || cred.Secret != "sk-shared" || cred.BaseURL != "" {
		t.Fatalf("expected (sk-shared, '', true), got %+v found=%v", cred, found)
	}
}

// TestResolveNonEmptyBaseURLIsValidated proves that any non-empty
// BaseURL travels through the dial-time gate. We use a value that
// passes the gate to assert the happy-path, then a value that
// fails (private loopback without allowlist) to assert the
// failure path.
//
// The success-path case uses a localhost (loopback) URL because
// the resolver's validator does an authoritative hostname lookup
// and rejects any destination that does not resolve. Sandboxed
// test environment frequently cannot resolve example.com, so
// loopback is the only stable choice for CI; the resolver runs
// the same validateAtDialTime path either way.
func TestResolveNonEmptyBaseURLIsValidated(t *testing.T) {
	srv := httptestTLSServer(t)
	defer srv.Close()
	cases := []struct {
		name     string
		baseURL  string
		allow    string
		wantErr  bool
	}{
		{
			name:    "loopback https with allowlist",
			baseURL: srv.URL,
			allow:   "127.0.0.0/8",
			wantErr: false,
		},
		{
			name:    "loopback rejected when not allowlisted",
			baseURL: srv.URL,
			allow:   "",
			wantErr: true,
		},
		{
			name:    "http scheme rejected even with allowlist",
			baseURL: strings.Replace(srv.URL, "https://", "http://", 1),
			allow:   "127.0.0.0/8",
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := &fakeCredSource{
				cred:  ResolvedCredential{Secret: "sk-x", BaseURL: c.baseURL},
				found: true,
			}
			cr := NewCredentialResolver(src, 0, c.allow)
			_, _, err := cr.Resolve(context.Background(), "default", "u1", "openai")
			if c.wantErr && err == nil {
				t.Errorf("expected error for %s, got nil", c.baseURL)
			}
			if !c.wantErr && err != nil {
				t.Errorf("expected no error for %s, got %v", c.baseURL, err)
			}
		})
	}
}

// TestEmptyBaseURLDialUsesProviderConstructor is the
// "what value gets used after the resolver passes empty"
// half of the audit. We assert at the credential-resolver
// layer that an empty BaseURL on the resolved credential does
// NOT cause validation to fail. The dial-layer test for
// "provider dial uses the constructor baseURL when caller
// credential has empty BaseURL" lives in
// internal/gateway/providers where the OpenAI adaptor lives;
// from the resolver's vantage the contract is "do not error".
func TestEmptyBaseURLDialUsesProviderConstructor(t *testing.T) {
	src := &fakeCredSource{
		cred: ResolvedCredential{
			Secret: "sk-byok",
			// BaseURL intentionally empty.
		},
		found: true,
	}
	cr := NewCredentialResolver(src, time.Minute, "")
	_, _, err := cr.Resolve(context.Background(), "default", "u1", "openai")
	if err != nil {
		t.Errorf("provider side should pick up process-wide baseURL, but resolver failed first: %v", err)
	}
}

// TestResolverSyncBlockGuard pins that even when Admin creates
// a credential with malicious BaseURL, validation at save-time
// is the gate. We assert the public surface a future admin route
// will hit isStore.CreateCredential and not the resolver's
// validateAtDialTime silently — keeping the resolver permissive on
// empty is the established spec, but every non-empty write MUST
// be scrubbed at save.
//
// Failure repro: a path that writes raw operator strings into
// ResolvedCredential.BaseURL bypassing urlpolicy would not
// surface here but would during a real request. This test pins
// the resolver's pass-through contract; the save-time gate lives
// in internal/core and is covered by a separate handler test.
func TestResolverSyncBlockGuard(t *testing.T) {
	src := &fakeCredSource{
		cred: ResolvedCredential{
			Secret:  "sk-byok",
			BaseURL: "https://api.openai.com/v1", // already public
		},
		found: true,
	}
	cr := NewCredentialResolver(src, time.Minute, "")
	cred, found, err := cr.Resolve(context.Background(), "default", "u1", "openai")
	if err != nil || !found || cred.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("expected (sk-byok, https://..., true, nil), got %+v found=%v err=%v", cred, found, err)
	}
}

// Sentinel race-detector contract: race -run TestResolverCache
// ConcurrentSafe surfaces any unsynchronised mutation on
// cr.cache. We run with -race in CI; a regression here is loud.
func TestResolverCacheConcurrentSafe(t *testing.T) {
	src := &fakeCredSource{
		cred:  ResolvedCredential{Secret: "sk-shared"},
		found: true,
	}
	cr := NewCredentialResolver(src, time.Minute, "")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _, _ = cr.Resolve(context.Background(), "o", "u", "openai")
				cr.Invalidate()
			}
		}()
	}
	wg.Wait()
}
