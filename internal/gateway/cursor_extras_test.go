package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormaliseHybridToolChoice_StringPassthrough(t *testing.T) {
	in := json.RawMessage(`"auto"`)
	got := normaliseHybridToolChoice(in)
	if string(got) != `"auto"` {
		t.Fatalf("string form lost: %s", got)
	}
}

func TestNormaliseHybridToolChoice_FlatToNested(t *testing.T) {
	in := json.RawMessage(`{"type":"function","name":"search"}`)
	got := normaliseHybridToolChoice(in)
	if !strings.Contains(string(got), `"function":{"name":"search"}`) {
		t.Fatalf("flat shape should nest: %s", got)
	}
}

func TestNormaliseHybridToolChoice_AlreadyNestedPassthrough(t *testing.T) {
	in := json.RawMessage(`{"type":"function","function":{"name":"search"}}`)
	got := normaliseHybridToolChoice(in)
	if string(got) != string(in) {
		t.Fatalf("nested payload should pass through unchanged: %s", got)
	}
}

func TestWrapApplyPatchGrammar_PreservesLark(t *testing.T) {
	custom := json.RawMessage(`{
		"name":"ApplyPatch",
		"description":"V4A",
		"format":{"type":"grammar","grammar":{"syntax":"lark","definition":"start: \":)V4A\" | \":)V4A\\n\""}}
	}`)
	out, ok := wrapApplyPatchGrammar(custom)
	if !ok {
		t.Fatalf("expected wrap success")
	}
	if !strings.Contains(string(out), `"ApplyPatch"`) {
		t.Fatalf("name lost: %s", out)
	}
	if !strings.Contains(string(out), `"format"`) {
		t.Fatalf("grammar parameters.format lost: %s", out)
	}
	if !strings.Contains(string(out), `"lark"`) {
		t.Fatalf("lark definition lost: %s", out)
	}
}

func TestPickResponsesExtras_OmitsPromotedKeys(t *testing.T) {
	body := []byte(`{
		"model":"x","input":"hi",
		"store":true,
		"include":["reasoning.encrypted_content"],
		"prompt_cache_key":"abc",
		"parallel_tool_calls":true,
		"tool_choice":"auto"
	}`)
	extras := pickResponsesExtras(body)
	if extras == nil {
		t.Fatal("expected extras")
	}
	if _, has := extras["store"]; !has {
		t.Fatalf("store missing: %+v", extras)
	}
	if _, has := extras["prompt_cache_key"]; !has {
		t.Fatalf("prompt_cache_key missing: %+v", extras)
	}
	if _, has := extras["parallel_tool_calls"]; has {
		t.Fatalf("parallel_tool_calls should not leak into extras: %+v", extras)
	}
	if _, has := extras["tool_choice"]; has {
		t.Fatalf("tool_choice should not leak into extras: %+v", extras)
	}
}

func TestChatCompletionRequestMarshalSplicesExtra(t *testing.T) {
	req := ChatCompletionRequest{
		Model: "fx-gpt-5.5",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Extra: map[string]json.RawMessage{
			"store":           json.RawMessage(`true`),
			"prompt_cache_key": json.RawMessage(`"abc"`),
		},
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"store":true`) {
		t.Fatalf("store not spliced: %s", b)
	}
	if !strings.Contains(string(b), `"prompt_cache_key":"abc"`) {
		t.Fatalf("prompt_cache_key not spliced: %s", b)
	}
	if !strings.Contains(string(b), `"messages"`) {
		t.Fatalf("messages lost during splice: %s", b)
	}
}

func TestTransformCursorHybrid_PreservesExtrasAndToolChoice(t *testing.T) {
	body := []byte(`{
		"model":"fx-gpt-5.5",
		"input":[{"role":"user","content":"hi"}],
		"tool_choice":{"type":"function","name":"search"},
		"parallel_tool_calls":false,
		"store":true,
		"prompt_cache_key":"abc",
		"reasoning":{"effort":"low"}
	}`)
	req, err := TransformCursorHybrid(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(req.ToolChoice), `"function":{"name":"search"}`) {
		t.Fatalf("tool_choice should be nested: %s", req.ToolChoice)
	}
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls != false {
		t.Fatalf("parallel_tool_calls lost: %v", req.ParallelToolCalls)
	}
	if req.Extra == nil || string(req.Extra["store"]) != "true" {
		t.Fatalf("store extra missing: %+v", req.Extra)
	}
	if req.Extra == nil || string(req.Extra["prompt_cache_key"]) != `"abc"` {
		t.Fatalf("prompt_cache_key extra missing: %+v", req.Extra)
	}
	if req.ReasoningEffort != "low" {
		t.Fatalf("reasoning_effort = %q", req.ReasoningEffort)
	}
	if _, has := req.Extra["parallel_tool_calls"]; has {
		t.Fatalf("parallel_tool_calls leaked: %+v", req.Extra)
	}
}
