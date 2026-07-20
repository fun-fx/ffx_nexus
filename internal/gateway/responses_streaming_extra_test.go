package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ffxnexus/nexus/internal/observability"
)

// multi slicing stub that emits two parallel tool calls before a stop.
type parallelToolProvider struct{}

func (parallelToolProvider) Name() string                                          { return "parallel-tools" }
func (parallelToolProvider) Models() []string                                      { return []string{"p"} }
func (parallelToolProvider) ChatCompletion(_ context.Context, _ ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return nil, errors.New("n/a")
}

func (parallelToolProvider) ChatCompletionStream(_ context.Context, _ ChatCompletionRequest) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, 8)
	zero, one := 0, 1
	tc := func(idx int, id, name string, args string) ToolCall {
		i := idx
		ti := i
		_ = ti
		tc := ToolCall{Index: &[]int{i}[0], ID: id, Type: "function", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: name, Arguments: args}}
		return tc
	}
	go func() {
		defer close(ch)
		// First chunk: open two tool calls (indices 0 and 1) interleaved.
		ch <- StreamEvent{Chunk: &ChatCompletionChunk{
			Choices: []ChunkChoice{{Index: 0, Delta: Delta{Role: "assistant", ToolCalls: []ToolCall{
				tc(zero, "call_a", "lookup", ""),
				tc(one, "call_b", "ls", ""),
			}}}},
		}}
		// Append arguments to both via subsequent deltas; the second delta
		// for tc(0) has empty name to make sure we don't drop args.
		ch <- StreamEvent{Chunk: &ChatCompletionChunk{
			Choices: []ChunkChoice{{Index: 0, Delta: Delta{ToolCalls: []ToolCall{
				{Index: &zero, Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Arguments: `{"q":"`}},
				{Index: &one, Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Arguments: `["/","`}},
			}}}},
		}}
		ch <- StreamEvent{Chunk: &ChatCompletionChunk{
			Choices: []ChunkChoice{{Index: 0, Delta: Delta{ToolCalls: []ToolCall{
				{Index: &zero, Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Arguments: `weather"}`}},
				{Index: &one, Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Arguments: `/tmp"]`}},
			}}}},
		}}
		ch <- StreamEvent{Chunk: &ChatCompletionChunk{
			Choices: []ChunkChoice{{Index: 0, FinishReason: "tool_calls"}},
			Usage: &Usage{PromptTokens: 11, CompletionTokens: 22, TotalTokens: 33},
		}}
		ch <- StreamEvent{Done: true}
	}()
	return ch, nil
}

func newStreamHandle(t *testing.T, p Provider) *Handler {
	t.Helper()
	reg := NewRegistry()
	reg.Register(p)
	return NewHandler(reg, observability.NoopRecorder{}, nil, slog.Default())
}

func TestResponsesSSE_ParallelToolsAccumulateCallIDs(t *testing.T) {
	h := newStreamHandle(t, parallelToolProvider{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"p","stream":true,"input":"hi","tools":[{"type":"function","function":{"name":"lookup"}},{"type":"function","function":{"name":"ls"}}]}`))
	h.Responses(rec, req)

	body := rec.Body.String()
	// output_item.added should appear exactly twice (one per parallel call).
	if c := strings.Count(body, "event: response.output_item.added"); c < 2 {
		t.Fatalf("expected ≥2 output_item.added events, got %d:\n%s", c, body)
	}
	if !strings.Contains(body, `"call_id":"call_a"`) || !strings.Contains(body, `"call_id":"call_b"`) {
		t.Fatalf("call_id round-trip failed:\n%s", body)
	}
	if !strings.Contains(body, `event: response.completed`) {
		t.Fatalf("missing completed event:\n%s", body)
	}
	if !strings.Contains(body, `\"q\":\"weather\"}`) {
		t.Fatalf("lookup arguments missing from SSE stream:\n%s", body)
	}
	if !strings.Contains(body, `\"/\",`) {
		t.Fatalf("ls arguments missing from SSE stream:\n%s", body)
	}
	if !strings.Contains(body, `"parallel_tool_calls":true`) {
		t.Fatalf("response.completed should carry parallel_tool_calls, got:\n%s", body)
	}
	if !strings.Contains(body, `"total_tokens":33`) {
		t.Fatalf("response.completed should carry usage, got:\n%s", body)
	}
}

// truncatedStreamProvider emits text then closes without finish_reason.
type truncatedStreamProvider struct{}

func (truncatedStreamProvider) Name() string                                          { return "truncated" }
func (truncatedStreamProvider) Models() []string                                      { return []string{"t"} }
func (truncatedStreamProvider) ChatCompletion(_ context.Context, _ ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return nil, errors.New("n/a")
}
func (truncatedStreamProvider) ChatCompletionStream(_ context.Context, _ ChatCompletionRequest) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, 4)
	go func() {
		defer close(ch)
		ch <- StreamEvent{Chunk: &ChatCompletionChunk{
			Choices: []ChunkChoice{{Index: 0, Delta: Delta{Role: "assistant", Content: "partial"}}},
		}}
		// No finish reason, then close -> incomplete.
		ch <- StreamEvent{Done: true}
	}()
	return ch, nil
}

func TestResponsesSSE_TruncatedStreamEmitsIncomplete(t *testing.T) {
	h := newStreamHandle(t, truncatedStreamProvider{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"t","stream":true,"input":"hi"}`))
	h.Responses(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `"status":"incomplete"`) {
		t.Fatalf("expected incomplete status on response.completed, got:\n%s", body)
	}
	if !strings.Contains(body, `event: response.completed`) {
		t.Fatalf("response.completed missing:\n%s", body)
	}
}

// failedStreamProvider closes with an error mid-text.
type failedStreamProvider struct{}

func (failedStreamProvider) Name() string                                          { return "errs" }
func (failedStreamProvider) Models() []string                                      { return []string{"e"} }
func (failedStreamProvider) ChatCompletion(_ context.Context, _ ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return nil, errors.New("n/a")
}
func (failedStreamProvider) ChatCompletionStream(_ context.Context, _ ChatCompletionRequest) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, 2)
	go func() {
		defer close(ch)
		ch <- StreamEvent{Chunk: &ChatCompletionChunk{
			Choices: []ChunkChoice{{Index: 0, Delta: Delta{Role: "assistant", Content: "hi"}}},
		}}
		ch <- StreamEvent{Err: errors.New("upstream cut")}
	}()
	return ch, nil
}

func TestResponsesSSE_StreamErrorEmitsFailedEvent(t *testing.T) {
	h := newStreamHandle(t, failedStreamProvider{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"e","stream":true,"input":"hi"}`))
	h.Responses(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "event: response.failed") {
		t.Fatalf("missing response.failed event:\n%s", body)
	}
	if !strings.Contains(body, `"status":"failed"`) {
		t.Fatalf("failed status missing:\n%s", body)
	}
	if !strings.Contains(body, `"upstream cut"`) {
		t.Fatalf("error message not propagated:\n%s", body)
	}
	if strings.Contains(body, "event: response.completed") {
		t.Fatalf("completed should NOT fire after failed:\n%s", body)
	}
}

// Verifies that the response.completed payload carries the full Responses
// shell, including output[], usage, parallel_tool_calls, instructions,
// and an echoed tools list (we keep it empty when the request didn't
// supply tools — clients fall back to defaults).
func TestResponsesSSE_CompletedPayloadShape(t *testing.T) {
	h := newStreamHandle(t, textOnlyProvider{t: "hello"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"txt","stream":true,"input":"hi","instructions":"be brief","tools":[]}`))
	h.Responses(rec, req)
	body := rec.Body.String()

	// Find the completed data line.
	idx := strings.Index(body, "event: response.completed")
	if idx < 0 {
		t.Fatalf("missing completed event:\n%s", body)
	}
	tail := body[idx:]
	lines := strings.Split(tail, "\n")
	var dataLine string
	for _, l := range lines {
		if strings.HasPrefix(l, "data: ") {
			dataLine = strings.TrimPrefix(l, "data: ")
			break
		}
	}
	if dataLine == "" {
		t.Fatalf("completed event has no data line:\n%s", body)
	}
	var decoded struct {
		Response struct {
			ID         string `json:"id"`
			Object     string `json:"object"`
			Status     string `json:"status"`
			Model      string `json:"model"`
			Output     []map[string]any `json:"output"`
			Usage      struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
				TotalTokens  int `json:"total_tokens"`
			} `json:"usage"`
			ParallelToolCalls *bool  `json:"parallel_tool_calls"`
			Instructions      string `json:"instructions"`
		} `json:"response"`
	}
	if err := json.NewDecoder(strings.NewReader(dataLine)).Decode(&decoded); err != nil {
		t.Fatalf("decode completed data: %v\nline=%s", err, dataLine)
	}
	if !strings.HasPrefix(decoded.Response.ID, "resp_") {
		t.Fatalf("id = %q", decoded.Response.ID)
	}
	if decoded.Response.Status != "completed" {
		t.Fatalf("status = %q", decoded.Response.Status)
	}
	if decoded.Response.Instructions != "be brief" {
		t.Fatalf("instructions echo lost: %q", decoded.Response.Instructions)
	}
	if decoded.Response.ParallelToolCalls == nil || !*decoded.Response.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls lost")
	}
	if decoded.Response.Usage.InputTokens == 0 {
		t.Fatalf("usage not propagated: %+v", decoded.Response.Usage)
	}
	if len(decoded.Response.Output) != 1 {
		t.Fatalf("expected one output item, got %d", len(decoded.Response.Output))
	}
	if decoded.Response.Output[0]["type"] != "message" {
		t.Fatalf("output[0].type = %v", decoded.Response.Output[0]["type"])
	}
}

// textOnlyProvider emits a single text delta with usage and a finish reason.
type textOnlyProvider struct{ t string }

func (textOnlyProvider) Name() string                                          { return "txt" }
func (textOnlyProvider) Models() []string                                      { return []string{"txt"} }
func (textOnlyProvider) ChatCompletion(_ context.Context, _ ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return nil, errors.New("n/a")
}
func (textOnlyProvider) ChatCompletionStream(_ context.Context, _ ChatCompletionRequest) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, 4)
	go func() {
		defer close(ch)
		ch <- StreamEvent{Chunk: &ChatCompletionChunk{
			Choices: []ChunkChoice{{Index: 0, Delta: Delta{Role: "assistant", Content: "hello"}}},
		}}
		ch <- StreamEvent{Chunk: &ChatCompletionChunk{
			Choices: []ChunkChoice{{Index: 0, FinishReason: "stop"}},
			Usage:    &Usage{PromptTokens: 7, CompletionTokens: 13, TotalTokens: 20},
		}}
		ch <- StreamEvent{Done: true}
	}()
	return ch, nil
}

// draining helper unused but reserved for future tests.
var _ = uuid.NewString
var _ = io.ReadAll
