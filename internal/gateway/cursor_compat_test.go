package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsCursorHybridRequestDetectsInput(t *testing.T) {
	raw := []byte(`{"model":"f","input":"hi"}`)
	if !IsCursorHybridRequest(raw) {
		t.Fatal("input string should trigger hybrid detection")
	}

	raw = []byte(`{"model":"f","input":[{"role":"user","content":"hi"}]}`)
	if !IsCursorHybridRequest(raw) {
		t.Fatal("input array should trigger hybrid detection")
	}

	raw = []byte(`{"model":"f","messages":[{"role":"user","content":"hi"}]}`)
	if IsCursorHybridRequest(raw) {
		t.Fatal("plain chat message should NOT trigger hybrid detection")
	}
}

func TestIsCursorHybridRequestDetectsCustomTool(t *testing.T) {
	raw := []byte(`{"model":"f","messages":[],"tools":[{"type":"custom","custom":{"name":"patch"}}]}`)
	if !IsCursorHybridRequest(raw) {
		t.Fatal("custom tool should trigger hybrid detection")
	}
}

func TestTransformCursorHybridBasic(t *testing.T) {
	body := []byte(`{
		"model":"fx-gpt-5.5",
		"stream":true,
		"input":[{"role":"user","content":"explain"}],
		"reasoning":{"effort":"low"},
		"max_output_tokens":512,
		"tools":[
			{"type":"function","name":"read_file","description":"r","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}
		]
	}`)
	req, err := TransformCursorHybrid(body)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if req.Model != "fx-gpt-5.5" {
		t.Fatalf("model = %q", req.Model)
	}
	if !req.Stream {
		t.Fatal("stream flag lost")
	}
	if req.ReasoningEffort != "low" {
		t.Fatalf("reasoning effort = %q", req.ReasoningEffort)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 512 {
		t.Fatalf("MaxTokens = %v", req.MaxTokens)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" || req.Messages[0].Content != "explain" {
		t.Fatalf("messages wrong: %+v", req.Messages)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(req.Tools))
	}
	if req.Tools[0].Type != "function" {
		t.Fatalf("tool type = %q", req.Tools[0].Type)
	}
	var fn struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(req.Tools[0].Function, &fn); err != nil {
		t.Fatalf("unmarshal function: %v", err)
	}
	if fn.Name != "read_file" {
		t.Fatalf("name lost: %q", fn.Name)
	}
	if !strings.Contains(string(fn.Parameters), `"path"`) {
		t.Fatalf("parameters lost: %s", fn.Parameters)
	}
}

func TestTransformCursorHybridFunctionCallRoundtrip(t *testing.T) {
	body := []byte(`{
		"model":"f",
		"input":[
			{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"weather\"}"},
			{"type":"function_call_output","call_id":"call_1","name":"lookup","output":{"sunny":true}}
		]
	}`)
	req, err := TransformCursorHybrid(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(req.Messages))
	}
	if req.Messages[0].Role != "assistant" || len(req.Messages[0].ToolCalls) != 1 {
		t.Fatalf("assistant tool call wrong: %+v", req.Messages[0])
	}
	tc := req.Messages[0].ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "lookup" || tc.Function.Arguments != `{"q":"weather"}` {
		t.Fatalf("tc payload wrong: %+v", tc)
	}
	if req.Messages[1].Role != "tool" || req.Messages[1].ToolCallID != "call_1" {
		t.Fatalf("tool result wrong: %+v", req.Messages[1])
	}
	if req.Messages[1].Content != `{"sunny":true}` {
		t.Fatalf("tool output missing: %q", req.Messages[1].Content)
	}
}

func TestTransformCursorHybridAnthropicToolUse(t *testing.T) {
	body := []byte(`{
		"model":"f",
		"input":[
			{"role":"user","content":[{"type":"text","text":"edit file"}]},
			{"type":"tool_use","call_id":"tu1","content":{"name":"write_file","input":{"path":"a.go"}}}
		]
	}`)
	req, err := TransformCursorHybrid(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(req.Messages))
	}
	if req.Messages[1].Role != "assistant" || len(req.Messages[1].ToolCalls) != 1 {
		t.Fatalf("expected assistant with tool call: %+v", req.Messages[1])
	}
	tc := req.Messages[1].ToolCalls[0]
	if tc.Type != "function" || tc.ID != "tu1" || tc.Function.Name != "write_file" {
		t.Fatalf("tc wrong: %+v", tc)
	}
	if tc.Function.Arguments != `{"path":"a.go"}` {
		t.Fatalf("arguments wrong: %q", tc.Function.Arguments)
	}
}

func TestNormaliseToolCustom(t *testing.T) {
	raw := json.RawMessage(`{"type":"custom","custom":{"name":"ApplyPatch","description":"V","format":{"type":"grammar","grammar":{"syntax":"lark","definition":"x"}}}}`)
	t1, err := normaliseTool(raw)
	if err != nil {
		t.Fatal(err)
	}
	if t1.Type != "custom" {
		t.Fatalf("type = %q", t1.Type)
	}
	if !strings.Contains(string(t1.Function), `"ApplyPatch"`) {
		t.Fatalf("custom payload lost: %s", t1.Function)
	}
}

func TestToolCallMarshalCustom(t *testing.T) {
	tc := ToolCall{ID: "ctc_1", Type: "custom"}
	tc.Custom.Name = "ApplyPatch"
	tc.Custom.Input = "@@ -1 +1 @@\n-hi\n+bye"
	b, err := json.Marshal(tc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"type":"custom"`) || !strings.Contains(string(b), `"ApplyPatch"`) {
		t.Fatalf("custom tool call marshalled wrong: %s", b)
	}
}

func TestToolCallMarshalFunction(t *testing.T) {
	tc := ToolCall{ID: "c_1", Type: "function"}
	tc.Function.Name = "ls"
	tc.Function.Arguments = `{"path":"/tmp"}`
	b, err := json.Marshal(tc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"type":"function"`) || !strings.Contains(string(b), `"ls"`) {
		t.Fatalf("function tool call marshalled wrong: %s", b)
	}
	if strings.Contains(string(b), `"custom"`) {
		t.Fatalf("function tool call leaked custom: %s", b)
	}
}

func TestToolCallUnmarshalCustom(t *testing.T) {
	raw := []byte(`{"id":"ctc_1","type":"custom","custom":{"name":"patch","input":"@@"}}`)
	var tc ToolCall
	if err := json.Unmarshal(raw, &tc); err != nil {
		t.Fatal(err)
	}
	if tc.Type != "custom" || tc.Custom.Name != "patch" || tc.Custom.Input != "@@" {
		t.Fatalf("custom tc parse wrong: %+v", tc)
	}
}
