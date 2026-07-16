package gateway

import (
	"encoding/json"
	"testing"
)

func TestMessageUnmarshalStringContent(t *testing.T) {
	raw := `{"role":"user","content":"hello"}`
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if m.Content != "hello" {
		t.Fatalf("content = %q", m.Content)
	}
}

func TestMessageUnmarshalArrayContent(t *testing.T) {
	raw := `{"role":"user","content":[{"type":"text","text":"line one"},{"type":"text","text":"line two"}]}`
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	want := "line one\nline two"
	if m.Content != want {
		t.Fatalf("content = %q, want %q", m.Content, want)
	}
}

func TestChatCompletionRequestCursorStyleContent(t *testing.T) {
	raw := `{
		"model":"code-standard",
		"messages":[
			{"role":"system","content":[{"type":"text","text":"You are helpful"}]},
			{"role":"user","content":[{"type":"text","text":"Say hi"}]}
		]
	}`
	var req ChatCompletionRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages len = %d", len(req.Messages))
	}
	if req.Messages[1].Content != "Say hi" {
		t.Fatalf("user content = %q", req.Messages[1].Content)
	}
}
