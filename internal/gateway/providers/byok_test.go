package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/gateway"
)

// TestOpenAIBYOKInjection verifies the OpenAI adapter uses a per-request
// credential override from the context (Authorization header + base URL) instead
// of its process-wide configured key.
func TestOpenAIBYOKInjection(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(gateway.ChatCompletionResponse{
			ID: "x", Model: "gpt-4o-mini",
			Choices: []gateway.Choice{{Message: gateway.Message{Role: "assistant", Content: "ok"}}},
		})
	}))
	defer srv.Close()

	// Configured with a "shared" key + a bogus base URL that should be overridden.
	o := NewOpenAI("sk-shared", "http://127.0.0.1:1/should-not-be-used", 5*time.Second)

	ctx := gateway.WithCallerCredential(context.Background(), gateway.CallerCredential{
		Secret:  "sk-user-byok",
		BaseURL: srv.URL,
		Source:  "user",
	})
	resp, err := o.ChatCompletion(ctx, gateway.ChatCompletionRequest{
		Model:    "gpt-4o-mini",
		Messages: []gateway.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content != "ok" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if gotAuth != "Bearer sk-user-byok" {
		t.Fatalf("expected per-request key, got %q", gotAuth)
	}
}

// TestOpenAISharedKeyDefault verifies that without a context override the
// adapter falls back to its configured key.
func TestOpenAISharedKeyDefault(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(gateway.ChatCompletionResponse{
			ID: "x", Model: "gpt-4o-mini",
			Choices: []gateway.Choice{{Message: gateway.Message{Role: "assistant", Content: "ok"}}},
		})
	}))
	defer srv.Close()

	o := NewOpenAI("sk-shared", srv.URL, 5*time.Second)
	if _, err := o.ChatCompletion(context.Background(), gateway.ChatCompletionRequest{
		Model:    "gpt-4o-mini",
		Messages: []gateway.Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if gotAuth != "Bearer sk-shared" {
		t.Fatalf("expected shared key, got %q", gotAuth)
	}
}
