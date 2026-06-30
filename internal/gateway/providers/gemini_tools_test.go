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

func TestGeminiToolsForwardedAndFunctionCallUnwrapped(t *testing.T) {
	var gotReq geminiRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{
				"content": map[string]any{
					"role": "model",
					"parts": []map[string]any{
						{"text": "Let me check."},
						{"functionCall": map[string]any{
							"name": "search_docs",
							"args": map[string]any{"q": "agent gateway"},
						}},
					},
				},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]any{
				"promptTokenCount":     11,
				"candidatesTokenCount": 4,
				"totalTokenCount":      15,
			},
		})
	}))
	defer srv.Close()

	g := &Gemini{
		apiKey:  "gem-test",
		baseURL: srv.URL,
		client:  &http.Client{Timeout: 5 * time.Second},
		models:  []string{"gemini-2.5-flash"},
	}

	resp, err := g.ChatCompletion(context.Background(), gateway.ChatCompletionRequest{
		Model: "gemini-2.5-flash",
		Messages: []gateway.Message{
			{Role: "user", Content: "search for agent gateway"},
			{
				Role: "assistant", Content: "",
				ToolCalls: []gateway.ToolCall{{
					Type: "function",
					ID:   "call_search_docs_prev",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "search_docs", Arguments: `{"q":"embedding"}`},
				}},
			},
			{Role: "tool", ToolCallID: "search_docs", Content: `{"hits":3}`},
		},
		Tools: []gateway.Tool{
			{
				Type: "function",
				Function: json.RawMessage(`{
					"name":"search_docs",
					"description":"search the corpus",
					"parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}
				}`),
			},
		},
		ToolChoice: json.RawMessage(`"required"`),
	})
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	// Upstream: functionDeclarations
	if len(gotReq.Tools) != 1 || len(gotReq.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("declarations not forwarded; got %+v", gotReq.Tools)
	}
	if gotReq.Tools[0].FunctionDeclarations[0].Name != "search_docs" {
		t.Fatalf("name lost: %q", gotReq.Tools[0].FunctionDeclarations[0].Name)
	}
	if !strings.Contains(string(gotReq.Tools[0].FunctionDeclarations[0].Parameters), `"q"`) {
		t.Fatalf("parameters (was 'parameters') not forwarded; got %s", gotReq.Tools[0].FunctionDeclarations[0].Parameters)
	}
	// Tool config
	if gotReq.ToolConfig == nil || gotReq.ToolConfig.FunctionCallingConfig.Mode != "ANY" {
		t.Fatalf("tool_choice 'required' should map to ANY; got %+v", gotReq.ToolConfig)
	}
	// Last user message is the tool result, with FunctionResponse part.
	last := gotReq.Contents[len(gotReq.Contents)-1]
	if last.Role != "user" {
		t.Fatalf("tool result should map to user role in Gemini; got %q", last.Role)
	}
	if len(last.Parts) != 1 || last.Parts[0].FunctionResponse == nil || last.Parts[0].FunctionResponse.Name != "search_docs" {
		t.Fatalf("functionResponse part missing: %+v", last.Parts)
	}
	// Assistant's prior tool call surfaces as a functionCall part.
	second := gotReq.Contents[len(gotReq.Contents)-2]
	if second.Role != "model" || len(second.Parts) != 1 || second.Parts[0].FunctionCall == nil {
		t.Fatalf("previous assistant's toolCall should land as FunctionCall part; got %+v", second)
	}

	// Response: functionCall part -> Choice.Message.ToolCalls.
	if len(resp.Choices) != 1 || len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("functionCall should land on ToolCalls; got %+v", resp.Choices)
	}
	tc := resp.Choices[0].Message.ToolCalls[0]
	if tc.Function.Name != "search_docs" || tc.Function.Arguments != `{"q":"agent gateway"}` {
		t.Fatalf("tool call unwrap mismatch: %+v", tc)
	}
	if resp.Choices[0].FinishReason != "stop" {
		// Gemini emits STOP for both plain completion and tool_use turns.
		// The downstream consumer should discover tool turns via the
		// presence of Message.ToolCalls on the choice, not via
		// finish_reason — which the gateway preserves unchanged.
		t.Fatalf("finish reason should preserve the upstream value ('stop' from Gemini STOP); got %q", resp.Choices[0].FinishReason)
	}
}

func TestGeminiToolChoiceObjectPinsName(t *testing.T) {
	var gotReq geminiRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{
				"content":      map[string]any{"role": "model", "parts": []map[string]any{{"text": "ok"}}},
				"finishReason": "STOP",
			}},
		})
	}))
	defer srv.Close()

	g := &Gemini{apiKey: "k", baseURL: srv.URL, client: &http.Client{Timeout: 5 * time.Second}, models: []string{"x"}}
	_, err := g.ChatCompletion(context.Background(), gateway.ChatCompletionRequest{
		Model:    "x",
		Messages: []gateway.Message{{Role: "user", Content: "hi"}},
		Tools: []gateway.Tool{
			{Type: "function", Function: json.RawMessage(`{"name":"fnA","parameters":{}}`)},
			{Type: "function", Function: json.RawMessage(`{"name":"fnB","parameters":{}}`)},
		},
		ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"fnA"}}`),
	})
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if gotReq.ToolConfig == nil ||
		gotReq.ToolConfig.FunctionCallingConfig.Mode != "ANY" ||
		len(gotReq.ToolConfig.FunctionCallingConfig.AllowedFunctionNames) != 1 ||
		gotReq.ToolConfig.FunctionCallingConfig.AllowedFunctionNames[0] != "fnA" {
		t.Fatalf("object tool_choice should pin Gemini to ANY + [fnA]; got %+v", gotReq.ToolConfig)
	}
}

func TestGeminiToolChoiceNoneDisablesCalls(t *testing.T) {
	var gotReq geminiRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{
				"content":      map[string]any{"role": "model", "parts": []map[string]any{{"text": "ok"}}},
				"finishReason": "STOP",
			}},
		})
	}))
	defer srv.Close()
	g := &Gemini{apiKey: "k", baseURL: srv.URL, client: &http.Client{Timeout: 5 * time.Second}, models: []string{"x"}}
	_, _ = g.ChatCompletion(context.Background(), gateway.ChatCompletionRequest{
		Model:      "x",
		Messages:   []gateway.Message{{Role: "user", Content: "hi"}},
		Tools:      []gateway.Tool{{Type: "function", Function: json.RawMessage(`{"name":"fnA","parameters":{}}`)}},
		ToolChoice: json.RawMessage(`"none"`),
	})
	if gotReq.ToolConfig == nil || gotReq.ToolConfig.FunctionCallingConfig.Mode != "NONE" {
		t.Fatalf("tool_choice 'none' should map to NONE; got %+v", gotReq.ToolConfig)
	}
}
