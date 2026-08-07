package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- Serialization Contract (ChatCompletions + Responses usage shape) ---

// When cost is zero we MUST omit the field so OpenAI-strict parsers (that
// reject unknown keys) still parse the response cleanly. Callers that want a
// definite value — including zero — read the x-nexus-cost-usd header instead.
func TestUsage_EMitemptyZero(t *testing.T) {
	u := Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "cost_usd") {
		t.Fatalf("zero cost_usd must be omitted, got %s", b)
	}
}

func TestUsage_HasCostUSD(t *testing.T) {
	u := Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000, TotalTokens: 2_000_000, CostUSD: 0.00123}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v, ok := back["cost_usd"]
	if !ok {
		t.Fatalf("cost_usd missing from %s", b)
	}
	// JSON float roundtrips through float64; the value should be ~0.00123.
	if f, _ := v.(float64); f < 0.001 || f > 0.002 {
		t.Fatalf("cost_usd = %v; want ~0.00123", v)
	}
}

func TestResponsesUsage_EMitemptyZero(t *testing.T) {
	u := ResponsesUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "cost_usd") {
		t.Fatalf("zero cost_usd must be omitted, got %s", b)
	}
}

func TestResponsesUsage_HasCostUSD(t *testing.T) {
	u := ResponsesUsage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30, CostUSD: 0.42}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"cost_usd":0.42`) {
		t.Fatalf("cost_usd=0.42 not serialised as expected: %s", b)
	}
}

// ---- Header emission contract ----

func TestSetCostHeader_EmitsEvenWhenZero(t *testing.T) {
	rec := httptest.NewRecorder()
	setCostHeader(rec, 0)
	if got := rec.Header().Get("x-nexus-cost-usd"); got != "0.000000" {
		t.Fatalf("x-nexus-cost-usd = %q, want 0.000000", got)
	}
}

func TestSetCostHeader_FormatsSixDecimals(t *testing.T) {
	rec := httptest.NewRecorder()
	setCostHeader(rec, 0.001234567)
	if got := rec.Header().Get("x-nexus-cost-usd"); got != "0.001235" {
		t.Fatalf("x-nexus-cost-usd = %q, want 0.001235", got)
	}
}

// ---- End-to-end: chat completions non-streaming surfaces cost ----

// costStubProvider returns a fixed response with predictable token counts so
// the test can compute the expected cost directly from the pricing table.
type costStubProvider struct {
	name      string
	modelName string
}

func (p *costStubProvider) Name() string     { return p.name }
func (p *costStubProvider) Models() []string { return []string{p.modelName} }

func (p *costStubProvider) ChatCompletion(_ context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return &ChatCompletionResponse{
		ID:    "chatcmpl-cost-test",
		Model: req.Model,
		Choices: []Choice{{
			Message: Message{Role: "assistant", Content: "ok"},
		}},
		// ~1M prompt + 1M completion → matches gpt-4o-mini pricing directly:
		// 0.15 + 0.60 = 0.75 USD (use this as the expected value below).
		Usage: Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000, TotalTokens: 2_000_000},
	}, nil
}

func (p *costStubProvider) ChatCompletionStream(_ context.Context, _ ChatCompletionRequest) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent)
	close(ch)
	return ch, nil
}

func TestChatCompletions_NonStream_EmitsUsageCostAndHeader(t *testing.T) {
	// Use a model that IS in the pricing table so CostUSD() returns a known
	// non-zero value (and therefore omitempty does NOT drop the field).
	p := &costStubProvider{name: "openai-pro", modelName: "gpt-4o-mini"}
	h := newTestHandler(p)
	rec := doChat(h, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Header MUST be set, even though the body omitempty-decides whether to
	// include the field. For a known-pricing model the values match.
	gotHeader := rec.Header().Get("x-nexus-cost-usd")
	if gotHeader == "" {
		t.Fatalf("missing x-nexus-cost-usd header")
	}
	// gpt-4o-mini: 0.15/1M in + 0.60/1M out = 0.75 → formatted as "0.750000".
	if gotHeader != "0.750000" {
		t.Fatalf("x-nexus-cost-usd = %q, want 0.750000", gotHeader)
	}

	// Body MUST carry usage.cost_usd = 0.75 since the model is priced.
	if !strings.Contains(rec.Body.String(), `"cost_usd":0.75`) {
		t.Fatalf("body missing usage.cost_usd=0.75: %s", rec.Body.String())
	}
}

func TestChatCompletions_NonStream_OmitsBodyFieldWhenCostZero(t *testing.T) {
	// Use a model that isn't in the pricing table → CostUSD returns 0 →
	// omitempty drops the field, but the header MUST still be set to "0".
	p := &costStubProvider{name: "openai-pro", modelName: "gpt-9000-future"}
	h := newTestHandler(p)
	rec := doChat(h, `{"model":"gpt-9000-future","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if got := rec.Header().Get("x-nexus-cost-usd"); got != "0.000000" {
		t.Fatalf("x-nexus-cost-usd = %q, want 0.000000 (header always set)", got)
	}
	if strings.Contains(rec.Body.String(), "cost_usd") {
		t.Fatalf("zero cost must omit cost_usd from body: %s", rec.Body.String())
	}
}

// ---- End-to-end: Responses API non-streaming surfaces cost ----

func doResponses(h *Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.Responses(rec, req)
	return rec
}

func TestResponses_NonStream_EmitsUsageCostAndHeader(t *testing.T) {
	p := &costStubProvider{name: "openai-pro", modelName: "gpt-4o-mini"}
	h := newTestHandler(p)
	rec := doResponses(h, `{"model":"gpt-4o-mini","input":"hi"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("x-nexus-cost-usd"); got != "0.750000" {
		t.Fatalf("x-nexus-cost-usd = %q, want 0.750000", got)
	}
	// Responses shape uses snake_case `usage.cost_usd` per OpenAI convention.
	if !strings.Contains(rec.Body.String(), `"cost_usd":0.75`) {
		t.Fatalf("body missing usage.cost_usd=0.75: %s", rec.Body.String())
	}
}

// ---- Unit: ensure the line-shaped wire output is still OpenAI-parseable ----

func TestChatCompletions_NonStream_BodyStillOpenAIParseable(t *testing.T) {
	// The litmus test for omitempty: when an OpenAI-strict client decodes
	// the body, it should not see "cost_usd":0. When the model IS priced
	// (above), the field should appear unchanged.
	p := &costStubProvider{name: "openai-pro", modelName: "gpt-9000-future"}
	h := newTestHandler(p)
	rec := doChat(h, `{"model":"gpt-9000-future","messages":[{"role":"user","content":"hi"}]}`)

	var generic map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &generic); err != nil {
		t.Fatalf("body must remain valid JSON: %v: %s", err, rec.Body.String())
	}
	usage, ok := generic["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage object missing: %s", rec.Body.String())
	}
	if _, present := usage["cost_usd"]; present {
		t.Fatalf("cost_usd must be omitted for unknown model: %s", rec.Body.String())
	}
	for _, k := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		if _, ok := usage[k]; !ok {
			t.Fatalf("usage missing required field %s: %s", k, rec.Body.String())
		}
	}
}

// TestChatCompletions_NonStream_TotalTokensIsPromptPlusCompletion is a
// regression test for the discrepancy where Read-side totals on the
// Recent-sessions panel must match what the gateway emitted on the
// wire. If usage.total_tokens were ever skipped or set only by the
// upstream stub, the frontend's Tokens column would under/over-count
// and the ClickHouse roll-up would diverge from the client-reported
// value.
func TestChatCompletions_NonStream_TotalTokensIsPromptPlusCompletion(t *testing.T) {
	p := &costStubProvider{name: "openai-pro", modelName: "gpt-4o-mini"}
	h := newTestHandler(p)
	rec := doChat(h, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)

	var resp struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
			CostUSD          float64 `json:"cost_usd"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	if resp.Usage.PromptTokens != 1_000_000 || resp.Usage.CompletionTokens != 1_000_000 {
		t.Fatalf("stub tokens off: prompt=%d completion=%d",
			resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	}
	if resp.Usage.TotalTokens != resp.Usage.PromptTokens+resp.Usage.CompletionTokens {
		t.Fatalf("total_tokens != prompt+completion: %d vs %d+%d",
			resp.Usage.TotalTokens, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	}
	if resp.Usage.CostUSD <= 0 {
		t.Fatalf("cost_usd should be > 0 for gpt-4o-mini: %f", resp.Usage.CostUSD)
	}
}
