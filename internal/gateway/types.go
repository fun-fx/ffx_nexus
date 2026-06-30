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

	// ToolChoice follows the OpenAI v1 spec:
	//   "none"                       — model must not call any tool
	//   "auto"                       — model decides (default)
	//   "required"                   — model must call at least one tool
	//   {"type":"function","function":{"name":"X"}} — force a specific tool
	//
	// Captured as a raw JSON value so we preserve the string vs object shape
	// exactly across the wire. Adapters translate it to their native
	// equivalent (e.g. Anthropic's tool_choice variant) or drop it where
	// the provider does not support it.
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`

	// ParallelToolCalls mirrors the OpenAI 1.1+ field. nil means "let the
	// provider default"; false disables parallel; true forces the provider
	// to emit as many tool calls as it can in one turn.
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
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

// EmbeddingRequest is the OpenAI-compatible /v1/embeddings request body.
// Input may be a string or an array of strings/tokens; the union is captured as
// raw JSON and forwarded to providers that also accept that shape. We validate
// upstream-side format support lazily: most providers expect []string.
//
// See https://platform.openai.com/docs/api-reference/embeddings/create
type EmbeddingRequest struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	EncodingFormat string          `json:"encoding_format,omitempty"` // "float" | "base64"
	User           string          `json:"user,omitempty"`
	Dimensions     *int            `json:"dimensions,omitempty"`
}

// EmbeddingResponse is the OpenAI-compatible /v1/embeddings response body.
type EmbeddingResponse struct {
	Object string              `json:"object"` // always "list"
	Data   []EmbeddingItem     `json:"data"`
	Model  string              `json:"model"`
	Usage  EmbeddingTokenUsage `json:"usage"`
}

// EmbeddingItem is one vector in the embeddings response.
type EmbeddingItem struct {
	Object    string    `json:"object"` // always "embedding"
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

// EmbeddingTokenUsage is the token accounting for an embeddings call.
type EmbeddingTokenUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// InputItem is a single entry of the OpenAI Responses API `input` array.
// We support the common subset: text messages, role-prefixed messages, and
// tool-call / tool-result exchanges. Anything unknown is preserved as raw JSON
// in Extra so we can forward it to providers that understand more shapes.
//
// See https://platform.openai.com/docs/api-reference/responses/create
type InputItem struct {
	Type      string                     `json:"type,omitempty"` // "message" | "function_call" | "function_call_output" | ...
	Role      string                     `json:"role,omitempty"` // "user" | "assistant" | "system" | "developer"
	Content   json.RawMessage            `json:"content,omitempty"`
	Name      string                     `json:"name,omitempty"` // for function_call_output
	CallID    string                     `json:"call_id,omitempty"`
	Arguments string                     `json:"arguments,omitempty"` // for function_call
	Output    string                     `json:"output,omitempty"`    // for function_call_output
	Extra     map[string]json.RawMessage `json:"-"`
}

// ResponsesRequest is the OpenAI Responses API request body. It is intentionally
// a superset of ChatCompletionRequest fields so the gateway can translate it
// into a normal /v1/chat/completions request internally (which every backend
// understands), then unwrap the response back into the Responses shape.
type ResponsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"` // string | []InputItem
	Instructions    string          `json:"instructions,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	Tools           []Tool          `json:"tools,omitempty"`
	// ToolChoice is forwarded to the chat-completions backend without
	// translation. See ChatCompletionRequest.ToolChoice for the contract.
	ToolChoice        json.RawMessage            `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool                      `json:"parallel_tool_calls,omitempty"`
	User              string                     `json:"user,omitempty"`
	NexusEval         *NexusEvalContext          `json:"nexus_eval,omitempty"`
	Extra             map[string]json.RawMessage `json:"-"`
}

// ResponsesResponse is the OpenAI Responses API response body.
type ResponsesResponse struct {
	ID        string            `json:"id"`
	Object    string            `json:"object"` // always "response"
	CreatedAt int64             `json:"created_at"`
	Model     string            `json:"model"`
	Status    string            `json:"status"`
	Output    []ResponsesOutput `json:"output"`
	Usage     ResponsesUsage    `json:"usage"`
}

// ResponsesOutput is one element of the Responses API output array. Today we
// only emit "message" items (text content); future additions (reasoning,
// file_citation, etc.) can be added as new types without breaking clients that
// only read Content.
type ResponsesOutput struct {
	Type    string             `json:"type"` // "message"
	ID      string             `json:"id,omitempty"`
	Role    string             `json:"role"` // "assistant"
	Status  string             `json:"status,omitempty"`
	Content []ResponsesContent `json:"content"`
}

// ResponsesContent is one content part inside a Responses output message.
type ResponsesContent struct {
	Type string `json:"type"`           // "output_text"
	Text string `json:"text,omitempty"` // rendered assistant text
}

// ResponsesUsage covers the standard input/output token accounting.
type ResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ModerationRequest is the OpenAI-compatible /v1/moderations request body.
// Input may be a single string or a string array; the union is captured as raw
// JSON to preserve both shapes across the API surface.
//
// See https://platform.openai.com/docs/api-reference/moderations/create
type ModerationRequest struct {
	Model string          `json:"model,omitempty"` // optional; defaults to omni-moderation-latest
	Input json.RawMessage `json:"input"`           // string | []string
	User  string          `json:"user,omitempty"`
}

// ModerationResponse is the OpenAI-compatible /v1/moderations response body.
type ModerationResponse struct {
	ID      string             `json:"id"`
	Model   string             `json:"model"`
	Results []ModerationResult `json:"results"`
}

// ModerationResult is a single per-input moderation outcome.
type ModerationResult struct {
	Flagged    bool            `json:"flagged"`
	Categories ModerationCats  `json:"categories"`
	Scores     ModerationScore `json:"category_scores"`
}

// ModerationCats maps OpenAI category names to booleans. Only the supported
// subset of omni-moderation-latest is listed here; providers that return extra
// categories land in Extra.
type ModerationCats struct {
	Hate            bool `json:"hate"`
	HateThreatening bool `json:"hate/threatening"`
	Harassment      bool `json:"harassment"`
	HarassmentThr   bool `json:"harassment/threatening"`
	SelfHarm        bool `json:"self-harm"`
	SelfHarmIntent  bool `json:"self-harm/intent"`
	SelfHarmInstr   bool `json:"self-harm/instructions"`
	Sexual          bool `json:"sexual"`
	SexualMinors    bool `json:"sexual/minors"`
	Violence        bool `json:"violence"`
	ViolenceGraphic bool `json:"violence/graphic"`
}

// ModerationScore mirrors ModerationCats with confidence scores (0..1).
type ModerationScore struct {
	Hate            float64 `json:"hate"`
	HateThreatening float64 `json:"hate/threatening"`
	Harassment      float64 `json:"harassment"`
	HarassmentThr   float64 `json:"harassment/threatening"`
	SelfHarm        float64 `json:"self-harm"`
	SelfHarmIntent  float64 `json:"self-harm/intent"`
	SelfHarmInstr   float64 `json:"self-harm/instructions"`
	Sexual          float64 `json:"sexual"`
	SexualMinors    float64 `json:"sexual/minors"`
	Violence        float64 `json:"violence"`
	ViolenceGraphic float64 `json:"violence/graphic"`
}

// ImageGenerationRequest is the OpenAI-compatible /v1/images/generations body.
type ImageGenerationRequest struct {
	Model          string `json:"model,omitempty"` // dall-e-2 | dall-e-3 | gpt-image-1
	Prompt         string `json:"prompt"`
	N              *int   `json:"n,omitempty"`
	Quality        string `json:"quality,omitempty"`         // standard | hd (dall-e-3)
	Size           string `json:"size,omitempty"`            // 256x256 | 512x512 | 1024x1024 | 1792x1024 | 1024x1792
	ResponseFormat string `json:"response_format,omitempty"` // url | b64_json
	User           string `json:"user,omitempty"`
	Style          string `json:"style,omitempty"`      // vivid | natural (dall-e-3)
	Background     string `json:"background,omitempty"` // transparent | opaque | auto (gpt-image-1)
}

// ImageGenerationResponse is the OpenAI-compatible /v1/images/generations body.
type ImageGenerationResponse struct {
	Created int64           `json:"created"`
	Data    []ImageDataItem `json:"data"`
}

// ImageDataItem is one image in the generation response.
type ImageDataItem struct {
	B64JSON       string `json:"b64_json,omitempty"`
	URL           string `json:"url,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}
