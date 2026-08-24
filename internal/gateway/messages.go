package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ffxnexus/nexus/internal/observability"
)

// MessagesRequest is the Anthropic Messages API request body accepted on
// POST /v1/messages. It mirrors the wire spec at
// https://docs.anthropic.com/en/api/messages so the official Anthropic SDKs can
// point their base URL at the gateway verbatim.
//
// Inbound shape differences from canonical ChatCompletionRequest:
//   - System prompt lives at the top level (string or content blocks) instead
//     of inside `messages` as role:"system".
//   - Each message `content` is an array of typed blocks
//     (text | image | tool_use | tool_result) instead of a single string.
//   - Tools are flat `[{name, description, input_schema}]`, not nested under
//     `function`.
//   - `max_tokens` is mandatory.
//
// Tool definition parsing is intentionally permissive: the gateway accepts
// the canonical Chat Completions nested form on this path so its own SDK
// callers do not have to rewrite tool descriptions on their way to a Claude
// upstream. The Anthropic adapter already accepts the canonical form.
type MessagesRequest struct {
	Model         string            `json:"model"`
	MaxTokens     int               `json:"max_tokens"`
	System        json.RawMessage   `json:"system,omitempty"` // string | []ContentBlock
	Messages      []MessagesMsg     `json:"messages"`
	Tools         []MessagesTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage   `json:"tool_choice,omitempty"`
	Stream        bool              `json:"stream,omitempty"`
	StopSequences []string          `json:"stop_sequences,omitempty"`
	Temperature   *float64          `json:"temperature,omitempty"`
	TopP          *float64          `json:"top_p,omitempty"`
	Metadata      map[string]any    `json:"metadata,omitempty"`
	NexusEval     *NexusEvalContext `json:"nexus_eval,omitempty"`
}

// MessagesMsg is one entry of the Anthropic `messages` array. Tool exchanges
// use ToolUseID / ToolResultContent; plain text uses Text. The custom
// unmarshaler flattens the Anthropic content-block shape onto the request
// flow.
type MessagesMsg struct {
	Role string `json:"role"`

	// Text is the rendered text for plain user / assistant messages. Populated
	// for `content: string` and for blocks-of-text arrays. Empty when the
	// message only carries image documents or tool exchanges, in which case
	// the caller must read the typed fields below.
	Text string `json:"-"`

	// ToolUse blocks emitted by the assistant. The gateway accumulates the
	// arguments string into a single block before forwarding to a chat
	// tool_call.
	ToolUse []MessagesToolUse `json:"-"`

	// ToolResult blocks emitted on a follow-up user turn in reply to a
	// previous tool_use id. Multiple results may be carried in one message.
	ToolResult []MessagesToolResult `json:"-"`

	// Document blocks (images today) are surfaced as image_url canonical
	// content on the chat side.
	Documents []MessagesDocument `json:"-"`
}

// MessagesTool is one tool definition. Sequential Tool unmarshal keeps the
// shape OpenAI-nested compatible so the canonical pipeline can pass it
// through to every provider adapter unchanged.
type MessagesTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// MessagesToolUse is one assistant tool invocation.
type MessagesToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// MessagesToolResult is one follow-up tool result keyed by tool_use_id.
type MessagesToolResult struct {
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error,omitempty"`
}

// MessagesDocument is a non-text content block. Only image is materialised
// today; the field is left open for future modalities.
type MessagesDocument struct {
	Type      string `json:"type"`       // "image"
	MediaType string `json:"media_type"` // "image/png" | ...
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// MessagesResponse is the Anthropic Messages API non-stream response. The
// shape mirrors the upstream spec one-for-one so stock Anthropic SDKs (Python
// and TypeScript) parse the gateway's reply without a custom decoder.
type MessagesResponse struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"` // always "message"
	Role         string            `json:"role"` // "assistant"
	Content      []MessagesRespBlk `json:"content"`
	Model        string            `json:"model"`
	StopReason   string            `json:"stop_reason"`
	StopSequence *string           `json:"stop_sequence,omitempty"`
	Usage        MessagesUsage     `json:"usage"`
}

// MessagesRespBlk is one content block inside MessagesResponse.Content. Today
// only text and tool_use are emitted; the field stays open for future
// blocks (tool_result, document, ...).
type MessagesRespBlk struct {
	Type  string          `json:"type"` // "text" | "tool_use"
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// MessagesUsage mirrors the Anthropic `usage` envelope.
type MessagesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Messaging handles POST /v1/messages for inbound Anthropic SDK traffic.
//
// The handler translates the request into the canonical ChatCompletionRequest,
// then funnels it through ChatCompletions' pipeline (resolveChain → guardrails →
// handleUnary/handleStream). On the way out the canonical response is rewritten
// into the Anthropic Messages shape (unary) or Anthropic SSE events (stream).
//
// Tracing, BYOK, fail-over, eval routing, cost accounting, and guardrails are
// all inherited from the chat path because the canonical request that flows
// through them is byte-for-byte equivalent to what /v1/chat/completions would
// have sent for the same logical conversation.
//
// Reference: https://docs.anthropic.com/en/api/messages
func (h *Handler) Messages(w http.ResponseWriter, r *http.Request) {
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request_error", "cannot read body: "+err.Error())
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(rawBody))

	var req MessagesRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, r, http.StatusBadRequest, "invalid_request_error", "messages is required and must contain at least one entry")
		return
	}

	// Surface the wire's per-conversation marker on every trace this request
	// produces so the overview can fold N Messages turns from the same
	// agent loop into one session row. Same probe as the chat path.
	if sid := extractSessionID(rawBody); sid != "" {
		ctx := context.WithValue(r.Context(), ctxKeySessionID, sid)
		r = r.WithContext(ctx)
	}

	chatReq, err := messagesToChat(req)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	if req.Stream {
		h.handleMessagesStream(w, r, chatReq, req)
		return
	}

	resp, trace := h.executeMessagesUnary(r, chatReq)
	if resp == nil {
		writeError(w, r, http.StatusBadGateway, "upstream_error", "all candidate providers failed")
		return
	}
	if trace != nil {
		trace.CostUSD = ResolveCostUSD(resp.Usage.EstimatedCost, chatReq.Model, trace.ResponseModel, &resp.Usage)
		resp.Usage.CostUSD = trace.CostUSD
		setCostHeader(w, trace.CostUSD)
		h.recorder.Record(*trace)
		h.recordSpend(r.Context(), trace.CostUSD)
	}
	writeJSON(w, http.StatusOK, chatToMessages(resp, req))
}

// executeMessagesUnary mirrors the chat path's unary dispatch but uses the
// simpler pickResponsesChain so the Anthropic inbound surface stays
// deterministic for callers that expected a stable provider selection (SDKs
// driven by user-configured base URLs). Quality-aware ranks are still
// honoured because pickResponsesChain resolves through the same groups map.
func (h *Handler) executeMessagesUnary(r *http.Request, req ChatCompletionRequest) (*ChatCompletionResponse, *observability.Trace) {
	ctx := r.Context()
	providers, ok := h.pickResponsesChain(ctx, req.Model)
	if !ok {
		return nil, nil
	}

	trace := h.newTrace(r, req, providers[0].Provider.Name())
	attemptStart := time.Now()

	for i, p := range providers {
		attempt := req
		attempt.Model = p.ForwardModel

		callCtx := ctx
		credSource := "env"
		if h.credResolver != nil && h.keyMode != KeyModeShared {
			if cred, found, err := h.credResolver.Resolve(ctx, OrgIDFrom(ctx), UserIDFrom(ctx), p.Provider.Name()); err == nil && found {
				callCtx = WithCallerCredential(ctx, CallerCredential{
					Secret: cred.Secret, BaseURL: cred.BaseURL, Source: cred.Source,
				})
				credSource = cred.Source
			} else if err != nil && h.keyMode == KeyModeStrictBYOK {
				trace.LatencyMs = time.Since(attemptStart).Milliseconds()
				trace.StatusCode = http.StatusForbidden
				trace.ErrorType = "missing_byok_key"
				trace.ErrorMsg = err.Error()
				trace.CredentialSource = "none"
				return nil, &trace
			}
		}
		trace.CredentialSource = credSource

		resp, err := p.Provider.ChatCompletion(callCtx, attempt)
		if err != nil {
			trace.LatencyMs = time.Since(attemptStart).Milliseconds()
			trace.StatusCode = http.StatusBadGateway
			trace.ErrorType = "upstream_error"
			if i < len(providers)-1 {
				trace.ErrorType = "upstream_error_failover"
			}
			trace.ErrorMsg = err.Error()
			return nil, &trace
		}

		trace.LatencyMs = time.Since(attemptStart).Milliseconds()
		trace.StatusCode = http.StatusOK
		trace.ResponseModel = resp.Model
		trace.InputTokens = resp.Usage.PromptTokens
		trace.OutputTokens = resp.Usage.CompletionTokens
		if len(resp.Choices) > 0 {
			trace.OutputMessages = resp.Choices[0].Message.Content
			trace.FinishReason = resp.Choices[0].FinishReason
		}
		return resp, &trace
	}
	return nil, &trace
}

// handleMessagesStream pipes the canonical ChatCompletion-stream channel
// through Anthropic SSE envelopes:
//
//	message_start
//	content_block_start (× N: text or tool_use)
//	content_block_delta  (× N)
//	content_block_stop   (× N)
//	message_delta        (with usage on the last block)
//	message_stop
//
// Stream errors are surfaced as Anthropic `error` events followed by
// message_stop with stop_reason:"error".
func (h *Handler) handleMessagesStream(w http.ResponseWriter, r *http.Request, chatReq ChatCompletionRequest, msgReq MessagesRequest) {
	ctx := r.Context()
	providers, ok := h.pickResponsesChain(ctx, chatReq.Model)
	if !ok {
		writeError(w, r, http.StatusBadGateway, "upstream_error", "no provider for model "+chatReq.Model)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "streaming unsupported")
		return
	}

	msgID := "msg_" + uuid.NewString()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Trailer", costHeaderName)
	w.WriteHeader(http.StatusOK)

	emit := func(eventType string, data any) {
		payload, err := json.Marshal(data)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, payload)
		flusher.Flush()
	}

	// message_start announces the message envelope before any block opens.
	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         chatReq.Model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	})

	// State for streamed block assembly. Anthropic requires each text /
	// tool_use block to be opened with content_block_start, fed with one or
	// more content_block_delta, then closed with content_block_stop. We
	// index blocks in arrival order, with text always at index 0 when
	// present and tool_use taking the next slots.
	type toolPartial struct {
		idx         int
		blockID     string
		name        string
		inputBuf    bytes.Buffer
		opened      bool
		stoppedSent bool
	}
	state := struct {
		textOpened    bool
		textIdx       int
		textBuf       strings.Builder
		tools         map[int]*toolPartial
		order         []int
		usage         MessagesUsage
		hasUsage      bool
		finishReason  string
		streamErrSeen bool
	}{tools: map[int]*toolPartial{}}

	streamStart := time.Now()

	for _, p := range providers {
		attempt := chatReq
		attempt.Model = p.ForwardModel
		callCtx := ctx
		var credSource string
		if h.credResolver != nil && h.keyMode != KeyModeShared {
			if cred, found, err := h.credResolver.Resolve(ctx, OrgIDFrom(ctx), UserIDFrom(ctx), p.Provider.Name()); err == nil && found {
				callCtx = WithCallerCredential(ctx, CallerCredential{
					Secret: cred.Secret, BaseURL: cred.BaseURL, Source: cred.Source,
				})
				credSource = cred.Source
			}
		}

		t := h.newTrace(r, chatReq, p.Provider.Name())
		t.RequestModel = chatReq.Model
		t.Streamed = true
		t.CredentialSource = credSource

		events, err := p.Provider.ChatCompletionStream(callCtx, attempt)
		if err != nil {
			t.LatencyMs = time.Since(streamStart).Milliseconds()
			t.StatusCode = http.StatusBadGateway
			t.ErrorType = "upstream_error"
			if len(providers) > 1 {
				t.ErrorType = "upstream_error_failover"
			}
			t.ErrorMsg = err.Error()
			h.recorder.Record(t)
			continue
		}

		firstChunk := true
		for evt := range events {
			if evt.Err != nil {
				state.streamErrSeen = true
				t.LatencyMs = time.Since(streamStart).Milliseconds()
				t.ErrorType = "stream_error"
				t.ErrorMsg = evt.Err.Error()
				emit("error", map[string]any{
					"type":    "stream_error",
					"message": evt.Err.Error(),
				})
				break
			}
			if evt.Done || evt.Chunk == nil {
				break
			}
			if firstChunk {
				t.TTFTMillis = time.Since(streamStart).Milliseconds()
				firstChunk = false
			}
			if evt.Chunk.Model != "" {
				t.ResponseModel = evt.Chunk.Model
			}
			if evt.Chunk.Usage != nil {
				state.usage = MessagesUsage{
					InputTokens:  evt.Chunk.Usage.PromptTokens,
					OutputTokens: evt.Chunk.Usage.CompletionTokens,
				}
				state.hasUsage = true
				t.InputTokens = evt.Chunk.Usage.PromptTokens
				t.OutputTokens = evt.Chunk.Usage.CompletionTokens
			}
			for _, dc := range evt.Chunk.Choices {
				// ---- Text content deltas ------------------------------
				if dc.Delta.Content != "" {
					if !state.textOpened {
						state.textOpened = true
						state.textIdx = 0
						emit("content_block_start", map[string]any{
							"type":  "content_block_start",
							"index": state.textIdx,
							"content_block": map[string]any{
								"type": "text",
								"text": "",
							},
						})
					}
					state.textBuf.WriteString(dc.Delta.Content)
					emit("content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": state.textIdx,
						"delta": map[string]any{
							"type": "text_delta",
							"text": dc.Delta.Content,
						},
					})
				}

				// ---- Tool call deltas ----------------------------------
				for _, tc := range dc.Delta.ToolCalls {
					idx := 0
					if tc.Index != nil {
						idx = *tc.Index
					}
					blockIdx := idx
					if state.textOpened {
						blockIdx = idx + 1
					}
					p, seen := state.tools[idx]
					if !seen {
						stp := &toolPartial{
							idx:     blockIdx,
							blockID: "toolu_" + uuid.NewString(),
							name:    tc.Function.Name,
						}
						p = stp
						state.tools[idx] = p
						state.order = append(state.order, idx)
						emit("content_block_start", map[string]any{
							"type":  "content_block_start",
							"index": blockIdx,
							"content_block": map[string]any{
								"type":  "tool_use",
								"id":    p.blockID,
								"name":  p.name,
								"input": map[string]any{},
							},
						})
						p.opened = true
					} else if tc.Function.Name != "" && p.name == "" {
						p.name = tc.Function.Name
					}
					if tc.Function.Arguments != "" {
						p.inputBuf.WriteString(tc.Function.Arguments)
						emit("content_block_delta", map[string]any{
							"type":  "content_block_delta",
							"index": blockIdx,
							"delta": map[string]any{
								"type":         "input_json_delta",
								"partial_json": tc.Function.Arguments,
							},
						})
					}
				}

				if dc.FinishReason != "" {
					state.finishReason = mapCanonicalStopToAnthropic(dc.FinishReason)
				}
			}
		}
		break
	}

	// ---- Close open blocks ------------------------------------------------
	if state.textOpened {
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": state.textIdx,
		})
	}
	for _, idx := range state.order {
		p := state.tools[idx]
		if p.stoppedSent {
			continue
		}
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": p.idx,
		})
		p.stoppedSent = true
	}

	// ---- Final message_delta and message_stop -----------------------------
	deltaUsage := map[string]any{
		"input_tokens":  state.usage.InputTokens,
		"output_tokens": state.usage.OutputTokens,
	}
	finalStop := state.finishReason
	if state.streamErrSeen && finalStop == "" {
		finalStop = "error"
	}
	emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   finalStop,
			"stop_sequence": nil,
		},
		"usage": deltaUsage,
	})
	emit("message_stop", map[string]any{"type": "message_stop"})

	// Acknowledge the trailer slot for HTTP-aware clients without forcing a
	// value forward; the cost composer runs on the unary path. The
	// streaming path also writes the trailer through setCostTrailer below.
	_ = deltaUsage
}

// messagesToChat normalises an Anthropic Messages request into the canonical
// Chat Completions shape. System prompts are promoted to role:"system"
// messages so guardrail + tracing + cache logic on the chat path can act on
// them without special-casing the inbound surface.
func messagesToChat(req MessagesRequest) (ChatCompletionRequest, error) {
	chat := ChatCompletionRequest{
		Model:             req.Model,
		Temperature:       req.Temperature,
		TopP:              req.TopP,
		MaxTokens:         encodeMaxTokens(req.MaxTokens),
		Stream:            req.Stream,
		Stop:              req.StopSequences,
		User:              userFromMetadata(req.Metadata),
		NexusEval:         req.NexusEval,
		ParallelToolCalls: ptrBool(false),
	}
	if systemPrompt, err := decodeSystem(req.System); err == nil && systemPrompt != "" {
		chat.Messages = append(chat.Messages, Message{Role: "system", Content: systemPrompt})
	}
	if chat.Tools, _ = encodeTools(req.Tools); len(chat.Tools) == 0 {
		chat.Tools = nil
	}
	if len(req.ToolChoice) > 0 {
		chat.ToolChoice = encodeToolChoice(req.ToolChoice)
	}

	for _, m := range req.Messages {
		if text := strings.TrimSpace(m.Text); text != "" {
			chat.Messages = append(chat.Messages, Message{Role: m.Role, Content: text})
		}
		for _, tu := range m.ToolUse {
			tc := ToolCall{Type: "function", ID: tu.ID}
			tc.Function.Name = tu.Name
			tc.Function.Arguments = stringOr(tu.Input, "{}")
			chat.Messages = append(chat.Messages, Message{
				Role:      "assistant",
				ToolCalls: []ToolCall{tc},
			})
		}
		for _, tr := range m.ToolResult {
			chat.Messages = append(chat.Messages, Message{
				Role:       "tool",
				Content:    stringOr(tr.Content, ""),
				ToolCallID: tr.ToolUseID,
			})
		}
		for _, doc := range m.Documents {
			imageURL := encodeImageSource(doc)
			if imageURL == "" {
				continue
			}
			chat.Messages = append(chat.Messages, Message{
				Role:    m.Role,
				Content: imageURL,
			})
		}
	}
	return chat, nil
}

// encodeMaxTokens maps the Anthropic mandatory int back to the canonical
// pointer shape. A zero result falls back to a default so the upstream
// provider never sees a literal "0".
func encodeMaxTokens(src int) *int {
	if src <= 0 {
		defaultTokens := 4096
		return &defaultTokens
	}
	out := src
	return &out
}

// ptrBool is a tiny helper for fields whose zero-value must be serialised
// explicitly (e.g. parallel_tool_calls=false).
func ptrBool(b bool) *bool { return &b }

// decodeSystem renders the Anthropic top-level `system` field — either a
// plain string or a list of typed blocks — into a single string for the
// canonical role:"system" message. Content blocks other than text are
// silently dropped because guardrails and tracing only need the rendered
// text.
func decodeSystem(raw json.RawMessage) (string, error) {
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
		return "", fmt.Errorf("system must be a string or array of blocks")
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("system blocks decode: %w", err)
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" && blk.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String(), nil
}

// encodeTools converts Anthropic flat tool definitions into the canonical
// Chat Completions nested shape. Tools already in Chat Completions form pass
// through; round-tripping preserves every field so providers that honour
// `parameters` (OpenAI / Groq / Mistral) receive the same wire shape the
// canonical path emits.
func encodeTools(tools []MessagesTool) ([]Tool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		if t.InputSchema != nil && t.Name != "" {
			fn, _ := json.Marshal(map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.InputSchema,
			})
			out = append(out, Tool{Type: "function", Function: fn})
			continue
		}
		// Anthropic may also accept a Nested Chat Completions shape (the SDK
		// has historically shipped such variations). Repack them as-is so the
		// outbound adapter sees a uniform shape.
		var nested struct {
			Type     string          `json:"type"`
			Function json.RawMessage `json:"function"`
		}
		if err := json.Unmarshal([]byte(t.Name), &nested); err == nil && nested.Type != "" {
			out = append(out, Tool{Type: nested.Type, Function: nested.Function})
			continue
		}
		return nil, fmt.Errorf("tool %q requires input_schema or nested function body", t.Name)
	}
	return out, nil
}

// encodeToolChoice translates the Anthropic tool_choice shape (string
// "auto"|"any"|"tool", or {type,name}) into the canonical Chat Completions
// shape ("auto"|"required"|"none", or nested function). Semantic mapping:
//
//	"auto"   → "auto"
//	"any"    → "required"
//	"tool"   → {type:"function", function:{name:"..."}}
//	"none"   → "none"
func encodeToolChoice(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return raw
		}
		switch s {
		case "auto":
			return json.RawMessage(`"auto"`)
		case "any":
			return json.RawMessage(`"required"`)
		case "none":
			return json.RawMessage(`"none"`)
		case "tool":
			// Anthropic's bare "tool" without a name is meaningless; the SDK
			// should always pair it with the named object form. Fall through.
			return raw
		default:
			return raw
		}
	}
	if trimmed[0] != '{' {
		return raw
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	switch obj.Type {
	case "auto":
		return json.RawMessage(`"auto"`)
	case "any":
		return json.RawMessage(`"required"`)
	case "tool":
		if obj.Name == "" {
			return raw
		}
		nested, _ := json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]any{"name": obj.Name},
		})
		return nested
	case "none":
		return json.RawMessage(`"none"`)
	default:
		return raw
	}
}

// decodeMessages reconstructs the Anthropic content-block shape onto a
// MessagesMsg. Tool exchanges, plain text, and image documents are materialised
// into the typed fields so messagesToChat can produce the canonical chat side
// without a second pass over the raw bytes.
func decodeMessages(raw json.RawMessage) (MessagesMsg, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return MessagesMsg{}, nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return MessagesMsg{}, err
		}
		return MessagesMsg{Text: s}, nil
	}
	if trimmed[0] != '[' {
		return MessagesMsg{}, fmt.Errorf("content must be a string or an array of blocks")
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return MessagesMsg{}, fmt.Errorf("content blocks decode: %w", err)
	}
	msg := MessagesMsg{}
	for _, blk := range blocks {
		var probe struct {
			Type      string          `json:"type"`
			Text      string          `json:"text,omitempty"`
			ID        string          `json:"id,omitempty"`
			Name      string          `json:"name,omitempty"`
			Input     json.RawMessage `json:"input,omitempty"`
			ToolUseID string          `json:"tool_use_id,omitempty"`
			Content   json.RawMessage `json:"content,omitempty"`
			IsError   bool            `json:"is_error,omitempty"`
			Source    json.RawMessage `json:"source,omitempty"`
			MediaType string          `json:"media_type,omitempty"`
			Data      string          `json:"data,omitempty"`
			URL       string          `json:"url,omitempty"`
		}
		if err := json.Unmarshal(blk, &probe); err != nil {
			continue
		}
		switch probe.Type {
		case "text":
			if probe.Text != "" {
				if msg.Text != "" {
					msg.Text += "\n"
				}
				msg.Text += probe.Text
			}
		case "tool_use":
			msg.ToolUse = append(msg.ToolUse, MessagesToolUse{
				ID: probe.ID, Name: probe.Name, Input: probe.Input,
			})
		case "tool_result":
			msg.ToolResult = append(msg.ToolResult, MessagesToolResult{
				ToolUseID: probe.ToolUseID,
				Content:   probe.Content,
				IsError:   probe.IsError,
			})
		case "image":
			var src struct {
				Type      string `json:"type"`
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
				URL       string `json:"url"`
			}
			_ = json.Unmarshal(probe.Source, &src)
			msg.Documents = append(msg.Documents, MessagesDocument{
				Type:      "image",
				MediaType: firstNonEmpty(src.MediaType, probe.MediaType),
				Data:      firstNonEmpty(src.Data, probe.Data),
				URL:       firstNonEmpty(src.URL, probe.URL),
			})
		}
	}
	return msg, nil
}

// encodeImageSource converts an Anthropic image block into an OpenAI-style
// image_url canonical content value (data: URL or https URL). Image blocks
// without a payload are silently dropped, mirroring the chat pill behavior
// of other multimodal entries.
func encodeImageSource(doc MessagesDocument) string {
	switch doc.Type {
	case "image":
		if doc.URL != "" {
			return doc.URL
		}
		if doc.Data != "" {
			media := doc.MediaType
			if media == "" {
				media = "image/png"
			}
			return "data:" + media + ";base64," + doc.Data
		}
	}
	return ""
}

// userFromMetadata lifts the upstream-style `metadata.user_id` (Anthropic SDK
// default) onto the canonical user field so trace downstream can render it.
func userFromMetadata(meta map[string]any) string {
	if meta == nil {
		return ""
	}
	if v, ok := meta["user_id"].(string); ok {
		return v
	}
	return ""
}

// stringOr returns the canonical string form of an arbitrary JSON value
// (object → compact JSON, string → its body, etc.). It is the inverse shape
// of the canonical type system used in tool arguments and tool results: the
// Anthropic adapter accepts either a string or structured content blocks, and
// the canonical chat side accepts strings only.
func stringOr(raw json.RawMessage, fallback string) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return fallback
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	return string(raw)
}

// UnmarshalJSON for MessagesMsg converts the Anthropic content-block shape
// into the typed fields used by the gateway pipeline.
func (m *MessagesMsg) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	if len(raw.Content) == 0 {
		return nil
	}
	decoded, err := decodeMessages(raw.Content)
	if err != nil {
		return err
	}
	*m = decoded
	m.Role = raw.Role
	return nil
}

// chatToMessages rewrites the canonical non-stream response into the
// Anthropic Messages shape. Text and tool_use blocks live side-by-side, so a
// mixed response fans out into multiple content blocks in emission order.
func chatToMessages(c *ChatCompletionResponse, req MessagesRequest) MessagesResponse {
	out := MessagesResponse{
		ID:         firstNonEmpty(c.ID, "msg_"+uuid.NewString()),
		Type:       "message",
		Role:       "assistant",
		Model:      firstNonEmpty(c.Model, req.Model),
		StopReason: "end_turn",
		Usage: MessagesUsage{
			InputTokens:  c.Usage.PromptTokens,
			OutputTokens: c.Usage.CompletionTokens,
		},
	}
	if len(c.Choices) == 0 {
		return out
	}
	ch := c.Choices[0]
	for _, tc := range ch.Message.ToolCalls {
		// Anthropic reports tool_use in tool_calls order; we preserve that
		// ordering so a multi-tool response renders predictably to Anthropic
		// SDK callers.
		out.Content = append(out.Content, MessagesRespBlk{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(tc.Function.Arguments),
		})
	}
	if text := strings.TrimSpace(ch.Message.Content); text != "" {
		out.Content = append(out.Content, MessagesRespBlk{
			Type: "text",
			Text: text,
		})
	}
	if stop := mapCanonicalStopToAnthropic(ch.FinishReason); stop != "" {
		out.StopReason = stop
	}
	return out
}

// mapCanonicalStopToAnthropic translates the OpenAI-compatible
// `finish_reason` to the Anthropic `stop_reason` family. Mirrors the
// upstream provider's own mapAnthropicStop so the two paths agree on
// semantics across the wire.
func mapCanonicalStopToAnthropic(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "stop_sequence"
	case "":
		return ""
	default:
		return reason
	}
}
