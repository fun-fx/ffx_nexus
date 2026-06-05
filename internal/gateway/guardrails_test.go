package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ffxnexus/nexus/internal/guardrails"
)

// piiProvider returns a fixed response containing PII so output redaction can
// be exercised.
type piiProvider struct {
	name    string
	models  []string
	content string
	calls   *int
}

func (p *piiProvider) Name() string     { return p.name }
func (p *piiProvider) Models() []string { return p.models }

func (p *piiProvider) ChatCompletion(_ context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	if p.calls != nil {
		*p.calls++
	}
	return &ChatCompletionResponse{
		Model:   req.Model,
		Choices: []Choice{{Message: Message{Role: "assistant", Content: p.content}}},
		Usage:   Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}, nil
}

func (p *piiProvider) ChatCompletionStream(_ context.Context, _ ChatCompletionRequest) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, 1)
	ch <- StreamEvent{Done: true}
	close(ch)
	return ch, nil
}

func TestGuardrailBlocksPIIInput(t *testing.T) {
	calls := 0
	p := &piiProvider{name: "p", models: []string{"m"}, content: "fine", calls: &calls}
	h := newTestHandler(p)
	h.SetGuard(guardrails.New(guardrails.Config{Enabled: true, BlockPIIInput: true}))

	rec := doChat(h, `{"model":"m","messages":[{"role":"user","content":"my ssn is 123-45-6789"}]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 for blocked input, got %d: %s", rec.Code, rec.Body.String())
	}
	if calls != 0 {
		t.Fatalf("upstream must not be called when input is blocked, got %d calls", calls)
	}
	var env APIError
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Type != "guardrail_blocked" {
		t.Fatalf("want guardrail_blocked error type, got %q", env.Error.Type)
	}
}

func TestGuardrailRedactsPIIOutput(t *testing.T) {
	p := &piiProvider{name: "p", models: []string{"m"}, content: "reach me at jane@example.com"}
	h := newTestHandler(p)
	h.SetGuard(guardrails.New(guardrails.Config{Enabled: true, RedactPIIOutput: true}))

	rec := doChat(h, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp ChatCompletionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if got := resp.Choices[0].Message.Content; got == p.content || got != "reach me at [REDACTED]" {
		t.Fatalf("expected redacted output, got %q", got)
	}
}

func TestGuardrailAllowsCleanRequest(t *testing.T) {
	p := &piiProvider{name: "p", models: []string{"m"}, content: "all good"}
	h := newTestHandler(p)
	h.SetGuard(guardrails.New(guardrails.Config{Enabled: true, BlockPIIInput: true, RedactPIIOutput: true}))

	rec := doChat(h, `{"model":"m","messages":[{"role":"user","content":"a normal question"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clean request should pass, got %d: %s", rec.Code, rec.Body.String())
	}
}
