package gateway

import "encoding/json"

// This file defines the OpenAI-compatible wire schema used as the canonical
// internal representation. Provider adapters translate between this schema and
// their native formats.

// ChatCompletionRequest is the OpenAI-compatible request body.
type ChatCompletionRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
	Stop        []string  `json:"stop,omitempty"`
	Tools       []Tool    `json:"tools,omitempty"`
	User        string    `json:"user,omitempty"`
	// ResponseFormat follows the OpenAI schema. When set to json_object or
	// json_schema it is forwarded upstream AND used by the schema guardrail to
	// validate the model's output. Left intact for providers that support it.
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	// NexusEval carries optional RAG inputs for async evaluation only. Never
	// forwarded to upstream providers — strip with ForProvider() before calling
	// OpenAI-compatible adapters that marshal the full struct.
	NexusEval *NexusEvalContext `json:"nexus_eval,omitempty"`
	// Extra preserves unknown fields so we can forward provider-specific params.
	Extra map[string]json.RawMessage `json:"-"`
}

// NexusEvalContext holds retrieval data for RAG metrics (hallucination,
// faithfulness). The gateway stores this on the trace and passes it to the
// async eval worker; it is not injected into the LLM prompt unless the client
// already included the chunks in messages.
type NexusEvalContext struct {
	Contexts  []string `json:"contexts,omitempty"`  // retrieved document chunks
	Reference string   `json:"reference,omitempty"` // ground-truth / expected answer
}

// ForProvider returns a copy safe to send upstream (Nexus-only fields removed).
func (r ChatCompletionRequest) ForProvider() ChatCompletionRequest {
	r.NexusEval = nil
	return r
}

// ResponseFormat is the OpenAI-compatible output format directive.
type ResponseFormat struct {
	Type       string          `json:"type"` // "text" | "json_object" | "json_schema"
	JSONSchema *JSONSchemaSpec `json:"json_schema,omitempty"`
}

// JSONSchemaSpec carries a named JSON Schema for structured outputs.
type JSONSchemaSpec struct {
	Name   string          `json:"name,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Strict bool            `json:"strict,omitempty"`
}

// WantsJSON reports whether the request asks for JSON output.
func (r *ResponseFormat) WantsJSON() bool {
	return r != nil && (r.Type == "json_object" || r.Type == "json_schema")
}

// SchemaBytes returns the raw JSON Schema if one was supplied (json_schema mode).
func (r *ResponseFormat) SchemaBytes() []byte {
	if r == nil || r.JSONSchema == nil {
		return nil
	}
	return r.JSONSchema.Schema
}

// Message is a single chat message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Tool describes a function the model may call.
type Tool struct {
	Type     string          `json:"type"`
	Function json.RawMessage `json:"function"`
}

// ToolCall is a model-requested function invocation.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ChatCompletionResponse is the OpenAI-compatible non-streaming response.
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice is one completion candidate.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage holds token accounting.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatCompletionChunk is a single streamed delta (OpenAI SSE format).
type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

// ChunkChoice is a streamed choice delta.
type ChunkChoice struct {
	Index        int    `json:"index"`
	Delta        Delta  `json:"delta"`
	FinishReason string `json:"finish_reason,omitempty"`
}

// Delta is the incremental content for a streamed chunk.
type Delta struct {
	Role      string     `json:"role,omitempty"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// StreamEvent is emitted on the streaming channel. Exactly one of Chunk or Err
// is set per event; Done signals normal end of stream.
type StreamEvent struct {
	Chunk *ChatCompletionChunk
	Err   error
	Done  bool
}

// APIError is the OpenAI-compatible error envelope.
type APIError struct {
	Error APIErrorBody `json:"error"`
}

// APIErrorBody is the body of an API error.
type APIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}
