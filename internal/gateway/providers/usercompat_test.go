package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ffxnexus/nexus/internal/gateway"
)

// fakeReader satisfies the small surface of OpenAICompat.NewOpenAICompat
// input by simulating upstream. We use httptest to capture the forwarded
// request and assert the model id was stripped of the "user/<name>/" prefix.
func TestUserCompatStripsPrefixOnChatCompletion(t *testing.T) {
	var capturedPath string
	var capturedBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":0,"model":"raw","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	inner := NewOpenAICompat("myprov", "secret", upstream.URL,
		[]string{"raw"}, []string{"emb-raw"}, nil, nil, 0)
	uc := NewUserCompat(inner)

	if got := uc.Name(); got != "myprov" {
		t.Fatalf("name: got %q, want myprov", got)
	}
	models := uc.Models()
	if len(models) != 1 || models[0] != "user/myprov/raw" {
		t.Fatalf("Models() = %v; want [user/myprov/raw]", models)
	}
	if emb := uc.EmbeddingModels(); len(emb) != 1 || emb[0] != "user/myprov/emb-raw" {
		t.Fatalf("EmbeddingModels() = %v; want [user/myprov/emb-raw]", emb)
	}

	resp, err := uc.ChatCompletion(context.Background(), gateway.ChatCompletionRequest{
		Model: "user/myprov/raw",
	})
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if resp == nil {
		t.Fatal("nil response")
	}
	if capturedPath != "/chat/completions" {
		t.Fatalf("upstream path: got %q, want /chat/completions", capturedPath)
	}
	if got := capturedBody["model"]; got != "raw" {
		t.Fatalf("forwarded model: got %v, want raw", got)
	}
}

// TestUserCompatModelWithoutPrefixUnchanged ensures the wrapper is a no-op
// when the caller forgot the namespacing prefix (defensive: the upstream
// will then reject the model, but we do not silently re-prefix).
func TestUserCompatModelWithoutPrefixUnchanged(t *testing.T) {
	var forwarded string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		forwarded, _ = body["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":0,"model":"","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	inner := NewOpenAICompat("myprov", "secret", upstream.URL,
		[]string{"raw"}, nil, nil, nil, 0)
	uc := NewUserCompat(inner)

	_, err := uc.ChatCompletion(context.Background(), gateway.ChatCompletionRequest{
		Model: "raw",
	})
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if forwarded != "raw" {
		t.Fatalf("forwarded model: got %q, want raw (prefix not present, no strip)", forwarded)
	}
}
