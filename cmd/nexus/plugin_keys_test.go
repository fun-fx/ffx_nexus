package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ffxnexus/nexus/internal/evalplugin"
)

func TestConsoleKeyResolver_SetAndGetRoundTrip(t *testing.T) {
	r := newConsoleKeyResolver(nil)
	r.Set("langfuse-judge", map[string]string{
		"public_key": "pk-lf-abcdef1234567890",
		"secret_key": "sk-lf-zzzzzzzzzzzzzzzz",
	})

	got, ok := r.Get("langfuse-judge")
	if !ok {
		t.Fatalf("expected keys to be present, got ok=false")
	}
	if got["public_key"] != "pk-lf-abcdef1234567890" {
		t.Errorf("public_key mismatch: got %q", got["public_key"])
	}
	if got["secret_key"] != "sk-lf-zzzzzzzzzzzzzzzz" {
		t.Errorf("secret_key mismatch: got %q", got["secret_key"])
	}
}

func TestConsoleKeyResolver_ResolveRoundTrip(t *testing.T) {
	r := newConsoleKeyResolver(nil)
	r.Set("langfuse-judge", map[string]string{
		"public_key": "pk-lf-abc123",
		"secret_key": "sk-lf-xyz789",
	})

	creds, err := r.Resolve(context.Background(), evalplugin.AuthSpec{
		SecretRef: "langfuse-judge",
		KeyRef:    "public_key|secret_key",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(creds.Values) != 2 {
		t.Fatalf("expected 2 values, got %d", len(creds.Values))
	}
	if creds.Values[0] != "pk-lf-abc123" {
		t.Errorf("Values[0] mismatch: %q", creds.Values[0])
	}
	if creds.Values[1] != "sk-lf-xyz789" {
		t.Errorf("Values[1] mismatch: %q", creds.Values[1])
	}
}

func TestConsoleKeyResolver_NoAuthBlock(t *testing.T) {
	r := newConsoleKeyResolver(nil)
	creds, err := r.Resolve(context.Background(), evalplugin.AuthSpec{})
	if err != nil {
		t.Fatalf("self-hosted plugins should resolve to empty creds: %v", err)
	}
	if !creds.Empty() {
		t.Errorf("expected empty credentials, got %d values", len(creds.Values))
	}
}

func TestConsoleKeyResolver_NoKeysConfigured(t *testing.T) {
	r := newConsoleKeyResolver(nil)
	_, err := r.Resolve(context.Background(), evalplugin.AuthSpec{
		SecretRef: "langfuse-judge",
		KeyRef:    "public_key|secret_key",
	})
	if err == nil {
		t.Fatal("expected error when plugin has no keys configured")
	}
	if !strings.Contains(err.Error(), "langfuse-judge") {
		t.Errorf("error must name the plugin: %v", err)
	}
	if !strings.Contains(err.Error(), "Plugin Keys panel") {
		t.Errorf("error must guide operator to the UI: %v", err)
	}
}

func TestConsoleKeyResolver_PartialKeys(t *testing.T) {
	r := newConsoleKeyResolver(nil)
	r.Set("langfuse-judge", map[string]string{
		"public_key": "pk-lf-abc123",
		// secret_key intentionally omitted
	})
	_, err := r.Resolve(context.Background(), evalplugin.AuthSpec{
		SecretRef: "langfuse-judge",
		KeyRef:    "public_key|secret_key",
	})
	if err == nil {
		t.Fatal("expected error when one of the requested keys is missing")
	}
	if !strings.Contains(err.Error(), "secret_key") {
		t.Errorf("error must list the missing key name: %v", err)
	}
}

func TestConsoleKeyResolver_EmptyKeyRef(t *testing.T) {
	r := newConsoleKeyResolver(nil)
	r.Set("langfuse-judge", map[string]string{"public_key": "pk"})
	_, err := r.Resolve(context.Background(), evalplugin.AuthSpec{
		SecretRef: "langfuse-judge",
		KeyRef:    "",
	})
	if err == nil {
		t.Fatal("expected error when keyRef is empty even though keys are stored")
	}
}

func TestConsoleKeyResolver_Clear(t *testing.T) {
	r := newConsoleKeyResolver(nil)
	r.Set("langfuse-judge", map[string]string{"public_key": "pk"})
	r.Clear("langfuse-judge")
	if r.Has("langfuse-judge") {
		t.Errorf("expected keys to be cleared")
	}
	_, err := r.Resolve(context.Background(), evalplugin.AuthSpec{
		SecretRef: "langfuse-judge",
		KeyRef:    "public_key",
	})
	if err == nil {
		t.Fatal("expected error after Clear")
	}
}

func TestConsoleKeyResolver_SetStripsEmpties(t *testing.T) {
	r := newConsoleKeyResolver(nil)
	r.Set("langfuse-judge", map[string]string{
		"public_key": "pk-lf-abc",
		"unused":     "",
	})
	got, ok := r.Get("langfuse-judge")
	if !ok {
		t.Fatalf("expected keys present")
	}
	if _, hit := got["unused"]; hit {
		t.Errorf("expected empty-value key to be stripped")
	}
}

func TestConsoleKeyResolver_Has(t *testing.T) {
	r := newConsoleKeyResolver(nil)
	if r.Has("ghost") {
		t.Errorf("Has on empty resolver must be false")
	}
	r.Set("langfuse-judge", map[string]string{"public_key": "pk"})
	if !r.Has("langfuse-judge") {
		t.Errorf("Has on configured plugin must be true")
	}
	r.Set("langfuse-judge", nil)
	if r.Has("langfuse-judge") {
		t.Errorf("Has after Set(nil) must be false")
	}
}

func TestConsoleKeyResolver_ConcurrentAccess(t *testing.T) {
	r := newConsoleKeyResolver(nil)
	var wg sync.WaitGroup
	const goroutines = 16
	const iters = 200

	for i := 0; i < goroutines; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				r.Set("plugin-a", map[string]string{"k1": "v1"})
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				_, _ = r.Resolve(context.Background(), evalplugin.AuthSpec{
					SecretRef: "plugin-a",
					KeyRef:    "k1|k2",
				})
				_, _ = r.Get("plugin-a")
				_ = r.Has("plugin-a")
			}
		}(i)
	}
	wg.Wait()

	creds, err := r.Resolve(context.Background(), evalplugin.AuthSpec{
		SecretRef: "plugin-a",
		KeyRef:    "k1",
	})
	if err != nil {
		t.Fatalf("Resolve after concurrent Set/Resolve cycles: %v", err)
	}
	if len(creds.Values) != 1 || creds.Values[0] != "v1" {
		t.Errorf("post-race state corrupted: %+v", creds)
	}
}

func TestConsoleKeyResolver_ErrorMessagesDoNotLeakValue(t *testing.T) {
	r := newConsoleKeyResolver(nil)
	// Don't set the keys; the error path should not echo plugin values.
	_, err := r.Resolve(context.Background(), evalplugin.AuthSpec{
		SecretRef: "langfuse-judge",
		KeyRef:    "public_key|secret_key",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "pk-lf-") || strings.Contains(err.Error(), "sk-lf-") {
		t.Errorf("error message must not contain raw key bytes: %v", err)
	}
}

func TestMaskKey(t *testing.T) {
	cases := map[string]struct {
		in     string
		expect func(t *testing.T, got string)
	}{
		"empty becomes empty": {in: "", expect: func(t *testing.T, got string) {
			if got != "" {
				t.Errorf("empty -> empty; got %q", got)
			}
		}},
		"short becomes ***": {in: "abcd", expect: func(t *testing.T, got string) {
			if got != "***" {
				t.Errorf("short token -> ***; got %q", got)
			}
		}},
		"pk-lf style keeps prefix and tail": {
			in: "pk-lf-abcdef1234567890",
			expect: func(t *testing.T, got string) {
				if !strings.HasPrefix(got, "pk-") {
					t.Errorf("must keep prefix pk-; got %q", got)
				}
				if !strings.Contains(got, "***") {
					t.Errorf("must mask middle; got %q", got)
				}
				if !strings.HasSuffix(got, "7890") {
					t.Errorf("must keep trailing 4 chars; got %q", got)
				}
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) { tc.expect(t, maskKey(tc.in)) })
	}
}

// errorIs is a small helper mirroring errors.Is to avoid importing
// errors twice for one assertion in a focused test.
func errorIs(err, target error) bool {
	return errors.Is(err, target)
}

var _ = errorIs // keep the helper exported-shaped for future callers
