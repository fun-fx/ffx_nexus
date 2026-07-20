package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/observability"
)

// streamingStubProvider feeds a canned sequence of SSE chunks, mimicking an
// OpenAI-compatible provider that is gradually emitting one text message and
// one tool call.
type streamingStubProvider struct {
	streamEvents []StreamEvent
}

func (s *streamingStubProvider) Name() string     { return "stream-stub" }
func (s *streamingStubProvider) Models() []string { return []string{"m"} }

func (s *streamingStubProvider) ChatCompletion(_ context.Context, _ ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return nil, errors.New("not used in stream tests")
}

func (s *streamingStubProvider) ChatCompletionStream(_ context.Context, _ ChatCompletionRequest) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, len(s.streamEvents)+1)
	for _, e := range s.streamEvents {
		ch <- e
	}
	ch <- StreamEvent{Done: true}
	close(ch)
	return ch, nil
}

func newStreamTestHandler(p Provider) *Handler {
	reg := NewRegistry()
	reg.Register(p)
	return NewHandler(reg, observability.NoopRecorder{}, nil, slog.Default())
}

// TestResponsesSSEQ1StreamTextOnly checks that the Responses SSE stream emits
// the canonical sequence for plain text output.
func TestResponsesSSEStreamTextOnly(t *testing.T) {
	idx0 := 0
	provider := &streamingStubProvider{
		streamEvents: []StreamEvent{
			{Chunk: &ChatCompletionChunk{ID: "c1", Object: "chat.completion.chunk", Model: "m", Choices: []ChunkChoice{{Index: 0, Delta: Delta{Role: "assistant", Content: ""}}}}, Raw: nil},
			{Chunk: &ChatCompletionChunk{ID: "c1", Object: "chat.completion.chunk", Model: "m", Choices: []ChunkChoice{{Index: 0, Delta: Delta{Content: "Hello"}}}}, Raw: nil, Done: false},
			{Chunk: &ChatCompletionChunk{ID: "c1", Object: "chat.completion.chunk", Model: "m", Choices: []ChunkChoice{{Index: 0, Delta: Delta{Content: " world"}}}}, Raw: nil, Done: false},
			{Chunk: &ChatCompletionChunk{ID: "c1", Object: "chat.completion.chunk", Model: "m", Choices: []ChunkChoice{{Index: 0, Delta: Delta{}, FinishReason: "stop"}}, Usage: &Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}}, Raw: nil, Done: false},
		},
	}
	_ = idx0
	h := newStreamTestHandler(provider)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"m","input":"hello","stream":true}`))
	h.Responses(rec, req)

	body := rec.Body.String()
	// Order matters: created → added(output_item added) → delta → text.done → item.done → completed.
	wantOrder := []string{
		"event: response.created",
		`event: response.output_item.added`,
		`event: response.output_text.delta`,
		`event: response.output_text.done`,
		`event: response.output_item.done`,
		"event: response.completed",
	}
	prevIdx := -1
	for _, ev := range wantOrder {
		idx := strings.Index(body, ev)
		if idx < 0 {
			t.Fatalf("missing event %q in SSE stream:\n%s", ev, body)
		}
		if idx <= prevIdx {
			t.Fatalf("event %q appeared out of order in:\n%s", ev, body)
		}
		prevIdx = idx
	}
	if !strings.Contains(body, `"text":"Hello world"`) {
		t.Fatalf("output_text.done should carry the merged text, got:\n%s", body)
	}
}

// TestResponsesSSEQ2StreamToolCall verifies tool-call deltas are accumulated
// into a single arguments.done event.
func TestResponsesSSEStreamToolCall(t *testing.T) {
	zero := 0
	one := 1
	provider := &streamingStubProvider{
		streamEvents: []StreamEvent{
			{Chunk: &ChatCompletionChunk{Model: "m", Choices: []ChunkChoice{{Index: 0, Delta: Delta{Role: "assistant", ToolCalls: []ToolCall{
				{Index: &zero, ID: "c1", Type: "function", Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "lookup"}},
			}}}}}},
			{Chunk: &ChatCompletionChunk{Model: "m", Choices: []ChunkChoice{{Index: 0, Delta: Delta{ToolCalls: []ToolCall{
				{Index: &zero, Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Arguments: `{"q":"`}},
			}}}}}},
			{Chunk: &ChatCompletionChunk{Model: "m", Choices: []ChunkChoice{{Index: 0, Delta: Delta{ToolCalls: []ToolCall{
				{Index: &one, ID: "c2", Type: "function", Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "ls"}},
			}}}}}},
			{Chunk: &ChatCompletionChunk{Model: "m", Choices: []ChunkChoice{{Index: 0, Delta: Delta{ToolCalls: []ToolCall{
				{Index: &zero, Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Arguments: `weather"}`}},
			}}}}}},
			{Chunk: &ChatCompletionChunk{Model: "m", Choices: []ChunkChoice{{Index: 0, FinishReason: "tool_calls"}}}},
		},
	}
	h := newStreamTestHandler(provider)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"m","stream":true,"input":[{"role":"user","content":"q"}]}`))
	h.Responses(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "response.output_item.added") {
		t.Fatalf("missing output_item.added event:\n%s", body)
	}
	// Two tool calls => at least two function_call_arguments.delta events.
	if c := strings.Count(body, "event: response.function_call_arguments.delta"); c < 2 {
		t.Fatalf("expected ≥2 args.delta events, got %d:\n%s", c, body)
	}
	if c := strings.Count(body, "event: response.function_call_arguments.done"); c < 2 {
		t.Fatalf("expected ≥2 args.done events, got %d:\n%s", c, body)
	}
	if !strings.Contains(body, `"name":"lookup"`) || !strings.Contains(body, `"name":"ls"`) {
		t.Fatalf("tool names missing in SSE body:\n%s", body)
	}
	// Combined arguments should appear in the arguments.done event for c1.
	// JSON in SSE encoding escapes inner quotes with backslashes, so look
	// for either escaped or raw form.
	if !strings.Contains(body, `\"q\":\"`+"weather"+`\"`+"}") &&
		!strings.Contains(body, `"arguments":"{\"q\":\"weather\"}"`) {
		t.Fatalf("accumulated arguments missing from stream:\n%s", body)
	}
	if !strings.Contains(body, `event: response.completed`) {
		t.Fatalf("missing completion event:\n%s", body)
	}
}

// helper: pretty-print decoded SSE events from a buffer for debugging
func dumpSSE(b string) {
	reader := bufio.NewReader(strings.NewReader(b))
	for {
		e, err := readSSEEvent(reader)
		if err == io.EOF {
			return
		}
		if err != nil {
			fmt.Println("decode err:", err)
			return
		}
		fmt.Printf("event=%s data=%s\n", e.Event, e.Data)
	}
}

type sseEvent struct {
	Event string
	Data  string
}

func readSSEEvent(r *bufio.Reader) (sseEvent, error) {
	var e sseEvent
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return e, err
		}
		line = strings.TrimRight(line, "\n")
		if line == "" {
			if e.Event != "" || e.Data != "" {
				return e, nil
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			e.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if e.Data != "" {
				e.Data += "\n"
			}
			e.Data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
}

func TestResponsesSSEDataIsValidJSON(t *testing.T) {
	provider := &streamingStubProvider{
		streamEvents: []StreamEvent{
			{Chunk: &ChatCompletionChunk{Model: "m", Choices: []ChunkChoice{{Index: 0, Delta: Delta{Role: "assistant", Content: "hello"}}}}, Raw: nil},
			{Chunk: &ChatCompletionChunk{Model: "m", Choices: []ChunkChoice{{Index: 0, FinishReason: "stop"}}}},
		},
	}
	h := newStreamTestHandler(provider)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"m","stream":true,"input":"hi"}`))
	h.Responses(rec, req)

	r := bufio.NewReader(strings.NewReader(rec.Body.String()))
	for {
		e, err := readSSEEvent(r)
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if e.Data == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(e.Data), &raw); err != nil {
			t.Fatalf("event %q had invalid JSON data %q: %v", e.Event, e.Data, err)
		}
	}
}

// ctx context stub
var _ = context.TODO
