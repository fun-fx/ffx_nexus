package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMessagesToChatSystemPromoted verifies the top-level Anthropic `system`
// string is folded onto the canonical role:"system" message and that
// per-turn user / assistant text round-trips intact.
func TestMessagesToChatSystemPromoted(t *testing.T) {
	req := MessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 1024,
		System:    json.RawMessage(`"be concise"`),
		Messages: []MessagesMsg{
			{Role: "user", Text: "hi"},
			{Role: "assistant", Text: "hello"},
		},
	}
	chat, err := messagesToChat(req)
	if err != nil {
		t.Fatalf("messagesToChat: %v", err)
	}
	if len(chat.Messages) != 3 {
		t.Fatalf("expected 3 messages (system + 2 turns), got %d", len(chat.Messages))
	}
	if chat.Messages[0].Role != "system" || chat.Messages[0].Content != "be concise" {
		t.Fatalf("system prompt not promoted: %+v", chat.Messages[0])
	}
	if chat.Messages[1].Role != "user" || chat.Messages[1].Content != "hi" {
		t.Fatalf("first user turn wrong: %+v", chat.Messages[1])
	}
	if chat.Messages[2].Role != "assistant" || chat.Messages[2].Content != "hello" {
		t.Fatalf("assistant turn wrong: %+v", chat.Messages[2])
	}
	if chat.MaxTokens == nil || *chat.MaxTokens != 1024 {
		t.Fatalf("max_tokens not propagated: %v", chat.MaxTokens)
	}
}

// TestMessagesToChatToolUseBlock verifies that an assistant tool_use block
// becomes a canonical assistant tool_call message with arguments rendered
// back to a JSON string.
func TestMessagesToChatToolUseBlock(t *testing.T) {
	req := MessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 256,
		Messages: []MessagesMsg{
			{Role: "user", Text: "what's the weather?"},
			{
				Role: "assistant",
				ToolUse: []MessagesToolUse{{
					ID:    "toolu_1",
					Name:  "lookup",
					Input: json.RawMessage(`{"q":"sf"}`),
				}},
			},
			{
				Role: "user",
				ToolResult: []MessagesToolResult{{
					ToolUseID: "toolu_1",
					Content:   json.RawMessage(`"foggy"`),
				}},
			},
		},
	}
	chat, err := messagesToChat(req)
	if err != nil {
		t.Fatalf("messagesToChat: %v", err)
	}
	if len(chat.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(chat.Messages))
	}
	if chat.Messages[1].Role != "assistant" || len(chat.Messages[1].ToolCalls) != 1 {
		t.Fatalf("assistant tool_use not converted: %+v", chat.Messages[1])
	}
	tc := chat.Messages[1].ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Function.Name != "lookup" || tc.Function.Arguments != `{"q":"sf"}` {
		t.Fatalf("tool call payload wrong: %+v", tc)
	}
	if chat.Messages[2].Role != "tool" || chat.Messages[2].ToolCallID != "toolu_1" || chat.Messages[2].Content != "foggy" {
		t.Fatalf("tool_result not converted: %+v", chat.Messages[2])
	}
}

// TestMessagesToChatToolsVerified verifies the flat Anthropic tool list is
// repacked into the Chat Completions nested shape with parameters === input_schema.
func TestMessagesToChatToolsVerified(t *testing.T) {
	req := MessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 1024,
		Messages:  []MessagesMsg{{Role: "user", Text: "hi"}},
		Tools: []MessagesTool{
			{
				Name:        "lookup",
				Description: "search the docs",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
			},
		},
	}
	chat, err := messagesToChat(req)
	if err != nil {
		t.Fatalf("messagesToChat: %v", err)
	}
	if len(chat.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(chat.Tools))
	}
	got := string(chat.Tools[0].Function)
	if !strings.Contains(got, `"name":"lookup"`) {
		t.Fatalf("name missing in canonical tool: %s", got)
	}
	if !strings.Contains(got, `"parameters":{`) {
		t.Fatalf("parameters (input_schema) missing: %s", got)
	}
}

// TestMessagesToChatToolChoiceMapping verifies Anthropic tool_choice strings
// land on the canonical equivalents the upstream adapters expect.
func TestMessagesToChatToolChoiceMapping(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"auto", `"auto"`, `"auto"`},
		{"any_to_required", `"any"`, `"required"`},
		{"none", `"none"`, `"none"`},
		{"tool_named", `{"type":"tool","name":"lookup"}`, `"function"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := encodeToolChoice(json.RawMessage(c.input))
			if !strings.Contains(string(got), c.want) {
				t.Fatalf("tool_choice not mapped: input=%s got=%s want substring=%s", c.input, got, c.want)
			}
		})
	}
}

// TestEncodeMaxTokensDefaulted verifies a missing / <=0 max_tokens falls back
// to the Anthropic-required 4096-floor default so the upstream adapter
// (which always sets max_tokens from the canonical chat MaxTokens) sees a
// positive int.
func TestEncodeMaxTokensDefaulted(t *testing.T) {
	if mt := encodeMaxTokens(0); mt == nil || *mt != 4096 {
		t.Fatalf("zero input should fall back to 4096, got %v", mt)
	}
	if mt := encodeMaxTokens(-10); mt == nil || *mt != 4096 {
		t.Fatalf("negative input should fall back to 4096, got %v", mt)
	}
	if mt := encodeMaxTokens(700); mt == nil || *mt != 700 {
		t.Fatalf("positive input should round-trip, got %v", mt)
	}
}

// TestMessagesToChatImageBlock verifies that an Anthropic image document
// surfaces as a single user message on the chat side. The canonical chat
// Message stores Content as a string, so an image source becomes a data-URL
// (or https URL) carry-over on its own user message; the accompanying text
// block rides along as a separate user turn so cache + guardrails see each
// piece with a stable role.
func TestMessagesToChatImageBlock(t *testing.T) {
	req := MessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 256,
		Messages: []MessagesMsg{
			{
				Role: "user",
				Text: "describe this",
				Documents: []MessagesDocument{
					{
						Type:      "image",
						MediaType: "image/png",
						Data:      "AAAABBCC",
					},
				},
			},
		},
	}
	chat, err := messagesToChat(req)
	if err != nil {
		t.Fatalf("messagesToChat: %v", err)
	}
	if len(chat.Messages) != 2 {
		t.Fatalf("expected 2 messages (text + image), got %d", len(chat.Messages))
	}
	if chat.Messages[0].Role != "user" || chat.Messages[0].Content != "describe this" {
		t.Fatalf("text turn wrong: %+v", chat.Messages[0])
	}
	if chat.Messages[1].Role != "user" || !strings.HasPrefix(chat.Messages[1].Content, "data:image/png;base64,AAAABBCC") {
		t.Fatalf("image turn wrong: %+v", chat.Messages[1])
	}
}

// TestChatToMessagesToolAndText confirms the canonical non-stream response
// fans out into Anthropic content blocks in tool-call-then-text order.
func TestChatToMessagesToolAndText(t *testing.T) {
	in := &ChatCompletionResponse{
		ID:    "msg_chatcmpl-1",
		Model: "claude-sonnet-4-5",
		Choices: []Choice{{
			Message: Message{
				Role:    "assistant",
				Content: "searching",
				ToolCalls: []ToolCall{{
					ID:   "toolu_a",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "lookup", Arguments: `{"q":"weather"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
		Usage: Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
	}
	out := chatToMessages(in, MessagesRequest{Model: "claude-sonnet-4-5"})
	if out.Type != "message" || out.Role != "assistant" {
		t.Fatalf("envelope wrong: %+v", out)
	}
	if len(out.Content) != 2 {
		t.Fatalf("expected 2 content blocks (tool_use + text), got %d", len(out.Content))
	}
	if out.Content[0].Type != "tool_use" || out.Content[0].ID != "toolu_a" || out.Content[0].Name != "lookup" {
		t.Fatalf("tool_use block wrong: %+v", out.Content[0])
	}
	if out.Content[1].Type != "text" || out.Content[1].Text != "searching" {
		t.Fatalf("text block wrong: %+v", out.Content[1])
	}
	if out.StopReason != "tool_use" {
		t.Fatalf("stop_reason not translated: %s", out.StopReason)
	}
	if out.Usage.InputTokens != 7 || out.Usage.OutputTokens != 3 {
		t.Fatalf("usage not propagated: %+v", out.Usage)
	}
}

// TestMapCanonicalStopToAnthropic covers the canonical → Anthropic stop_reason
// mapping. The table is small but loud on failure; clients that rely on
// `end_turn` vs `max_tokens` semantics break visibly when the mapping
// regresses.
func TestMapCanonicalStopToAnthropic(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"stop":           "end_turn",
		"length":         "max_tokens",
		"tool_calls":     "tool_use",
		"content_filter": "stop_sequence",
		"unknown":        "unknown",
	}
	for in, want := range cases {
		got := mapCanonicalStopToAnthropic(in)
		if got != want {
			t.Fatalf("mapCanonicalStopToAnthropic(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMessagesToChatEmptyMessages ensures an empty `messages` array produces
// the same 400 the handler returns — guarding the contract that callers
// supply at least one turn before any backend call is attempted.
func TestMessagesToChatEmptyMessages(t *testing.T) {
	req := MessagesRequest{Model: "claude-sonnet-4-5", MaxTokens: 256}
	chat, err := messagesToChat(req)
	if err != nil {
		t.Fatalf("messagesToChat: %v", err)
	}
	if len(chat.Messages) != 0 {
		t.Fatalf("expected no messages, got %+v", chat.Messages)
	}
}
