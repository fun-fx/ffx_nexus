package gateway

import (
	"encoding/json"
	"testing"
)

// The load-bearing property: as an agent works through a question it
// appends its own tool calls and their results, and the key must not move
// while that happens. If it does, one question fans out into N console
// rows again — the exact regression this whole mechanism exists to stop.
func TestDeriveTurnKey_StableAcrossAgentLoop(t *testing.T) {
	system := Message{Role: "system", Content: "You are a coding agent."}
	question := Message{Role: "user", Content: "refactor the reader"}

	call1 := []Message{system, question}
	call2 := append(append([]Message{}, call1...),
		Message{Role: "assistant", ToolCalls: []ToolCall{{Type: "function", ID: "c1"}}},
		Message{Role: "tool", Content: "file contents", ToolCallID: "c1"},
	)
	call3 := append(append([]Message{}, call2...),
		Message{Role: "assistant", ToolCalls: []ToolCall{{Type: "function", ID: "c2"}}},
		Message{Role: "tool", Content: "grep results", ToolCallID: "c2"},
	)

	want := deriveTurnKey("u-1", call1)
	if want == "" {
		t.Fatal("first call in a turn must produce a key")
	}
	for i, msgs := range [][]Message{call2, call3} {
		if got := deriveTurnKey("u-1", msgs); got != want {
			t.Errorf("call %d drifted: got %q, want %q", i+2, got, want)
		}
	}
}

// The other half of the contract: a new question has to break the group,
// even though it shares the entire earlier history.
func TestDeriveTurnKey_ChangesOnNextQuestion(t *testing.T) {
	system := Message{Role: "system", Content: "You are a coding agent."}
	turn1 := []Message{system, {Role: "user", Content: "refactor the reader"}}
	turn2 := append(append([]Message{}, turn1...),
		Message{Role: "assistant", Content: "done"},
		Message{Role: "user", Content: "now add a test"},
	)

	if a, b := deriveTurnKey("u-1", turn1), deriveTurnKey("u-1", turn2); a == b {
		t.Fatalf("a second question must start a new turn (both %q)", a)
	}
}

func TestDeriveTurnKey_ScopedToUserAndSystemPrompt(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "You are a coding agent."},
		{Role: "user", Content: "hello"},
	}
	base := deriveTurnKey("u-1", msgs)

	if other := deriveTurnKey("u-2", msgs); other == base {
		t.Error("two users asking the same thing must not share a turn")
	}

	altSystem := []Message{
		{Role: "system", Content: "You are a writing assistant."},
		{Role: "user", Content: "hello"},
	}
	if other := deriveTurnKey("u-1", altSystem); other == base {
		t.Error("same question under a different system prompt must not share a turn")
	}
}

// "developer" is the Responses-era spelling. Cursor's instructions field
// lands as one or the other depending on which path served the request,
// and a turn must not split just because of that.
func TestDeriveTurnKey_TreatsDeveloperAsSystem(t *testing.T) {
	sys := deriveTurnKey("u-1", []Message{
		{Role: "system", Content: "instructions"},
		{Role: "user", Content: "hi"},
	})
	dev := deriveTurnKey("u-1", []Message{
		{Role: "developer", Content: "instructions"},
		{Role: "user", Content: "hi"},
	})
	if sys != dev {
		t.Errorf("developer and system prompts must key alike: %q vs %q", sys, dev)
	}
}

func TestDeriveTurnKey_EmptyWithoutUserMessage(t *testing.T) {
	cases := map[string][]Message{
		"no messages": nil,
		"system only": {{Role: "system", Content: "be helpful"}},
		"assistant only": {
			{Role: "assistant", Content: "hello"},
			{Role: "tool", Content: "result"},
		},
	}
	for name, msgs := range cases {
		if got := deriveTurnKey("u-1", msgs); got != "" {
			t.Errorf("%s: want empty key, got %q", name, got)
		}
	}
}

// A blank user message still keys — it is a real request the operator
// should see — but must not collide with the no-user-message case, which
// deliberately stays ungrouped.
func TestDeriveTurnKey_BlankUserContentStillKeys(t *testing.T) {
	if got := deriveTurnKey("u-1", []Message{{Role: "user", Content: ""}}); got == "" {
		t.Error("an empty user message is still a turn")
	}
}

func TestDeriveTurnKey_IgnoresSurroundingWhitespace(t *testing.T) {
	a := deriveTurnKey("u-1", []Message{{Role: "user", Content: "explain this"}})
	b := deriveTurnKey("u-1", []Message{{Role: "user", Content: "  explain this\n"}})
	if a != b {
		t.Errorf("whitespace must not split a turn: %q vs %q", a, b)
	}
}

// The Responses path reaches newTrace through toChatRequest, so the key
// has to survive that conversion — function_call_output becoming a
// "tool" message is what keeps the last user message in place.
func TestDeriveTurnKey_StableThroughResponsesConversion(t *testing.T) {
	build := func(items string) []Message {
		req := ResponsesRequest{
			Model:        "gpt-5",
			Instructions: "You are a coding agent.",
			Input:        json.RawMessage(items),
		}
		chat, err := responsesToChat(req)
		if err != nil {
			t.Fatalf("responsesToChat: %v", err)
		}
		return chat.Messages
	}

	first := build(`[{"role":"user","content":"fix the bug"}]`)
	afterTool := build(`[
		{"role":"user","content":"fix the bug"},
		{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"file contents"}
	]`)

	a, b := deriveTurnKey("u-1", first), deriveTurnKey("u-1", afterTool)
	if a == "" {
		t.Fatal("responses turn must produce a key")
	}
	if a != b {
		t.Errorf("tool round-trip split the turn: %q vs %q", a, b)
	}
}
