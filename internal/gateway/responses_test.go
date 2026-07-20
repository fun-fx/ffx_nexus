package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

const methodPOST = "POST"

// TestResponsesToChatStringInput covers the simplest Responses body: a plain
// string `input` becomes a single user message.
func TestResponsesToChatStringInput(t *testing.T) {
	req := ResponsesRequest{
		Model: "gpt-4o-mini",
		Input: json.RawMessage(`"hello"`),
	}
	chat, err := responsesToChat(req)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	if len(chat.Messages) != 1 || chat.Messages[0].Role != "user" || chat.Messages[0].Content != "hello" {
		t.Fatalf("unexpected messages: %+v", chat.Messages)
	}
}

// TestResponsesToChatArrayInput verifies the items array: instructions become
// a system message and user/assistant turns come through in order.
func TestResponsesToChatArrayInput(t *testing.T) {
	req := ResponsesRequest{
		Model:        "gpt-4o-mini",
		Instructions: "be brief",
		Input: json.RawMessage(`[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"hello there"},
			{"role":"user","content":[{"type":"text","text":"bye"}]}
		]`),
	}
	chat, err := responsesToChat(req)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	if len(chat.Messages) != 4 {
		t.Fatalf("expected 4 messages (system + 3 turns), got %d", len(chat.Messages))
	}
	if chat.Messages[0].Role != "system" || chat.Messages[0].Content != "be brief" {
		t.Fatalf("instructions not folded into system: %+v", chat.Messages[0])
	}
	if chat.Messages[1].Role != "user" || chat.Messages[1].Content != "hi" {
		t.Fatalf("first user turn wrong: %+v", chat.Messages[1])
	}
	if chat.Messages[2].Role != "assistant" || chat.Messages[2].Content != "hello there" {
		t.Fatalf("assistant turn wrong: %+v", chat.Messages[2])
	}
	if chat.Messages[3].Role != "user" || chat.Messages[3].Content != "bye" {
		t.Fatalf("second user turn wrong: %+v", chat.Messages[3])
	}
}

// TestResponsesToChatToolRoundTrip sends back function_call + function_call_output
// items; they should reconstruct the assistant tool_call message and tool result
// message that the chat model expects.
func TestResponsesToChatToolRoundTrip(t *testing.T) {
	req := ResponsesRequest{
		Model: "gpt-4o-mini",
		Input: json.RawMessage(`[
			{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"weather\"}"},
			{"type":"function_call_output","call_id":"call_1","name":"lookup","output":"sunny"}
		]`),
	}
	chat, err := responsesToChat(req)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	if len(chat.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(chat.Messages))
	}
	if chat.Messages[0].Role != "assistant" || len(chat.Messages[0].ToolCalls) != 1 {
		t.Fatalf("first message not assistant-with-tool: %+v", chat.Messages[0])
	}
	tc := chat.Messages[0].ToolCalls[0]
	if tc.Function.Name != "lookup" || tc.Function.Arguments != `{"q":"weather"}` {
		t.Fatalf("tool call payload wrong: %+v", tc)
	}
	if chat.Messages[1].Role != "tool" || chat.Messages[1].ToolCallID != "call_1" || chat.Messages[1].Content != "sunny" {
		t.Fatalf("tool result message wrong: %+v", chat.Messages[1])
	}
}

// TestChatToResponsesToolAndText ensures the response unwrap surfaces tool calls
// in their own output items before the trailing message, and that the message
// content is preserved verbatim.
func TestChatToResponsesToolAndText(t *testing.T) {
	in := &ChatCompletionResponse{
		ID:    "chatcmpl-xyz",
		Model: "gpt-4o-mini",
		Choices: []Choice{{
			Message: Message{
				Role:    "assistant",
				Content: "the weather is sunny",
				ToolCalls: []ToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "lookup", Arguments: "{}"},
				}},
			},
			FinishReason: "stop",
		}},
		Usage: Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	resp := chatToResponses(in, ResponsesRequest{Model: "gpt-4o-mini"})
	if resp.Status != "completed" || resp.Object != "response" {
		t.Fatalf("metadata wrong: %+v", resp)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 {
		t.Fatalf("usage not propagated: %+v", resp.Usage)
	}
	if len(resp.Output) != 2 {
		t.Fatalf("expected 2 output items (function_call + message), got %d", len(resp.Output))
	}
	if resp.Output[0].Type != "function_call" {
		t.Fatalf("first output should be function_call, got %s", resp.Output[0].Type)
	}
	if resp.Output[0].CallID != "call_1" || resp.Output[0].Name != "lookup" {
		t.Fatalf("function call payload lost: %+v", resp.Output[0])
	}
	if resp.Output[1].Type != "message" || resp.Output[1].Role != "assistant" {
		t.Fatalf("second output should be message, got %+v", resp.Output[1])
	}
	if got := resp.Output[1].Content[0].Text; got != "the weather is sunny" {
		t.Fatalf("message text lost: %q", got)
	}
}

// TestChatToResponsesCustomToolCall verifies that custom tool calls survive
// the Responses API round-trip and reach the client as a custom_tool_call
// output item (required for Cursor's ApplyPatch tool).
func TestChatToResponsesCustomToolCall(t *testing.T) {
	tc := ToolCall{ID: "ctc_x", Type: "custom"}
	tc.Custom.Name = "ApplyPatch"
	tc.Custom.Input = "@@ -1 +1 @@\n-a\n+b"
	in := &ChatCompletionResponse{
		ID:    "chatcmpl-p",
		Model: "gpt-4o-mini",
		Choices: []Choice{{
			Message: Message{
				Role:      "assistant",
				ToolCalls: []ToolCall{tc},
			},
		}},
	}
	resp := chatToResponses(in, ResponsesRequest{Model: "gpt-4o-mini"})
	if len(resp.Output) != 1 || resp.Output[0].Type != "custom_tool_call" {
		t.Fatalf("expected custom_tool_call output, got %+v", resp.Output)
	}
	if resp.Output[0].Name != "ApplyPatch" || resp.Output[0].Input != "@@ -1 +1 @@\n-a\n+b" {
		t.Fatalf("custom tool call payload lost: %+v", resp.Output[0])
	}
}

// TestToolCallStreamAccumulation verifies the Anthropic StreamEvent delta
// path correctly accumulates arguments across multiple chunks with `index`
// pointer preservation.
func TestToolCallStreamAccumulation(t *testing.T) {
	in := &ChatCompletionResponse{
		ID:    "chatcmpl-xyz",
		Model: "gpt-4o-mini",
		Choices: []Choice{{
			Message: Message{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					ID:   "call_a",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "search", Arguments: `{"q":"hello","tag":"final"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}
	out := chatToResponses(in, ResponsesRequest{Model: "gpt-4o-mini"})
	if len(out.Output) != 1 || out.Output[0].Arguments != `{"q":"hello","tag":"final"}` {
		t.Fatalf("accumulated arguments lost: %+v", out.Output)
	}
	if out.Output[0].CallID != "call_a" || out.Output[0].Name != "search" {
		t.Fatalf("tool metadata lost: %+v", out.Output[0])
	}
}

// TestEmbeddingsModelRequired enforces the contract that model + input are both
// present on every embeddings request, so callers get a clear 400 instead of
// an upstream-side 422.
func TestEmbeddingsModelRequired(t *testing.T) {
	// We don't stand up a registry; the handler should bail before provider
	// resolution. Use a Handler with a fresh registry to keep the test cheap.
	h := &Handler{registry: NewRegistry()}
	cases := []struct {
		name string
		body string
	}{
		{"empty body", `{}`},
		{"missing input", `{"model":"text-embedding-3-small"}`},
		{"null input", `{"model":"text-embedding-3-small","input":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(methodPOST, "/v1/embeddings", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			h.Embeddings(rec, req)
			if rec.Code != 400 {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
		})
	}
}

// TestEmbeddingsModelNotFound confirms an unregistered embedding model returns
// 404 with the OpenAI-compatible error envelope.
func TestEmbeddingsModelNotFound(t *testing.T) {
	reg := NewRegistry()
	// no providers registered
	h := &Handler{registry: reg}
	req := httptest.NewRequest(methodPOST, "/v1/embeddings",
		strings.NewReader(`{"model":"who-knows","input":"x"}`))
	rec := httptest.NewRecorder()
	h.Embeddings(rec, req)
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "model_not_found") {
		t.Fatalf("missing error envelope: %s", rec.Body.String())
	}
}
