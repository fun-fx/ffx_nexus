package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ffxnexus/nexus/internal/guardrails"
)

// scriptedProvider returns a scripted content per call and records every request
// it received, so self-correction behavior can be asserted deterministically.
type scriptedProvider struct {
	name     string
	models   []string
	contents []string // content returned on call N (last is reused if exhausted)
	requests []ChatCompletionRequest
}

func (p *scriptedProvider) Name() string     { return p.name }
func (p *scriptedProvider) Models() []string { return p.models }

func (p *scriptedProvider) ChatCompletion(_ context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	p.requests = append(p.requests, req)
	idx := len(p.requests) - 1
	content := p.contents[len(p.contents)-1]
	if idx < len(p.contents) {
		content = p.contents[idx]
	}
	return &ChatCompletionResponse{
		Model:   req.Model,
		Choices: []Choice{{Message: Message{Role: "assistant", Content: content}}},
		Usage:   Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}, nil
}

func (p *scriptedProvider) ChatCompletionStream(_ context.Context, _ ChatCompletionRequest) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, 1)
	ch <- StreamEvent{Done: true}
	close(ch)
	return ch, nil
}

func jsonModeBody() string {
	return `{"model":"m","messages":[{"role":"user","content":"give me json"}],"response_format":{"type":"json_object"}}`
}

func TestSelfCorrectionFixesInvalidJSON(t *testing.T) {
	p := &scriptedProvider{name: "p", models: []string{"m"}, contents: []string{"sorry, here: nope", `{"ok":true}`}}
	h := newTestHandler(p)
	h.SetGuard(guardrails.New(guardrails.Config{Enabled: true, ValidateJSONOutput: true}))
	h.SetSelfCorrection(1)

	rec := doChat(h, jsonModeBody())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 after correction, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(p.requests) != 2 {
		t.Fatalf("want 2 upstream calls (original + 1 correction), got %d", len(p.requests))
	}
	// The correction request must include the bad output and a user instruction.
	corr := p.requests[1].Messages
	if len(corr) != 3 {
		t.Fatalf("want 3 messages in correction request, got %d: %+v", len(corr), corr)
	}
	if corr[1].Role != "assistant" || corr[1].Content != "sorry, here: nope" {
		t.Errorf("correction must echo the rejected output, got %+v", corr[1])
	}
	if corr[2].Role != "user" {
		t.Errorf("correction must end with a user instruction, got %+v", corr[2])
	}
	var resp ChatCompletionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Choices[0].Message.Content != `{"ok":true}` {
		t.Errorf("want corrected JSON returned, got %q", resp.Choices[0].Message.Content)
	}
}

func TestSelfCorrectionExhaustsRetries(t *testing.T) {
	p := &scriptedProvider{name: "p", models: []string{"m"}, contents: []string{"bad1", "bad2", "bad3"}}
	h := newTestHandler(p)
	h.SetGuard(guardrails.New(guardrails.Config{Enabled: true, ValidateJSONOutput: true}))
	h.SetSelfCorrection(2)

	rec := doChat(h, jsonModeBody())
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 after exhausting retries, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(p.requests) != 3 {
		t.Fatalf("want 3 calls (original + 2 retries), got %d", len(p.requests))
	}
	var env APIError
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Type != "schema_validation_failed" {
		t.Errorf("want schema_validation_failed, got %q", env.Error.Type)
	}
}

func TestSelfCorrectionDisabledFailsImmediately(t *testing.T) {
	p := &scriptedProvider{name: "p", models: []string{"m"}, contents: []string{"not json", `{"ok":true}`}}
	h := newTestHandler(p)
	h.SetGuard(guardrails.New(guardrails.Config{Enabled: true, ValidateJSONOutput: true}))
	// SetSelfCorrection not called -> disabled.

	rec := doChat(h, jsonModeBody())
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 when self-correction disabled, got %d", rec.Code)
	}
	if len(p.requests) != 1 {
		t.Fatalf("disabled self-correction must not retry, got %d calls", len(p.requests))
	}
}

func TestSelfCorrectionNoOpOnValidFirstResponse(t *testing.T) {
	p := &scriptedProvider{name: "p", models: []string{"m"}, contents: []string{`{"ok":true}`, "should-not-be-used"}}
	h := newTestHandler(p)
	h.SetGuard(guardrails.New(guardrails.Config{Enabled: true, ValidateJSONOutput: true}))
	h.SetSelfCorrection(3)

	rec := doChat(h, jsonModeBody())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(p.requests) != 1 {
		t.Fatalf("valid first response must not trigger correction, got %d calls", len(p.requests))
	}
}

func TestSelfCorrectionDoesNotMutateOriginalMessages(t *testing.T) {
	p := &scriptedProvider{name: "p", models: []string{"m"}, contents: []string{"bad", `{"ok":true}`}}
	h := newTestHandler(p)
	h.SetGuard(guardrails.New(guardrails.Config{Enabled: true, ValidateJSONOutput: true}))
	h.SetSelfCorrection(1)

	rec := doChat(h, jsonModeBody())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	// The first (original) request must carry exactly the one user message.
	if len(p.requests[0].Messages) != 1 {
		t.Fatalf("original request messages mutated: %+v", p.requests[0].Messages)
	}
}
