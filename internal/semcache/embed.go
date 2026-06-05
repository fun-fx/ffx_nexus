// Package semcache provides a semantic response cache backed by Redis. Similar
// prompts (by embedding cosine similarity) return a stored completion without an
// upstream LLM call, cutting cost and latency. Lookups run on the request hot
// path but are bounded: one embedding call plus a linear scan over a capped
// per-model entry list in Redis.
package semcache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// Config controls semantic cache behaviour.
type Config struct {
	Enabled            bool
	TTL                time.Duration // entry lifetime; 0 = 24h default
	Threshold          float64       // min cosine similarity for a hit; 0 = 0.92
	MaxEntriesPerModel int           // cap per model list; 0 = 500
	EmbeddingsURL      string        // OpenAI-compatible /v1/embeddings base
	EmbeddingsModel    string
	EmbeddingsAPIKey   string
	Timeout            time.Duration
}

// Embedder returns a vector for a text prompt.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// OpenAIEmbedder calls an OpenAI-compatible embeddings endpoint.
type OpenAIEmbedder struct {
	baseURL string
	model   string
	apiKey  string
	hc      *http.Client
}

// NewOpenAIEmbedder builds an embedder. baseURL is e.g. http://host:11434/v1.
// The timeout is a hard ceiling on the hot-path embedding call; keep it tight so
// a degraded embeddings endpoint degrades to no-cache rather than stalling.
func NewOpenAIEmbedder(baseURL, model, apiKey string, timeout time.Duration) *OpenAIEmbedder {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	return &OpenAIEmbedder{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		apiKey:  apiKey,
		hc:      &http.Client{Timeout: timeout},
	}
}

func (e *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": e.model, "input": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	if len(parsed.Data) == 0 || len(parsed.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embeddings returned empty vector")
	}
	return parsed.Data[0].Embedding, nil
}

// cosineSimilarity returns the cosine similarity of two equal-length vectors.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
