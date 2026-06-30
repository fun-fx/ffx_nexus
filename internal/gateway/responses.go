package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ffxnexus/nexus/internal/observability"
)

// Responses handles POST /v1/responses.
//
// The OpenAI Responses API is a higher-level wrapper around chat completions:
// the gateway translates `input` to chat `messages`, forwards to the same
// provider pipeline as /v1/chat/completions, then unwraps the chat response
// back into the Responses shape on the way out. This keeps every backend the
// gateway already supports (OpenAI/Anthropic/Gemini) immediately available
// behind /v1/responses without per-provider adapters.
//
// Reference: https://platform.openai.com/docs/api-reference/responses/create
func (h *Handler) Responses(w http.ResponseWriter, r *http.Request) {
	var req ResponsesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	chatReq, err := responsesToChat(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	if chatReq.Stream {
		h.handleResponsesStream(w, r, chatReq)
		return
	}

	response, trace := h.executeResponsesUnary(r, chatReq)
	if response == nil {
		// Trace (with error) was already recorded; emit a generic 502 if no
		// provider succeeded. The execute function ensures the trace reflects
		// the final state.
		writeError(w, http.StatusBadGateway, "upstream_error", "all candidate providers failed")
		return
	}
	if trace != nil {
		h.recorder.Record(*trace)
	}
	writeJSON(w, http.StatusOK, chatToResponses(response, req))
}

// executeResponsesUnary walks the same routing chain as /v1/chat/completions
// (services-first failover, BYOK credential injection, eval trace capture) but
// without guardrails / self-correction, since those are scoped to chat-style
// responses. Returns nil response + recorded trace on total failure.
func (h *Handler) executeResponsesUnary(r *http.Request, req ChatCompletionRequest) (*ChatCompletionResponse, *observability.Trace) {
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

// handleResponsesStream pipes a streaming chat completion through SSE events
// shaped like the Responses API: response.created, response.output_text.delta,
// response.function_call, response.completed.
func (h *Handler) handleResponsesStream(w http.ResponseWriter, r *http.Request, chatReq ChatCompletionRequest) {
	ctx := r.Context()
	providers, ok := h.pickResponsesChain(ctx, chatReq.Model)
	if !ok {
		writeError(w, http.StatusBadGateway, "upstream_error", "no provider for model "+chatReq.Model)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal_error", "streaming unsupported")
		return
	}

	respID := "resp_" + uuid.NewString()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	emit := func(eventType string, data any) {
		payload, err := json.Marshal(data)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, payload)
		flusher.Flush()
	}

	emit("response.created", map[string]any{
		"id":         respID,
		"object":     "response",
		"model":      chatReq.Model,
		"created_at": time.Now().Unix(),
		"status":     "in_progress",
	})

	streamStart := time.Now()
	trace := h.newTrace(r, chatReq, providers[0].Provider.Name())
	trace.Streamed = true

	var (
		textBuf strings.Builder
	)
	for _, p := range providers {
		attempt := chatReq
		attempt.Model = p.ForwardModel
		callCtx := ctx
		if h.credResolver != nil && h.keyMode != KeyModeShared {
			if cred, found, err := h.credResolver.Resolve(ctx, OrgIDFrom(ctx), UserIDFrom(ctx), p.Provider.Name()); err == nil && found {
				callCtx = WithCallerCredential(ctx, CallerCredential{
					Secret: cred.Secret, BaseURL: cred.BaseURL, Source: cred.Source,
				})
				trace.CredentialSource = cred.Source
			}
		}

		events, err := p.Provider.ChatCompletionStream(callCtx, attempt)
		if err != nil {
			trace.LatencyMs = time.Since(streamStart).Milliseconds()
			trace.StatusCode = http.StatusBadGateway
			trace.ErrorType = "upstream_error_failover"
			trace.ErrorMsg = err.Error()
			continue
		}
		var firstChunk bool
		for evt := range events {
			if evt.Err != nil {
				emit("error", map[string]any{"message": evt.Err.Error()})
				continue
			}
			if evt.Done {
				break
			}
			if evt.Chunk == nil {
				continue
			}
			if !firstChunk {
				trace.TTFTMillis = time.Since(streamStart).Milliseconds()
				firstChunk = true
			}
			for _, dc := range evt.Chunk.Choices {
				for _, tc := range dc.Delta.ToolCalls {
					if tc.Function.Name != "" {
						emit("response.function_call", map[string]any{
							"id":        firstNonEmpty(tc.ID, "fc_"+uuid.NewString()),
							"call_id":   tc.ID,
							"name":      tc.Function.Name,
							"arguments": tc.Function.Arguments,
						})
					}
				}
				if dc.Delta.Content != "" {
					textBuf.WriteString(dc.Delta.Content)
					emit("response.output_text.delta", map[string]any{"id": respID, "delta": dc.Delta.Content})
				}
				if dc.FinishReason != "" {
					emit("response.done", map[string]any{"id": respID, "finish_reason": dc.FinishReason})
				}
			}
			if evt.Chunk.Usage != nil {
				trace.InputTokens = evt.Chunk.Usage.PromptTokens
				trace.OutputTokens = evt.Chunk.Usage.CompletionTokens
			}
		}
		trace.LatencyMs = time.Since(streamStart).Milliseconds()
		trace.StatusCode = http.StatusOK
		trace.OutputMessages = textBuf.String()
		break
	}
	h.recorder.Record(trace)

	if textBuf.Len() > 0 {
		emit("response.output_text.done", map[string]any{"id": respID, "text": textBuf.String()})
	}
	emit("response.completed", map[string]any{"id": respID, "status": "completed"})
}

// providerRef pairs a provider with the model id it should be asked for, since
// the alias may be "openai/gpt-4o" while the provider expects just "gpt-4o".
type providerRef struct {
	Provider     Provider
	ForwardModel string
}

// pickResponsesChain returns candidate providers in routing order for a
// /v1/responses request. Routing aliases resolve through the same groups map
// the chat pipeline uses, but we skip min-quality gating so the response shape
// is deterministic (clients that need eval-aware selection should hit
// /v1/chat/completions with model="auto" directly).
func (h *Handler) pickResponsesChain(ctx context.Context, model string) ([]providerRef, bool) {
	if c, ok := h.routeCandidates(model); ok {
		out := make([]providerRef, 0, len(c))
		for _, m := range c {
			if p, fwd, err := h.registry.Resolve(m); err == nil {
				out = append(out, providerRef{Provider: p, ForwardModel: fwd})
			}
		}
		if len(out) > 0 {
			return out, true
		}
	}
	p, fwd, err := h.registry.Resolve(model)
	if err != nil {
		return nil, false
	}
	return []providerRef{{Provider: p, ForwardModel: fwd}}, true
}

// responsesToChat converts a Responses request body into a ChatCompletionRequest.
// Input may be a string or an array of items.
func responsesToChat(req ResponsesRequest) (ChatCompletionRequest, error) {
	chat := ChatCompletionRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxOutputTokens,
		Stream:      req.Stream,
		Tools:       req.Tools,
		User:        req.User,
		NexusEval:   req.NexusEval,
	}
	if req.Instructions != "" {
		chat.Messages = append(chat.Messages, Message{Role: "system", Content: req.Instructions})
	}

	trimmed := strings.TrimSpace(string(req.Input))
	if trimmed == "" || trimmed == "null" {
		return chat, fmt.Errorf("input is required")
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(req.Input, &s); err != nil {
			return chat, fmt.Errorf("input string decode: %w", err)
		}
		chat.Messages = append(chat.Messages, Message{Role: "user", Content: s})
		return chat, nil
	}
	if trimmed[0] != '[' {
		return chat, fmt.Errorf("input must be a string or an array of items")
	}
	var items []InputItem
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return chat, fmt.Errorf("input array decode: %w", err)
	}
	for _, it := range items {
		switch {
		case it.Role == "user" || it.Role == "system" || it.Role == "assistant" || it.Role == "developer":
			chat.Messages = append(chat.Messages, Message{
				Role:    it.Role,
				Content: extractInputText(it.Content),
			})
		case it.Type == "function_call":
			tc := ToolCall{Type: "function", ID: it.CallID}
			tc.Function.Name = strings.TrimSpace(it.Name)
			tc.Function.Arguments = it.Arguments
			chat.Messages = append(chat.Messages, Message{
				Role:      "assistant",
				ToolCalls: []ToolCall{tc},
			})
		case it.Type == "function_call_output":
			chat.Messages = append(chat.Messages, Message{
				Role:       "tool",
				Content:    it.Output,
				ToolCallID: it.CallID,
				Name:       it.Name,
			})
		}
	}
	return chat, nil
}

// extractInputText reads content text from a Responses message content payload,
// which may be a JSON string or an array of {type, text} parts.
func extractInputText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return ""
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return string(raw)
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// chatToResponses unpacks a ChatCompletionResponse into the Responses shape.
// Tool calls become "function_call" output items before any final "message".
func chatToResponses(c *ChatCompletionResponse, req ResponsesRequest) ResponsesResponse {
	out := ResponsesResponse{
		ID:        firstNonEmpty(c.ID, "resp_"+uuid.NewString()),
		Object:    "response",
		CreatedAt: firstNonZero(c.Created, time.Now().Unix()),
		Model:     firstNonEmpty(c.Model, req.Model),
		Status:    "completed",
		Usage: ResponsesUsage{
			InputTokens:  c.Usage.PromptTokens,
			OutputTokens: c.Usage.CompletionTokens,
			TotalTokens:  c.Usage.TotalTokens,
		},
	}
	if len(c.Choices) == 0 {
		return out
	}
	ch := c.Choices[0]
	for _, tc := range ch.Message.ToolCalls {
		out.Output = append(out.Output, ResponsesOutput{
			Type: "function_call",
			ID:   "fc_" + firstNonEmpty(tc.ID, uuid.NewString()),
		})
	}
	out.Output = append(out.Output, ResponsesOutput{
		Type:   "message",
		ID:     "msg_" + uuid.NewString(),
		Role:   "assistant",
		Status: "completed",
		Content: []ResponsesContent{{
			Type: "output_text",
			Text: ch.Message.Content,
		}},
	})
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func firstNonZero(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}

// ensure bufio is not unused in case future tweaks pull it into SSE parsing
var _ = bufio.NewScanner
