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
type OpenAI struct {
	apiKey  string
	baseURL string
	models  []string
	client  *http.Client
}

// NewOpenAI builds an OpenAI adapter.
func NewOpenAI(apiKey, baseURL string, timeout time.Duration) *OpenAI {
	return &OpenAI{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		models: []string{
			"gpt-4o", "gpt-4o-mini", "gpt-4.1", "gpt-4.1-mini", "o3", "o4-mini",
		},
		client: &http.Client{Timeout: timeout},
	}
}

// Name implements gateway.Provider.
func (o *OpenAI) Name() string { return "openai" }

// Models implements gateway.Provider.
func (o *OpenAI) Models() []string { return o.models }

// ChatCompletion implements gateway.Provider.
func (o *OpenAI) ChatCompletion(ctx context.Context, req gateway.ChatCompletionRequest) (*gateway.ChatCompletionResponse, error) {
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
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	return o.client.Do(httpReq)
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
