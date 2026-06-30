package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/gateway"
)

// TestOpenAIEmbedPassThrough verifies the embeddings adapter forwards the
// upstream /v1/embeddings response, including a single-element input.
func TestOpenAIEmbedPassThrough(t *testing.T) {
	var (
		gotAuth string
		gotPath string
		gotBody string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		_ = json.NewEncoder(w).Encode(gateway.EmbeddingResponse{
			Object: "list",
			Model:  "text-embedding-3-small",
			Data: []gateway.EmbeddingItem{{
				Object:    "embedding",
				Index:     0,
				Embedding: []float32{0.1, 0.2, 0.3},
			}},
			Usage: gateway.EmbeddingTokenUsage{PromptTokens: 2, TotalTokens: 2},
		})
	}))
	defer srv.Close()

	o := NewOpenAI("sk-shared", srv.URL, 5*time.Second)
	resp, err := o.Embed(context.Background(), gateway.EmbeddingRequest{
		Model: "text-embedding-3-small",
		Input: json.RawMessage(`["hello","world"]`),
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotAuth != "Bearer sk-shared" {
		t.Fatalf("auth header = %q, want sk-shared", gotAuth)
	}
	if gotPath != "/embeddings" {
		t.Fatalf("upstream path = %q, want /embeddings", gotPath)
	}
	if !strings.Contains(gotBody, `"model":"text-embedding-3-small"`) {
		t.Fatalf("body did not include model: %q", gotBody)
	}
	if len(resp.Data) != 1 || len(resp.Data[0].Embedding) != 3 {
		t.Fatalf("unexpected embeddings payload: %+v", resp)
	}
	if resp.Usage.TotalTokens != 2 {
		t.Fatalf("usage token count = %d, want 2", resp.Usage.TotalTokens)
	}
}

// TestOpenAIEmbedBYOK ensures a per-call credential override flows through to
// the upstream /v1/embeddings request — matching the chat-completion behavior.
func TestOpenAIEmbedBYOK(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(gateway.EmbeddingResponse{Object: "list", Model: "text-embedding-3-small"})
	}))
	defer srv.Close()

	o := NewOpenAI("sk-shared", "http://127.0.0.1:1/", 5*time.Second)
	ctx := gateway.WithCallerCredential(context.Background(), gateway.CallerCredential{
		Secret:  "sk-user-byok",
		BaseURL: srv.URL,
		Source:  "user",
	})
	if _, err := o.Embed(ctx, gateway.EmbeddingRequest{
		Model: "text-embedding-3-small",
		Input: json.RawMessage(`"hi"`),
	}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotAuth != "Bearer sk-user-byok" {
		t.Fatalf("auth header = %q, want sk-user-byok (BYOK)", gotAuth)
	}
}
