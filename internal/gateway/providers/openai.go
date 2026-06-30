// Package providers implements LLM backend adapters that translate the
// canonical OpenAI-compatible schema to/from each provider's native API.
package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ffxnexus/nexus/internal/gateway"
)

// OpenAI adapts the OpenAI Chat Completions API. Because our canonical schema
// is OpenAI-compatible, this adapter is essentially a typed pass-through.
//
// It also implements the optional ModerationsProvider and ImageGenerationProvider
// capabilities so a single OpenAI credential can serve the full
// /v1/{chat,embeddings,moderations,images/generations} surface.
type OpenAI struct {
	apiKey           string
	baseURL          string
	models           []string
	embeddingModels  []string
	moderationModels []string
	imageModels      []string
	client           *http.Client
}

// NewOpenAI builds an OpenAI adapter.
func NewOpenAI(apiKey, baseURL string, timeout time.Duration) *OpenAI {
	return &OpenAI{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		models: []string{
			"gpt-4o", "gpt-4o-mini", "gpt-4.1", "gpt-4.1-mini", "o3", "o4-mini",
		},
		embeddingModels: []string{
			"text-embedding-3-small", "text-embedding-3-large", "text-embedding-ada-002",
		},
		moderationModels: []string{
			"omni-moderation-latest", "omni-moderation-2024-09-26",
			"text-moderation-latest", "text-moderation-stable",
		},
		imageModels: []string{
			"dall-e-2", "dall-e-3", "gpt-image-1",
		},
		client: &http.Client{Timeout: timeout},
	}
}

// EmbeddingModels implements gateway.EmbeddingsProvider.
func (o *OpenAI) EmbeddingModels() []string { return o.embeddingModels }

// Embed performs an OpenAI /v1/embeddings call. Honors per-request credential
// overrides the same way ChatCompletion does (BYOK).
func (o *OpenAI) Embed(ctx context.Context, req gateway.EmbeddingRequest) (*gateway.EmbeddingResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	apiKey, baseURL := o.apiKey, o.baseURL
	if c, ok := gateway.CallerCredentialFrom(ctx); ok {
		apiKey = c.Secret
		if c.BaseURL != "" {
			baseURL = strings.TrimRight(c.BaseURL, "/")
		}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, providerError("openai", resp)
	}
	var out gateway.EmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Name implements gateway.Provider.
func (o *OpenAI) Name() string { return "openai" }

// Models implements gateway.Provider.
func (o *OpenAI) Models() []string { return o.models }

// ChatCompletion implements gateway.Provider.
func (o *OpenAI) ChatCompletion(ctx context.Context, req gateway.ChatCompletionRequest) (*gateway.ChatCompletionResponse, error) {
	req = req.ForProvider()
	req.Stream = false
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpResp, err := o.do(ctx, body)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode >= 400 {
		return nil, providerError("openai", httpResp)
	}
	var resp gateway.ChatCompletionResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ChatCompletionStream implements gateway.Provider.
func (o *OpenAI) ChatCompletionStream(ctx context.Context, req gateway.ChatCompletionRequest) (<-chan gateway.StreamEvent, error) {
	req = req.ForProvider()
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpResp, err := o.do(ctx, body)
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode >= 400 {
		defer httpResp.Body.Close()
		return nil, providerError("openai", httpResp)
	}

	out := make(chan gateway.StreamEvent)
	go func() {
		defer close(out)
		defer httpResp.Body.Close()
		parseOpenAISSE(httpResp.Body, out)
	}()
	return out, nil
}

func (o *OpenAI) do(ctx context.Context, body []byte) (*http.Response, error) {
	return o.doJSON(ctx, "/chat/completions", body)
}

// doJSON performs a POST against the OpenAI-compatible base URL + path, honoring
// per-request credential overrides (BYOK). Used by every OpenAI capability
// (chat, embeddings, moderations, images).
func (o *OpenAI) doJSON(ctx context.Context, path string, body []byte) (*http.Response, error) {
	apiKey, baseURL := o.apiKey, o.baseURL
	if c, ok := gateway.CallerCredentialFrom(ctx); ok {
		apiKey = c.Secret
		if c.BaseURL != "" {
			baseURL = strings.TrimRight(c.BaseURL, "/")
		}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	return o.client.Do(httpReq)
}

// ModerationModels implements gateway.ModerationsProvider.
func (o *OpenAI) ModerationModels() []string { return o.moderationModels }

// ImageModels implements gateway.ImageGenerationProvider.
func (o *OpenAI) ImageModels() []string { return o.imageModels }

// Moderate posts the request to /v1/moderations and decodes the response.
// The default model (omni-moderation-latest) is filled in when the caller
// leaves Model empty — matches OpenAI's behavior.
func (o *OpenAI) Moderate(ctx context.Context, req gateway.ModerationRequest) (*gateway.ModerationResponse, error) {
	if req.Model == "" {
		req.Model = "omni-moderation-latest"
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpResp, err := o.doJSON(ctx, "/moderations", body)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode >= 400 {
		return nil, providerError("openai", httpResp)
	}
	var out gateway.ModerationResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GenerateImages posts to /v1/images/generations and decodes the response.
// Falls back to dall-e-3 if no model is specified — the most popular image
// model that has stable OpenAI API support.
func (o *OpenAI) GenerateImages(ctx context.Context, req gateway.ImageGenerationRequest) (*gateway.ImageGenerationResponse, error) {
	if req.Model == "" {
		req.Model = "dall-e-3"
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpResp, err := o.doJSON(ctx, "/images/generations", body)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode >= 400 {
		return nil, providerError("openai", httpResp)
	}
	var out gateway.ImageGenerationResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// parseOpenAISSE reads an OpenAI server-sent event stream and emits chunks.
// Shared by any provider that speaks the OpenAI SSE wire format.
func parseOpenAISSE(r io.Reader, out chan<- gateway.StreamEvent) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			out <- gateway.StreamEvent{Done: true}
			return
		}
		var chunk gateway.ChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			out <- gateway.StreamEvent{Err: fmt.Errorf("decode chunk: %w", err)}
			return
		}
		out <- gateway.StreamEvent{Chunk: &chunk}
	}
	if err := scanner.Err(); err != nil {
		out <- gateway.StreamEvent{Err: err}
		return
	}
	out <- gateway.StreamEvent{Done: true}
}

func providerError(provider string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	return fmt.Errorf("%s upstream error (%d): %s", provider, resp.StatusCode, strings.TrimSpace(string(b)))
}
