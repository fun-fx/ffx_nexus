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

const anthropicVersion = "2023-06-01"

// Anthropic adapts the Anthropic Messages API to the OpenAI-compatible schema.
type Anthropic struct {
	apiKey  string
	baseURL string
	models  []string
	client  *http.Client
}

// NewAnthropic builds an Anthropic adapter.
func NewAnthropic(apiKey string, timeout time.Duration) *Anthropic {
	return &Anthropic{
		apiKey:  apiKey,
		baseURL: "https://api.anthropic.com/v1",
		models: []string{
			"claude-opus-4-1", "claude-sonnet-4-5", "claude-haiku-4-5",
			"claude-3-7-sonnet-latest", "claude-3-5-haiku-latest",
		},
		client: &http.Client{Timeout: timeout},
	}
}

// Name implements gateway.Provider.
func (a *Anthropic) Name() string { return "anthropic" }

// Models implements gateway.Provider.
func (a *Anthropic) Models() []string { return a.models }

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
	StopSeqs    []string           `json:"stop_sequences,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func toAnthropicRequest(req gateway.ChatCompletionRequest) anthropicRequest {
	ar := anthropicRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		StopSeqs:    req.Stop,
	}
	if req.MaxTokens != nil {
		ar.MaxTokens = *req.MaxTokens
	} else {
		ar.MaxTokens = 4096 // Anthropic requires max_tokens.
	}
	for _, m := range req.Messages {
		if m.Role == "system" {
			if ar.System != "" {
				ar.System += "\n\n"
			}
			ar.System += m.Content
			continue
		}
		ar.Messages = append(ar.Messages, anthropicMessage{Role: m.Role, Content: m.Content})
	}
	return ar
}

type anthropicResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (ar anthropicResponse) toOpenAI() *gateway.ChatCompletionResponse {
	var text strings.Builder
	for _, c := range ar.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	return &gateway.ChatCompletionResponse{
		ID:      ar.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   ar.Model,
		Choices: []gateway.Choice{{
			Index:        0,
			Message:      gateway.Message{Role: "assistant", Content: text.String()},
			FinishReason: mapAnthropicStop(ar.StopReason),
		}},
		Usage: gateway.Usage{
			PromptTokens:     ar.Usage.InputTokens,
			CompletionTokens: ar.Usage.OutputTokens,
			TotalTokens:      ar.Usage.InputTokens + ar.Usage.OutputTokens,
		},
	}
}

func mapAnthropicStop(s string) string {
	switch s {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return s
	}
}

// ChatCompletion implements gateway.Provider.
func (a *Anthropic) ChatCompletion(ctx context.Context, req gateway.ChatCompletionRequest) (*gateway.ChatCompletionResponse, error) {
	ar := toAnthropicRequest(req)
	ar.Stream = false
	resp, err := a.do(ctx, ar)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, providerError("anthropic", resp)
	}
	var ares anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&ares); err != nil {
		return nil, err
	}
	return ares.toOpenAI(), nil
}

// ChatCompletionStream implements gateway.Provider.
func (a *Anthropic) ChatCompletionStream(ctx context.Context, req gateway.ChatCompletionRequest) (<-chan gateway.StreamEvent, error) {
	ar := toAnthropicRequest(req)
	ar.Stream = true
	resp, err := a.do(ctx, ar)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, providerError("anthropic", resp)
	}

	out := make(chan gateway.StreamEvent)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		a.parseSSE(resp.Body, req.Model, out)
	}()
	return out, nil
}

func (a *Anthropic) do(ctx context.Context, ar anthropicRequest) (*http.Response, error) {
	body, err := json.Marshal(ar)
	if err != nil {
		return nil, err
	}
	apiKey, baseURL := a.apiKey, a.baseURL
	if c, ok := gateway.CallerCredentialFrom(ctx); ok {
		apiKey = c.Secret
		if c.BaseURL != "" {
			baseURL = strings.TrimRight(c.BaseURL, "/")
		}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	return a.client.Do(httpReq)
}

// parseSSE translates Anthropic streaming events into OpenAI-style chunks.
func (a *Anthropic) parseSSE(r io.ReadCloser, model string, out chan<- gateway.StreamEvent) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	created := time.Now().Unix()
	id := fmt.Sprintf("chatcmpl-%d", created)
	var usage gateway.Usage

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var evt struct {
			Type  string `json:"type"`
			Delta struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			continue
		}
		switch evt.Type {
		case "content_block_delta":
			if evt.Delta.Text != "" {
				out <- gateway.StreamEvent{Chunk: &gateway.ChatCompletionChunk{
					ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
					Choices: []gateway.ChunkChoice{{Index: 0, Delta: gateway.Delta{Content: evt.Delta.Text}}},
				}}
			}
		case "message_delta":
			if evt.Usage.OutputTokens > 0 {
				usage.CompletionTokens = evt.Usage.OutputTokens
			}
			if evt.Delta.StopReason != "" {
				out <- gateway.StreamEvent{Chunk: &gateway.ChatCompletionChunk{
					ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
					Choices: []gateway.ChunkChoice{{Index: 0, FinishReason: mapAnthropicStop(evt.Delta.StopReason)}},
					Usage:   &usage,
				}}
			}
		case "message_stop":
			out <- gateway.StreamEvent{Done: true}
			return
		}
	}
	if err := scanner.Err(); err != nil {
		out <- gateway.StreamEvent{Err: err}
		return
	}
	out <- gateway.StreamEvent{Done: true}
}
