package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/observability"
)

// stubProvider is a controllable Provider for handler tests.
type stubProvider struct {
	name   string
	models []string
	fail   bool
	calls  *int
}

func (s *stubProvider) Name() string     { return s.name }
func (s *stubProvider) Models() []string { return s.models }

func (s *stubProvider) ChatCompletion(_ context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	if s.calls != nil {
		*s.calls++
	}
	if s.fail {
		return nil, errors.New("upstream down: " + s.name)
	}
	return &ChatCompletionResponse{
		Model:   req.Model,
		Choices: []Choice{{Message: Message{Role: "assistant", Content: "ok from " + s.name}}},
		Usage:   Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}, nil
}

func (s *stubProvider) ChatCompletionStream(_ context.Context, _ ChatCompletionRequest) (<-chan StreamEvent, error) {
	if s.fail {
		return nil, errors.New("upstream down: " + s.name)
	}
	ch := make(chan StreamEvent, 1)
	ch <- StreamEvent{Done: true}
	close(ch)
	return ch, nil
}

// stubRouter returns a fixed ranking, ignoring stats.
type stubRouter struct{ chain []string }

func (s stubRouter) Rank(candidates []string, _ float64) []string {
	// Intersect the fixed chain with the allowed candidates, preserving order.
	allowed := map[string]bool{}
	for _, c := range candidates {
		allowed[c] = true
	}
	var out []string
	for _, m := range s.chain {
		if allowed[m] {
			out = append(out, m)
		}
	}
	return out
}

func newTestHandler(providers ...Provider) *Handler {
	reg := NewRegistry()
	for _, p := range providers {
		reg.Register(p)
	}
	return NewHandler(reg, observability.NoopRecorder{}, nil, slog.Default())
}

func doChat(h *Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, req)
	return rec
}

func TestUnaryFailoverToNextCandidate(t *testing.T) {
	badCalls, goodCalls := 0, 0
	bad := &stubProvider{name: "bad", models: []string{"bad-model"}, fail: true, calls: &badCalls}
	good := &stubProvider{name: "good", models: []string{"good-model"}, fail: false, calls: &goodCalls}

	h := newTestHandler(bad, good)
	h.SetRouter(stubRouter{chain: []string{"bad-model", "good-model"}}, map[string][]string{
		"grp": {"bad-model", "good-model"},
	})

	rec := doChat(h, `{"model":"grp","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 after failover, got %d: %s", rec.Code, rec.Body.String())
	}
	if badCalls != 1 || goodCalls != 1 {
		t.Fatalf("expected bad=1 good=1 calls, got bad=%d good=%d", badCalls, goodCalls)
	}
	var resp ChatCompletionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Choices[0].Message.Content != "ok from good" {
		t.Fatalf("expected response from 'good', got %q", resp.Choices[0].Message.Content)
	}
}

func TestUnaryAllCandidatesFail(t *testing.T) {
	bad1 := &stubProvider{name: "bad1", models: []string{"m1"}, fail: true}
	bad2 := &stubProvider{name: "bad2", models: []string{"m2"}, fail: true}

	h := newTestHandler(bad1, bad2)
	h.SetRouter(stubRouter{chain: []string{"m1", "m2"}}, map[string][]string{"grp": {"m1", "m2"}})

	rec := doChat(h, `{"model":"grp","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502 when all fail, got %d", rec.Code)
	}
}

func TestConcreteModelNoFallback(t *testing.T) {
	calls := 0
	bad := &stubProvider{name: "bad", models: []string{"bad-model"}, fail: true, calls: &calls}
	good := &stubProvider{name: "good", models: []string{"good-model"}, fail: false}

	h := newTestHandler(bad, good)
	h.SetRouter(stubRouter{chain: []string{"bad-model", "good-model"}}, nil)

	// Direct concrete model request must NOT fall back to other models.
	rec := doChat(h, `{"model":"bad-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("concrete model failure should be 502, got %d", rec.Code)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 attempt for concrete model, got %d", calls)
	}
}

func TestStreamFailoverOpensNextCandidate(t *testing.T) {
	bad := &stubProvider{name: "bad", models: []string{"bad-model"}, fail: true}
	good := &stubProvider{name: "good", models: []string{"good-model"}, fail: false}

	h := newTestHandler(bad, good)
	h.SetRouter(stubRouter{chain: []string{"bad-model", "good-model"}}, map[string][]string{
		"grp": {"bad-model", "good-model"},
	})

	rec := doChat(h, `{"model":"grp","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream should open on the good candidate, got %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("expected SSE [DONE], got %q", string(body))
	}
}
