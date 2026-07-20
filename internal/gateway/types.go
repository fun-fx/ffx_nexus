package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
)

// parseMessageContent accepts OpenAI Chat Completions content shapes:
//   - string
//   - array of {type,text} parts (Cursor Agent / multimodal clients)
//
// Non-text parts (e.g. image_url) are skipped; text parts are joined with newlines.
func parseMessageContent(raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	if trimmed[0] != '[' {
		return "", fmt.Errorf("content must be a string or an array of parts")
	}
	var parts []struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		InputText string `json:"input_text"` // Responses-style alias
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range parts {
		text := p.Text
		if text == "" {
			text = p.InputText
		}
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
	}
	return b.String(), nil
}

// UnmarshalJSON accepts both string content and multimodal part arrays so
// Cursor Agent and other OpenAI-compatible clients can POST to /v1/chat/completions.
func (m *Message) UnmarshalJSON(data []byte) error {
	type messageFields struct {
		Role       string     `json:"role"`
		Name       string     `json:"name,omitempty"`
		ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
		ToolCallID string     `json:"tool_call_id,omitempty"`
	}
	var raw struct {
		messageFields
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.Name = raw.Name
	m.ToolCalls = raw.ToolCalls
	m.ToolCallID = raw.ToolCallID
	if raw.Content != nil {
		text, err := parseMessageContent(raw.Content)
		if err != nil {
			return err
		}
		m.Content = text
	}
	return nil
}

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

	// ReasoningEffort is the Chat Completions representation of the Responses
	// API reasoning.effort field used by Cursor's hybrid Agent requests.
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	StreamOptions   json.RawMessage `json:"stream_options,omitempty"`

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
	// Extra preserves unknown fields so we can forward provider-specific
	// params (e.g. Responses-only "store", "include", "prompt_cache_key"
	// that the Cursor Agent hybrid path collects from the original body).
	// MarshalJSON splices Extra into the wire JSON so provider adapters
	// that marshal req for the upstream see every originally supplied key.
	Extra map[string]json.RawMessage `json:"-"`
}

// MarshalJSON ensures Extra keys are spliced into the wire payload alongside
// the canonical Chat Completions fields. Adapter code only needs to marshal
// req once; the helper handles the splice.
func (r ChatCompletionRequest) MarshalJSON() ([]byte, error) {
	type alias ChatCompletionRequest
	base, err := json.Marshal(alias(r))
	if err != nil {
		return nil, err
	}
	if len(r.Extra) == 0 {
		return base, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(base, &m); err != nil {
		return base, nil
	}
	for k, v := range r.Extra {
		if _, exists := m[k]; exists {
			continue
		}
		m[k] = v
	}
	return json.Marshal(m)
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

// Tool describes a function or custom tool the model may call.
type Tool struct {
	Type     string          `json:"type"`
	Function json.RawMessage `json:"function,omitempty"`
	Custom   json.RawMessage `json:"custom,omitempty"`
}

// UnmarshalJSON accepts standard Chat Completions nested tools as well as the
// flat Responses definitions that Cursor can send to /chat/completions.
func (t *Tool) UnmarshalJSON(data []byte) error {
	var kind struct {
		Type     string          `json:"type"`
		Function json.RawMessage `json:"function"`
	}
	if err := json.Unmarshal(data, &kind); err != nil {
		return err
	}
	// If the body has either a nested function OR nested custom block, we
	// pass through verbatim so callers that need the original shape (e.g.
	// upstream providers serialising exactly what we received) see no
	// surprises.  Flat Responses-style definitions go through normaliseTool
	// so we always store {type, function} with the original function payload
	// preserved.
	if kind.Type == "custom" {
		var c struct {
			Custom json.RawMessage `json:"custom"`
		}
		if err := json.Unmarshal(data, &c); err == nil && len(c.Custom) > 0 {
			t.Type = "custom"
			t.Custom = c.Custom
			return nil
		}
	}
	if len(kind.Function) > 0 {
		t.Type = kind.Type
		t.Function = kind.Function
		return nil
	}
	normalized, err := normaliseTool(data)
	if err != nil {
		return err
	}
	if normalized == nil {
		return fmt.Errorf("empty tool definition")
	}
	*t = *normalized
	return nil
}

// ToolCall is a model-requested function or custom-tool invocation.
type ToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"-"`
	Custom struct {
		Name  string `json:"name"`
		Input string `json:"input"`
	} `json:"-"`
}

func (t ToolCall) MarshalJSON() ([]byte, error) {
	base := struct {
		Index    *int   `json:"index,omitempty"`
		ID       string `json:"id,omitempty"`
		Type     string `json:"type"`
		Function any    `json:"function,omitempty"`
		Custom   any    `json:"custom,omitempty"`
	}{Index: t.Index, ID: t.ID, Type: t.Type}
	if t.Type == "custom" {
		base.Custom = t.Custom
	} else {
		base.Function = t.Function
	}
	return json.Marshal(base)
}

func (t *ToolCall) UnmarshalJSON(data []byte) error {
	var raw struct {
		Index    *int   `json:"index,omitempty"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
		Custom struct {
			Name  string `json:"name"`
			Input string `json:"input"`
		} `json:"custom"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	t.Index, t.ID, t.Type = raw.Index, raw.ID, raw.Type
	t.Function.Name, t.Function.Arguments = raw.Function.Name, raw.Function.Arguments
	t.Custom.Name, t.Custom.Input = raw.Custom.Name, raw.Custom.Input
	return nil
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

// StreamEvent is emitted on the streaming channel. Exactly one of Chunk, Raw,
// or Err is set per event; Done signals normal end of stream.
//
// Raw carries the upstream SSE event's wire bytes verbatim (data lines +
// trailing blank separator). Providers speaking an OpenAI-compatible wire
// format set Raw so the gateway can forward chunk bytes to the client without
// re-serialising — that round-trip currently drops provider-specific fields
// like `reasoning_content` / `thinking_blocks` and reorders keys, which
// OpenAI-strict clients (Cursor agent mode, LiteLLM pydantic, the
// @ai-sdk/openai-compatible zod schema) refuse to parse. See
// handleStream's Raw branch for the forwarding path; Chunk-based providers
// (Anthropic) keep the historical parse path because their wire format is
// not OpenAI-shaped.
type StreamEvent struct {
	Chunk *ChatCompletionChunk
	Raw   []byte // a complete SSE event (one or more lines + "\n\n")
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
	ID        string                     `json:"id,omitempty"`
	Role      string                     `json:"role,omitempty"` // "user" | "assistant" | "system" | "developer"
	Content   json.RawMessage            `json:"content,omitempty"`
	Name      string                     `json:"name,omitempty"`
	CallID    string                     `json:"call_id,omitempty"`
	ToolUseID string                     `json:"tool_use_id,omitempty"`
	Input     json.RawMessage            `json:"input,omitempty"`
	Arguments string                     `json:"arguments,omitempty"` // for function_call
	Output    json.RawMessage            `json:"output,omitempty"`    // for function_call_output
	Extra     map[string]json.RawMessage `json:"-"`
}

// OutputString returns the tool result output as a string, regardless of
// whether the upstream sent it as a string, an array of blocks, or a JSON
// object.  This keeps the chat-completions tool message deterministic when
// Cursor posts Responses-shaped payloads with structured results.
func (it InputItem) OutputString() string {
	if len(it.Output) == 0 {
		return ""
	}
	trim := strings.TrimSpace(string(it.Output))
	if trim == "" || trim == "null" {
		return ""
	}
	if trim[0] == '"' {
		var s string
		if err := json.Unmarshal(it.Output, &s); err == nil {
			return s
		}
	}
	return strings.TrimSpace(string(it.Output))
}

// ResponsesRequest is the OpenAI Responses API request body. It is intentionally
// a superset of ChatCompletionRequest fields so the gateway can translate it
// into a normal /v1/chat/completions request internally (which every backend
// understands), then unwrap the response back into the Responses shape.
type ResponsesRequest struct {
	Model             string                     `json:"model"`
	Input             json.RawMessage            `json:"input"` // string | []InputItem
	Instructions      string                     `json:"instructions,omitempty"`
	Temperature       *float64                   `json:"temperature,omitempty"`
	TopP              *float64                   `json:"top_p,omitempty"`
	MaxOutputTokens   *int                       `json:"max_output_tokens,omitempty"`
	Stream            bool                       `json:"stream,omitempty"`
	Tools             []Tool                     `json:"tools,omitempty"`
	ToolChoice        json.RawMessage            `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool                      `json:"parallel_tool_calls,omitempty"`
	Reasoning         json.RawMessage            `json:"reasoning,omitempty"`
	Text              json.RawMessage            `json:"text,omitempty"`
	StreamOptions     json.RawMessage            `json:"stream_options,omitempty"`
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
	Type      string             `json:"type"`
	ID        string             `json:"id,omitempty"`
	CallID    string             `json:"call_id,omitempty"`
	Name      string             `json:"name,omitempty"`
	Arguments string             `json:"arguments,omitempty"`
	Input     string             `json:"input,omitempty"`
	Role      string             `json:"role,omitempty"`
	Status    string             `json:"status,omitempty"`
	Content   []ResponsesContent `json:"content,omitempty"`
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
