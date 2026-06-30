package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/gateway"
)

func TestAnthropicToolsForwardedAndToolUseUnwrapped(t *testing.T) {
	var gotReq anthropicRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "msg_test",
			"model": "claude-sonnet-4-5",
			"content": []map[string]any{
				{"type": "text", "text": "Let me look that up."},
				{
					"type": "tool_use",
					"id":   "toolu_abc",
					"name": "get_weather",
					"input": map[string]any{
						"city": "Seoul",
					},
				},
			},
			"stop_reason": "tool_use",
			"usage":       map[string]any{"input_tokens": 12, "output_tokens": 9},
		})
	}))
	defer srv.Close()

	a := &Anthropic{
		apiKey:  "sk-ant-test",
		baseURL: srv.URL,
		client:  &http.Client{Timeout: 5 * time.Second},
		models:  []string{"claude-sonnet-4-5"},
	}

	resp, err := a.ChatCompletion(context.Background(), gateway.ChatCompletionRequest{
		Model: "claude-sonnet-4-5",
		Messages: []gateway.Message{
			{Role: "user", Content: "What is the weather in Seoul?"},
			{
				Role: "assistant", Content: "I'll check.",
				ToolCalls: []gateway.ToolCall{{
					Type: "function",
					ID:   "toolu_prev",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "get_weather", Arguments: `{"city":"Berlin"}`},
				}},
			},
			{Role: "tool", ToolCallID: "toolu_prev", Content: `{"temp":7}`},
		},
		ToolChoice: json.RawMessage(`"required"`),
		Tools: []gateway.Tool{
			{
				Type: "function",
				Function: json.RawMessage(`{
					"name":"get_weather",
					"description":"Look up weather",
					"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}
				}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	// Upstream contract
	if gotReq.Model != "claude-sonnet-4-5" {
		t.Fatalf("model lost: %q", gotReq.Model)
	}
	if gotReq.ToolChoice == nil || gotReq.ToolChoice.Type != "any" {
		t.Fatalf("tool_choice 'required' should map to Anthropic 'any'; got %+v", gotReq.ToolChoice)
	}
	if len(gotReq.Tools) != 1 || gotReq.Tools[0].Name != "get_weather" {
		t.Fatalf("tools not forwarded; got %+v", gotReq.Tools)
	}
	if !strings.Contains(string(gotReq.Tools[0].InputSchema), `"city"`) {
		t.Fatalf("input_schema does not contain city; got %s", gotReq.Tools[0].InputSchema)
	}
	// Message shape: tool result mapped to tool_result block with id.
	if last := gotReq.Messages[len(gotReq.Messages)-1]; last.Role != "user" {
		t.Fatalf("tool result should reuse user role in Anthropic; got %q", last.Role)
	} else if !strings.Contains(string(last.Content), "toolu_prev") || !strings.Contains(string(last.Content), "tool_result") {
		t.Fatalf("tool_result block missing or wrong id; got %s", last.Content)
	}
	// Assistant's tool call mapped to tool_use block.
	if second := gotReq.Messages[len(gotReq.Messages)-2]; second.Role != "assistant" {
		t.Fatalf("previous assistant should still be 'assistant'; got %q", second.Role)
	} else if !strings.Contains(string(second.Content), "toolu_prev") || !strings.Contains(string(second.Content), "tool_use") {
		t.Fatalf("assistant tool_use block missing; got %s", second.Content)
	}

	// Response contract: tool_use blocks become tool_calls on the choice.
	if len(resp.Choices) != 1 || len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool_use should land on Choice.Message.ToolCalls; got %+v", resp.Choices)
	}
	tc := resp.Choices[0].Message.ToolCalls[0]
	if tc.ID != "toolu_abc" || tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"Seoul"}` {
		t.Fatalf("tool call unwrap mismatch: %+v", tc)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("anthropic tool_use stop reason should map to 'tool_calls'; got %q", resp.Choices[0].FinishReason)
	}
}

func TestAnthropicToolChoiceObjectShapePinsName(t *testing.T) {
	var gotReq anthropicRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_test", "model": "claude-haiku-4-5",
			"content":     []map[string]any{{"type": "text", "text": "ok"}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	defer srv.Close()

	a := &Anthropic{apiKey: "k", baseURL: srv.URL, client: &http.Client{Timeout: 5 * time.Second}, models: []string{"x"}}
	_, err := a.ChatCompletion(context.Background(), gateway.ChatCompletionRequest{
		Model: "x",
		Messages: []gateway.Message{
			{Role: "user", Content: "hi"},
		},
		Tools: []gateway.Tool{
			{Type: "function", Function: json.RawMessage(`{"name":"fnA","parameters":{}}`)},
			{Type: "function", Function: json.RawMessage(`{"name":"fnB","parameters":{}}`)},
		},
		ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"fnB"}}`),
	})
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if gotReq.ToolChoice == nil || gotReq.ToolChoice.Type != "tool" || gotReq.ToolChoice.Name != "fnB" {
		t.Fatalf("object tool_choice should pin Anthropic (tool, fnB); got %+v", gotReq.ToolChoice)
	}
}

func TestAnthropicToolChoiceNoneDoesNotAddChoiceField(t *testing.T) {
	// "none" in OpenAI has no direct Anthropic equivalent; we simply omit
	// tool_choice. The Anthropic default (auto) applies. The test is a
	// regression guard so we don't accidentally drop tools on the way.
	var gotReq anthropicRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "m", "model": "x", "content": []map[string]any{{"type": "text", "text": "ok"}}, "stop_reason": "end_turn"})
	}))
	defer srv.Close()

	a := &Anthropic{apiKey: "k", baseURL: srv.URL, client: &http.Client{Timeout: 5 * time.Second}, models: []string{"x"}}
	_, _ = a.ChatCompletion(context.Background(), gateway.ChatCompletionRequest{
		Model:      "x",
		Messages:   []gateway.Message{{Role: "user", Content: "hi"}},
		ToolChoice: json.RawMessage(`"none"`),
	})
	if gotReq.ToolChoice != nil {
		t.Fatalf("anthropic should accept 'none' as 'omit'; got %+v", gotReq.ToolChoice)
	}
}
