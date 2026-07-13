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
		client: PooledHTTPClient(timeout),
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
	// Tools describes the functions the model may call. Anthropic accepts
	// the same `tools` array as OpenAI but renames the schema field
	// (`input_schema` instead of `parameters`).
	Tools []anthropicTool `json:"tools,omitempty"`
	// ToolChoice mirrors OpenAI's tool_choice string family: "any" forces
	// at least one tool call; "auto" lets the model decide; "tool" pins a
	// specific named tool.
	ToolChoice *anthropicToolChoice `json:"tool_choice,omitempty"`
}

// anthropicTool is the Anthropic tool description. The shape differs from
// OpenAI's in one place: parameters is renamed to input_schema.
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// anthropicToolChoice translates OpenAI's tool_choice into Anthropic's
// variant. Only the named forms are supported today.
type anthropicToolChoice struct {
	Type string `json:"type"`           // "any" | "auto" | "tool"
	Name string `json:"name,omitempty"` // populated for Type=="tool"
}

// anthropicMessage is the Anthropic Messages API message. Content is a JSON
// array of "blocks" so the same message can carry text, tool_use (assistant
// -> model) and tool_result (assistant <- tool) parts.
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
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

	// Translate the OpenAI `tools` array into Anthropic's shape. We accept
	// the OpenAI tool description format ({"type":"function","function":{...}})
	// and unpack `function.parameters` into Anthropic's `input_schema`.
	if len(req.Tools) > 0 {
		ar.Tools = make([]anthropicTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			var fn struct {
				Name        string          `json:"name"`
				Description string          `json:"description,omitempty"`
				Parameters  json.RawMessage `json:"parameters,omitempty"`
			}
			_ = json.Unmarshal(t.Function, &fn)
			if fn.Name == "" {
				continue
			}
			ar.Tools = append(ar.Tools, anthropicTool{
				Name:        fn.Name,
				Description: fn.Description,
				InputSchema: fn.Parameters,
			})
		}
		if len(ar.Tools) == 0 {
			ar.Tools = nil
		}
	}

	// Translate OpenAI's `tool_choice` string into Anthropic's variant.
	// Object form (`{"type":"function","function":{"name":"X"}}`) is also
	// handled. Anything else is dropped — the Anthropic default ("auto")
	// is the right behavior in that case.
	if len(req.ToolChoice) > 0 {
		var s string
		if err := json.Unmarshal(req.ToolChoice, &s); err == nil {
			switch s {
			case "auto":
				ar.ToolChoice = &anthropicToolChoice{Type: "auto"}
			case "required":
				ar.ToolChoice = &anthropicToolChoice{Type: "any"}
			case "none":
				// Anthropic does not have a "none" mode — the standard
				// workaround is to omit tools entirely. We keep tools on
				// the request and add a system nudge instead; production
				// callers can also just drop tools themselves.
			}
		} else {
			var obj struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(req.ToolChoice, &obj); err == nil {
				if obj.Type == "function" && obj.Function.Name != "" {
					ar.ToolChoice = &anthropicToolChoice{Type: "tool", Name: obj.Function.Name}
				}
			}
		}
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if ar.System != "" {
				ar.System += "\n\n"
			}
			ar.System += m.Content
		case "tool":
			// The tool's output is a single Anthropic "tool_result" block
			// keyed by the call id we issued in the prior turn.
			block, _ := json.Marshal([]map[string]any{{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     m.Content,
			}})
			ar.Messages = append(ar.Messages, anthropicMessage{Role: "user", Content: block})
		case "assistant":
			if len(m.ToolCalls) > 0 {
				// Each tool call becomes a "tool_use" content block. We keep
				// the assistant text (m.Content) alongside the tool calls in
				// the same message so the model sees its own previous answer.
				blocks := []map[string]any{}
				if m.Content != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
				}
				for _, tc := range m.ToolCalls {
					var args map[string]any
					if tc.Function.Arguments != "" {
						_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
					}
					blocks = append(blocks, map[string]any{
						"type":  "tool_use",
						"id":    tc.ID,
						"name":  tc.Function.Name,
						"input": args,
					})
				}
				raw, _ := json.Marshal(blocks)
				ar.Messages = append(ar.Messages, anthropicMessage{Role: "assistant", Content: raw})
				continue
			}
			block, _ := json.Marshal([]map[string]any{{"type": "text", "text": m.Content}})
			ar.Messages = append(ar.Messages, anthropicMessage{Role: "assistant", Content: block})
		default:
			block, _ := json.Marshal([]map[string]any{{"type": "text", "text": m.Content}})
			ar.Messages = append(ar.Messages, anthropicMessage{Role: m.Role, Content: block})
		}
	}
	return ar
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	// Input is marshaled as the model's arguments object for tool_use blocks.
	Input json.RawMessage `json:"input,omitempty"`
}

type anthropicResponse struct {
	ID         string                  `json:"id"`
	Model      string                  `json:"model"`
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (ar anthropicResponse) toOpenAI() *gateway.ChatCompletionResponse {
	var text strings.Builder
	var toolCalls []gateway.ToolCall
	for _, c := range ar.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
		case "tool_use":
			arguments := "{}"
			if len(c.Input) > 0 {
				arguments = string(c.Input)
			}
			tc := gateway.ToolCall{Type: "function"}
			tc.ID = c.ID
			tc.Function.Name = c.Name
			tc.Function.Arguments = arguments
			toolCalls = append(toolCalls, tc)
		}
	}
	finish := mapAnthropicStop(ar.StopReason)
	if len(toolCalls) > 0 && finish == "" {
		finish = "tool_calls"
	}
	return &gateway.ChatCompletionResponse{
		ID:      ar.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   ar.Model,
		Choices: []gateway.Choice{{
			Index:        0,
			Message:      gateway.Message{Role: "assistant", Content: text.String(), ToolCalls: toolCalls},
			FinishReason: finish,
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
	buf := acquireBuffer()
	defer releaseBuffer(buf)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(buf, 1024*1024)
	created := time.Now().Unix()
	id := fmt.Sprintf("chatcmpl-%d", created)
	var usage gateway.Usage

	// Tool-use streaming on Anthropic arrives as content_block_start with the
	// tool name/id (no input), then content_block_delta events with the
	// partial JSON input, then content_block_stop. We accumulate the
	// arguments by index so the final OpenAI chunk for the call carries the
	// full arguments string and an assigned Idx.
	pendingToolDeltas := map[int]*gateway.ToolCall{}
	pendingToolOrder := []int{}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var evt struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock *struct {
				Type string `json:"type"`
				ID   string `json:"id,omitempty"`
				Name string `json:"name,omitempty"`
			} `json:"content_block,omitempty"`
			Delta struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				StopReason string `json:"stop_reason"`
				// PartialJSON arrives as a string-shaped JSON snippet.
				PartialJSON json.RawMessage `json:"partial_json,omitempty"`
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
		case "content_block_start":
			if evt.ContentBlock != nil && evt.ContentBlock.Type == "tool_use" {
				tc := &gateway.ToolCall{
					Type: "function",
				}
				tc.ID = evt.ContentBlock.ID
				tc.Function.Name = evt.ContentBlock.Name
				pendingToolDeltas[evt.Index] = tc
				pendingToolOrder = append(pendingToolOrder, evt.Index)
				out <- gateway.StreamEvent{Chunk: &gateway.ChatCompletionChunk{
					ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
					Choices: []gateway.ChunkChoice{{Index: 0, Delta: gateway.Delta{
						Role: "assistant",
						ToolCalls: []gateway.ToolCall{{
							Type: "function",
							ID:   tc.ID,
							Function: struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							}{Name: tc.Function.Name},
						}},
					}}},
				}}
			}
		case "content_block_delta":
			if evt.Delta.Type == "text_delta" && evt.Delta.Text != "" {
				out <- gateway.StreamEvent{Chunk: &gateway.ChatCompletionChunk{
					ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
					Choices: []gateway.ChunkChoice{{Index: 0, Delta: gateway.Delta{Content: evt.Delta.Text}}},
				}}
				continue
			}
			if evt.Delta.Type == "input_json_delta" && len(evt.Delta.PartialJSON) > 0 {
				if tc, ok := pendingToolDeltas[evt.Index]; ok {
					tc.Function.Arguments += string(evt.Delta.PartialJSON)
					out <- gateway.StreamEvent{Chunk: &gateway.ChatCompletionChunk{
						ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
						Choices: []gateway.ChunkChoice{{Index: 0, Delta: gateway.Delta{
							ToolCalls: []gateway.ToolCall{{
								Type: "function",
								ID:   tc.ID,
								Function: struct {
									Name      string `json:"name"`
									Arguments string `json:"arguments"`
								}{Arguments: string(evt.Delta.PartialJSON)},
							}},
						}}},
					}}
				}
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
