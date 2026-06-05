package evalbatch

import (
	"strings"
	"testing"
)

func TestParseDatasetBasic(t *testing.T) {
	in := `
# a comment line
{"id":"a","model":"m1","input":"hi","output":"hello","reference":"hello"}

{"input":"second","output":"out2"}
{"messages":[{"role":"user","content":"q"}],"output":"o","contexts":["c1","c2"]}
`
	cases, err := parseDataset(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cases) != 3 {
		t.Fatalf("want 3 cases, got %d", len(cases))
	}
	if cases[0].ID != "a" {
		t.Errorf("want id a, got %q", cases[0].ID)
	}
	if cases[1].ID != "case-2" {
		t.Errorf("want auto id case-2, got %q", cases[1].ID)
	}
	if got := cases[2].ChatMessages(); len(got) != 1 || got[0].Content != "q" {
		t.Errorf("messages not preserved: %+v", got)
	}
	if len(cases[2].Contexts) != 2 {
		t.Errorf("want 2 contexts, got %d", len(cases[2].Contexts))
	}
}

func TestChatMessagesFromInput(t *testing.T) {
	c := Case{Input: "ping"}
	msgs := c.ChatMessages()
	if len(msgs) != 1 || msgs[0].Role != "user" || msgs[0].Content != "ping" {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
}

func TestParseDatasetRejectsEmptyCase(t *testing.T) {
	_, err := parseDataset(strings.NewReader(`{"output":"x"}`))
	if err == nil {
		t.Fatal("expected error for case with no input/messages")
	}
}

func TestParseDatasetRejectsBadJSON(t *testing.T) {
	_, err := parseDataset(strings.NewReader(`{not json}`))
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestParseDatasetEmpty(t *testing.T) {
	_, err := parseDataset(strings.NewReader("\n# only a comment\n"))
	if err == nil {
		t.Fatal("expected error for empty dataset")
	}
}
