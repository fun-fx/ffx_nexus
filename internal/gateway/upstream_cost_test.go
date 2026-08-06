package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- ResolveCostUSD precedence ----

// The whole point of reading `usage.estimated_cost` is that a spot market
// price cannot be reproduced from a static table. When the upstream tells
// us what it charged, that number wins outright — including when our own
// table would have produced a different (wrong) answer.
func TestResolveCostUSD_UpstreamBeatsTable(t *testing.T) {
	// gpt-4o-mini would price 1M+1M at 0.15+0.60 = 0.75 from the table.
	got := ResolveCostUSD(0.123456, "gpt-4o-mini", "", 1_000_000, 1_000_000)
	if got != 0.123456 {
		t.Fatalf("ResolveCostUSD = %v; want the upstream-reported 0.123456", got)
	}
}

// A Grid instrument that no static table entry covers must still produce a
// real cost, because The Grid reports it. This is the case that used to
// silently record 0 for every instrument added after the last price review.
func TestResolveCostUSD_UnpricedModelStillCosted(t *testing.T) {
	// gpt-sol-latest has no published per-token price to put in the table,
	// so it is the honest stand-in for "instrument we cannot estimate".
	if p, ok := matchPrice("gpt-sol-latest"); ok {
		t.Fatalf("precondition: gpt-sol-latest should have no table entry, got %+v", p)
	}
	if got := CostUSD("gpt-sol-latest", "", 900, 300); got != 0 {
		t.Fatalf("precondition: table should not price gpt-sol-latest, got %v", got)
	}
	got := ResolveCostUSD(0.0042, "gpt-sol-latest", "", 900, 300)
	if got != 0.0042 {
		t.Fatalf("ResolveCostUSD = %v; want 0.0042 from upstream", got)
	}
}

// Providers that do not report spend (OpenAI, Anthropic, Gemini, Groq,
// Mistral) must keep falling back to the table, so this change cannot
// regress the five providers that were already costed.
func TestResolveCostUSD_FallsBackToTableWhenUpstreamSilent(t *testing.T) {
	got := ResolveCostUSD(0, "gpt-4o-mini", "", 1_000_000, 1_000_000)
	if got != 0.75 {
		t.Fatalf("ResolveCostUSD = %v; want the table's 0.75", got)
	}
}

// A negative or zero report is treated as "not reported" rather than as a
// free call, so a malformed upstream payload cannot zero out billing.
func TestResolveCostUSD_IgnoresNonPositiveUpstream(t *testing.T) {
	if got := ResolveCostUSD(-1, "gpt-4o-mini", "", 1_000_000, 1_000_000); got != 0.75 {
		t.Fatalf("negative upstream cost: got %v, want table fallback 0.75", got)
	}
}

// ---- gpt-4.1-nano must not inherit gpt-4.1's price ----

// `gpt-4.1` is a string prefix of `gpt-4.1-nano`, so before nano had its
// own alias every nano call was billed at 20x its real rate.
func TestPricing_Gpt41NanoNotBilledAsGpt41(t *testing.T) {
	nano := CostUSD("gpt-4.1-nano", "", 1_000_000, 1_000_000)
	full := CostUSD("gpt-4.1", "", 1_000_000, 1_000_000)
	if nano == full {
		t.Fatalf("gpt-4.1-nano billed at gpt-4.1 rate (%v); alias ordering regressed", nano)
	}
	// 0.10 in + 0.40 out per 1M.
	if nano != 0.5 {
		t.Fatalf("gpt-4.1-nano cost = %v; want 0.50", nano)
	}
	// The versioned form must resolve to the same family.
	if v := CostUSD("gpt-4.1-nano-2025-04-14", "", 1_000_000, 1_000_000); v != 0.5 {
		t.Fatalf("versioned nano cost = %v; want 0.50", v)
	}
}

// ---- Grid lab-latest markets price off the supplier id ----

// The Grid's task-tier instruments report `estimated_cost`, but its
// lab-latest markets do not — they pass the supplier's own usage block
// through and name the supplier in `model` (verified against the live API:
// `claude-opus-latest` answers as `anthropic/claude-opus-5`). Those calls
// are therefore priced from the response model, which only works if the
// alias table matches an Opus tier rather than one exact point release.
func TestPricing_LabLatestPricedFromSupplierModel(t *testing.T) {
	for _, resp := range []string{
		"anthropic/claude-opus-5",
		"anthropic/claude-opus-4.8",
	} {
		got := CostUSD("claude-opus-latest", resp, 1_000_000, 1_000_000)
		if got != 90 {
			t.Errorf("CostUSD(claude-opus-latest, %s) = %v; want 90 (Opus tier 15/75)", resp, got)
		}
	}
}

// The older Anthropic generations are priced differently from 4.x, so the
// generic tier aliases must not swallow them.
func TestPricing_LegacyClaudeGenerationsUnaffected(t *testing.T) {
	if got := CostUSD("claude-3-7-sonnet-latest", "", 1_000_000, 1_000_000); got != 18 {
		t.Errorf("claude-3-7-sonnet-latest = %v; want 18", got)
	}
	if got := CostUSD("claude-3-5-haiku-latest", "", 1_000_000, 1_000_000); got != 4.8 {
		t.Errorf("claude-3-5-haiku-latest = %v; want 4.8", got)
	}
}

// ---- Unknown model is rejected before a trace exists ----

// This is why a lab-latest call showed up nowhere in the console rather
// than showing up with a zero cost: resolveChain answers 404 and returns
// before the recorder ever sees a row. The catalog membership itself is
// asserted in internal/gateway/providers (importing it here would cycle).
func TestChatCompletions_UnknownModelIs404(t *testing.T) {
	p := &upstreamCostProvider{modelName: "code-prime", cost: 0.001}
	h := newTestHandler(p)
	rec := doChat(h, `{"model":"claude-opus-latest","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for a model outside the catalog, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("x-nexus-cost-usd") != "" {
		t.Fatalf("a rejected request must not report a cost")
	}
}

// ---- End-to-end: upstream-reported cost reaches the wire ----

// upstreamCostProvider mimics The Grid: it answers on an instrument name
// that no pricing table entry covers and reports its own spend.
type upstreamCostProvider struct {
	modelName string
	cost      float64
	stream    bool
}

func (p *upstreamCostProvider) Name() string     { return "grid" }
func (p *upstreamCostProvider) Models() []string { return []string{p.modelName} }

func (p *upstreamCostProvider) ChatCompletion(_ context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return &ChatCompletionResponse{
		ID:      "chatcmpl-grid",
		Model:   "openai/gpt-oss-120b", // The Grid returns the supplier model.
		Choices: []Choice{{Message: Message{Role: "assistant", Content: "ok"}}},
		Usage: Usage{
			PromptTokens:     55,
			CompletionTokens: 5,
			TotalTokens:      60,
			EstimatedCost:    p.cost,
		},
	}, nil
}

func (p *upstreamCostProvider) ChatCompletionStream(_ context.Context, _ ChatCompletionRequest) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, 3)
	ch <- StreamEvent{Chunk: &ChatCompletionChunk{
		ID: "c1", Object: "chat.completion.chunk", Model: "openai/gpt-oss-120b",
		Choices: []ChunkChoice{{Index: 0, Delta: Delta{Content: "ok"}}},
	}}
	ch <- StreamEvent{Chunk: &ChatCompletionChunk{
		ID: "c1", Object: "chat.completion.chunk", Model: "openai/gpt-oss-120b",
		Choices: []ChunkChoice{{Index: 0, Delta: Delta{}, FinishReason: "stop"}},
		Usage: &Usage{
			PromptTokens:     55,
			CompletionTokens: 5,
			TotalTokens:      60,
			EstimatedCost:    p.cost,
		},
	}}
	ch <- StreamEvent{Done: true}
	close(ch)
	return ch, nil
}

func TestChatCompletions_NonStream_UsesUpstreamReportedCost(t *testing.T) {
	p := &upstreamCostProvider{modelName: "claude-opus-latest", cost: 0.0025}
	h := newTestHandler(p)
	rec := doChat(h, `{"model":"claude-opus-latest","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("x-nexus-cost-usd"); got != "0.002500" {
		t.Fatalf("x-nexus-cost-usd = %q; want 0.002500 from upstream", got)
	}
	if !strings.Contains(rec.Body.String(), `"cost_usd":0.0025`) {
		t.Fatalf("body missing usage.cost_usd=0.0025: %s", rec.Body.String())
	}
	// The upstream's own field is echoed so callers can distinguish a
	// vendor-authoritative number from our local estimate.
	if !strings.Contains(rec.Body.String(), `"estimated_cost":0.0025`) {
		t.Fatalf("body dropped upstream usage.estimated_cost: %s", rec.Body.String())
	}
}

// The streaming cost must be asserted over a real connection, not against
// an httptest.ResponseRecorder. A recorder keeps a plain header map, so a
// Header().Set() issued after WriteHeader still shows up there — while on
// the wire it is silently dropped, because the response head has already
// been flushed. That gap hid a streaming path that reported no cost at all
// to real clients despite a green test.
func TestChatCompletions_Stream_ReportsCostOverTheWire(t *testing.T) {
	p := &upstreamCostProvider{modelName: "code-prime", cost: 0.0189, stream: true}
	h := newTestHandler(p)

	srv := httptest.NewServer(http.HandlerFunc(h.ChatCompletions))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL,
		strings.NewReader(`{"model":"code-prime","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	// Trailers are only readable once the body is drained.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if got := resp.Trailer.Get("x-nexus-cost-usd"); got != "0.018900" {
		t.Errorf("x-nexus-cost-usd trailer = %q; want 0.018900", got)
	}
	// Belt and braces for clients that ignore trailers: the chunk carrying
	// the usage block must also carry the cost.
	if !strings.Contains(string(body), `"cost_usd":0.0189`) {
		t.Errorf("final chunk missing in-band usage.cost_usd: %s", body)
	}
}

// Same wire-level assertion for the Responses SSE path.
func TestResponses_Stream_ReportsCostOverTheWire(t *testing.T) {
	p := &upstreamCostProvider{modelName: "code-prime", cost: 0.0071, stream: true}
	h := newTestHandler(p)

	srv := httptest.NewServer(http.HandlerFunc(h.Responses))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL,
		strings.NewReader(`{"model":"code-prime","input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if got := resp.Trailer.Get("x-nexus-cost-usd"); got != "0.007100" {
		t.Errorf("x-nexus-cost-usd trailer = %q; want 0.007100", got)
	}
	if !strings.Contains(string(body), "response.completed") {
		t.Errorf("stream did not complete: %s", body)
	}
}

// The non-streaming path can still set a normal header, since nothing has
// been written when the cost becomes known.
func TestChatCompletions_NonStream_CostIsARealHeaderNotATrailer(t *testing.T) {
	p := &upstreamCostProvider{modelName: "code-prime", cost: 0.0042}
	h := newTestHandler(p)

	srv := httptest.NewServer(http.HandlerFunc(h.ChatCompletions))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL,
		strings.NewReader(`{"model":"code-prime","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if got := resp.Header.Get("x-nexus-cost-usd"); got != "0.004200" {
		t.Fatalf("x-nexus-cost-usd header = %q; want 0.004200", got)
	}
}

func TestResponses_NonStream_UsesUpstreamReportedCost(t *testing.T) {
	p := &upstreamCostProvider{modelName: "claude-opus-latest", cost: 0.0031}
	h := newTestHandler(p)
	rec := doResponses(h, `{"model":"claude-opus-latest","input":"hi"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("x-nexus-cost-usd"); got != "0.003100" {
		t.Fatalf("x-nexus-cost-usd = %q; want 0.003100 from upstream", got)
	}
}
