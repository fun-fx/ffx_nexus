// Passthrough correctness test.
//
// Goal: prove that parseOpenAISSEWithRaw emits wire bytes byte-for-byte
// (keeping provider-specific fields like reasoning_content alive) while
// still attaching a parsed Chunk for metrics.
package providers

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/gateway"
)

// sampleOpenAIChunk is a complete SSE event containing a non-standard field
// (reasoning_content) that OpenAI-strict clients expect as a plain string.
// The exact byte sequence must survive the passthrough round-trip.
const sampleOpenAIChunk = `data: {"id":"chatcmpl-test1","object":"chat.completion.chunk","created":1720000000,"model":"text-prime","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello ","reasoning_content":"Let me greet the user."},"finish_reason":null}]}

`

func TestParseOpenAISSEWithRaw_ByteExact(t *testing.T) {
	upstream := strings.NewReader(sampleOpenAIChunk)
	ch := make(chan gateway.StreamEvent, 2)
	go func() {
		defer close(ch)
		parseOpenAISSEWithRaw(upstream, ch)
	}()

	var got []gateway.StreamEvent
	for evt := range ch {
		got = append(got, evt)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one event, got %d", len(got))
	}
	evt := got[0]

	// Raw must be the exact upstream bytes, including trailing "\n\n".
	if !bytes.Equal(evt.Raw, []byte(sampleOpenAIChunk)) {
		t.Errorf("Raw mismatch\nwant: %q\ngot:  %q", sampleOpenAIChunk, string(evt.Raw))
	}

	// Done must NOT be set — stream termination is signalled by channel close.
	if evt.Done {
		t.Error("expected Done=false for a data event with Raw")
	}

	// Chunk must be parsed from the data line so downstream can still
	// extract metrics without re-marshalling.
	if evt.Chunk == nil {
		t.Fatal("expected parsed Chunk for metrics")
	}
	if len(evt.Chunk.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(evt.Chunk.Choices))
	}
	delta := evt.Chunk.Choices[0].Delta
	if delta.Content != "Hello " {
		t.Errorf("Chunk content mismatch: got %q", delta.Content)
	}
	// reasoning_content is deliberately NOT on our typed Delta struct, so
	// the parsed Chunk copy drops it — that is by design. The important
	// invariant is that Raw still carries it.
	if delta.Content == "" {
		t.Error("parsed Chunk lost Content")
	}
}

func TestParseOpenAISSEWithRaw_DoneTerminator(t *testing.T) {
	const doneEvent = "data: [DONE]\n\n"
	upstream := strings.NewReader(doneEvent + doneEvent) // two [DONE] events
	ch := make(chan gateway.StreamEvent, 4)
	go func() { defer close(ch); parseOpenAISSEWithRaw(upstream, ch) }()

	var got []gateway.StreamEvent
	for evt := range ch {
		got = append(got, evt)
	}
	// Two separate [DONE] events should both be emitted as Raw.
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d", len(got))
	}
	for i, evt := range got {
		if evt.Done {
			t.Errorf("event %d: Done should remain un-set in passthrough mode; Raw carries [DONE]", i)
		}
		if !bytes.Equal(evt.Raw, []byte(doneEvent)) {
			t.Errorf("event %d: Raw mismatch: got %q", i, string(evt.Raw))
		}
	}
}

func TestParseOpenAISSEWithRaw_CommentLines(t *testing.T) {
	const event = `: keepalive
: routing-id=abc
data: {"choices":[{"delta":{"content":"x"}}]}

`
	upstream := strings.NewReader(event)
	ch := make(chan gateway.StreamEvent, 2)
	go func() { defer close(ch); parseOpenAISSEWithRaw(upstream, ch) }()

	var got []gateway.StreamEvent
	for evt := range ch {
		got = append(got, evt)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one event, got %d", len(got))
	}
	if !bytes.Equal(got[0].Raw, []byte(event)) {
		t.Errorf("Raw mismatch\nwant: %q\ngot:  %q", event, string(got[0].Raw))
	}
	if got[0].Chunk == nil {
		t.Error("expected parsed Chunk for the data line")
	}
	if got[0].Chunk.Choices[0].Delta.Content != "x" {
		t.Errorf("content mismatch: got %q", got[0].Chunk.Choices[0].Delta.Content)
	}
}

func TestParseOpenAISSEWithRaw_MultiDataLines(t *testing.T) {
	const event = "data: line1\ndata: line2\n\n"
	upstream := strings.NewReader(event)
	ch := make(chan gateway.StreamEvent, 2)
	go func() { defer close(ch); parseOpenAISSEWithRaw(upstream, ch) }()

	var got []gateway.StreamEvent
	for evt := range ch {
		got = append(got, evt)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one event, got %d", len(got))
	}
	if !bytes.Equal(got[0].Raw, []byte(event)) {
		t.Errorf("Raw mismatch\nwant: %q\ngot:  %q", event, string(got[0].Raw))
	}
	// Multi-line data is not a valid single JSON object, so parsing
	// should produce an error on the Chunk.
	if got[0].Err == nil {
		t.Error("expected Err for malformed multi-line data")
	}
}

func TestParseOpenAISSEWithRaw_ReasoningFieldSurvives(t *testing.T) {
	const event = `data: {"id":"cmpl-123","object":"chat.completion.chunk","created":1720000000,"model":"deepseek-r1","choices":[{"index":0,"delta":{"content":"The answer is","reasoning_content":"I need to think about this..."},"finish_reason":null}]}

`
	upstream := strings.NewReader(event)
	ch := make(chan gateway.StreamEvent, 2)
	go func() { defer close(ch); parseOpenAISSEWithRaw(upstream, ch) }()

	var got []gateway.StreamEvent
	for evt := range ch {
		got = append(got, evt)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one event, got %d", len(got))
	}

	raw := string(got[0].Raw)
	if !strings.Contains(raw, `"reasoning_content":"I need to think about this..."`) {
		t.Errorf("reasoning_content missing from Raw\ngot: %s", raw)
	}

	// The typed Chunk drops unknown fields by design — that is the whole
	// point of passthrough. Only Raw carries them.
	if got[0].Chunk == nil || got[0].Chunk.Choices[0].Delta.Content != "The answer is" {
		t.Error("typed Chunk should still hold known fields")
	}
}

func TestOpenAIClientPassthroughEnabled(t *testing.T) {
	grid := NewGrid("test-key", 30*time.Second)
	if !grid.StreamPassthrough {
		t.Error("NewGrid should set StreamPassthrough=true")
	}
	openai := NewOpenAI("test-key", "https://api.openai.com/v1", 30*time.Second)
	if openai.StreamPassthrough {
		t.Error("NewOpenAI should set StreamPassthrough=false by default")
	}
}
