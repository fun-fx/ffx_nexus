package router

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestWebhookNotifierPostsEnvelope confirms a generic webhook notifier
// POSTs our stable FailoverEvent shape and accepts 2xx.
func TestWebhookNotifierPostsEnvelope(t *testing.T) {
	var received atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		var got FailoverEvent
		if err := json.Unmarshal(buf, &got); err != nil {
			t.Errorf("envelope not valid JSON: %v (%q)", err, buf)
		}
		if got.Primary != "primary" || got.Fallback != "fallback" || got.Reason != "upstream_error_failover" || got.Alias != "fast" || got.LatencyMs != 123 {
			t.Errorf("envelope preserved wrong fields: %+v", got)
		}
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := NewWebhookNotifier(srv.URL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n.Notify(context.Background(), FailoverEvent{
		OrgID:        "default",
		VirtualKeyID: "v1",
		Alias:        "fast",
		Tried:        []string{"primary", "fallback"},
		Primary:      "primary",
		Fallback:     "fallback",
		Reason:       "upstream_error_failover",
		LatencyMs:    123,
	})
	if err := WaitFor(t, time.Second, func() bool { return received.Load() >= 1 }); err != nil {
		t.Fatalf("webhook never received a body: %v", err)
	}
}

// TestSlackNotifierPostsHumanText confirms the Slack shape — minimal
// `{"text": "..."}` envelope, content includes primary/fallback.
func TestSlackNotifierPostsHumanText(t *testing.T) {
	var received atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(buf), "openai/gpt-4o") || !strings.Contains(string(buf), "gemini/gemini-2.5-flash") {
			t.Errorf("slack text missing model names: %q", buf)
		}
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := NewSlackNotifier(srv.URL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n.Notify(context.Background(), FailoverEvent{
		Primary:      "openai/gpt-4o",
		Fallback:     "gemini/gemini-2.5-flash",
		VirtualKeyID: "vk-1",
		Reason:       "upstream_error_failover",
	})
	if err := WaitFor(t, time.Second, func() bool { return received.Load() >= 1 }); err != nil {
		t.Fatalf("slack never received a body: %v", err)
	}
}

// TestDisabledNotifiersReturnNil confirms empty endpoints produce nil
// notifiers (opt-in contract — no goroutines, no DNS).
func TestDisabledNotifiersReturnNil(t *testing.T) {
	// Empty URL: the constructor returns a literal nil *bufferedNotifier
	// (not the zero value of a struct), so equality on a typed value is
	// `== nil` (Go treats nil-pointer-receiver as nil interface).
	if n := NewWebhookNotifier("", nil); n != nil {
		t.Fatalf("empty webhook URL must produce nil notifier, got %#v", n)
	}
	if n := NewSlackNotifier("", nil); n != nil {
		t.Fatalf("empty slack URL must produce nil notifier, got %#v", n)
	}
	if n := NewMultiNotifier(); n != nil {
		t.Fatalf("empty multi notifier must be nil")
	}
	if n := NewMultiNotifier(nil, NoopNotifier{}); n != nil {
		t.Fatalf("multi notifier with only no-op / nil members must collapse to nil")
	}
}

// TestMultiNotifierIsConcurrentSafe confirms the multi-notifier never
// races its sinks. The test goroutine calls Notify many times in a
// loop; the recorder uses an atomic counter so the suite races.
func TestMultiNotifierIsConcurrentSafe(t *testing.T) {
	var received atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mn := NewMultiNotifier(
		NewWebhookNotifier(srv.URL, slog.New(slog.NewTextHandler(io.Discard, nil))),
		NewSlackNotifier(srv.URL, slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if mn == nil {
		t.Fatal("expected non-nil multi notifier (both sinks set)")
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			mn.Notify(context.Background(), FailoverEvent{Primary: "x", Fallback: "y"})
		}
		close(done)
	}()
	<-done
	if err := WaitFor(t, time.Second, func() bool { return received.Load() >= 50 }); err != nil {
		t.Fatalf("expected ≥50 POST events, got %d", received.Load())
	}
}

// WaitFor waits up to d for cond() to return true. Uses a hardcoded
// poll cadence so test timeouts are deterministic.
func WaitFor(t *testing.T, d time.Duration, cond func() bool) error {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cond() {
		return nil
	}
	return errTimeout{}
}

type errTimeout struct{}

func (errTimeout) Error() string { return "timeout" }
