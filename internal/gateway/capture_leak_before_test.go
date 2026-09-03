package gateway

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/observability"
)

// D-2c.1 leak reproduction.
//
// docs/d2c-implementation-spec.md §7.1 requires a test per capture surface
// that asserts the runtime writes raw content even when capture is off. For
// every surface but this one, "off" is unrepresentable: there is no capture
// policy in this codebase yet, so a test cannot express the off state. This
// surface is the exception, and the reason is that it does not consult a
// policy at all.
//
// handler.go builds the trace with
//
//	// Capture input messages (opt-in content capture; on by default in dev).
//	if b, err := json.Marshal(req.Messages); err == nil {
//
// and there is no condition. Not a policy read, not an env check, not a
// dev-vs-prod branch — the comment describes a control that was never
// written. The same is true of every OutputMessages assignment. From there
// observability/clickhouse.go INSERTs both columns into gateway_traces,
// where the ClickHouse init migration declares them plain `String` and
// retains them `INTERVAL 90 DAY`.
//
// So these tests pass today by reproducing the leak, which is what makes
// them the audit trail the spec asks for. When D-2c.3 introduces the gate,
// they must be flipped deliberately (capture off ⇒ bodies absent) rather
// than quietly deleted, and git history keeps the proof that the gateway
// used to persist customer prompts unconditionally.
//
// Scope: these assert what reaches observability.Recorder, the interface
// whose own contract is "enqueues a trace for persistence". They do not
// need a live ClickHouse, because the leak is decided before the recorder
// is called, not inside it.

// captureLeakRecorder records what the handler hands to persistence.
type captureLeakRecorder struct{ traces []observability.Trace }

func (c *captureLeakRecorder) Record(t observability.Trace) { c.traces = append(c.traces, t) }
func (c *captureLeakRecorder) Close(context.Context) error  { return nil }

func (c *captureLeakRecorder) only(t *testing.T) observability.Trace {
	t.Helper()
	if len(c.traces) != 1 {
		t.Fatalf("recorder saw %d traces, want exactly 1", len(c.traces))
	}
	return c.traces[0]
}

// captureEchoProvider reflects the prompt back in the completion so one
// request demonstrates both directions of the leak with one marker.
type captureEchoProvider struct{}

func (captureEchoProvider) Name() string     { return "capture-echo" }
func (captureEchoProvider) Models() []string { return []string{"capture-echo-model"} }

func (captureEchoProvider) ChatCompletion(_ context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	var echo string
	if len(req.Messages) > 0 {
		echo = req.Messages[len(req.Messages)-1].Content
	}
	return &ChatCompletionResponse{
		Model:   req.Model,
		Choices: []Choice{{Message: Message{Role: "assistant", Content: "echo: " + echo}}},
		Usage:   Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}, nil
}

func (captureEchoProvider) ChatCompletionStream(_ context.Context, _ ChatCompletionRequest) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, 1)
	ch <- StreamEvent{Done: true}
	close(ch)
	return ch, nil
}

// captureCanary is deliberately unmistakable: if it turns up in a trace, a
// dump, or a vendor payload, it came from a request body and nothing asked
// permission to keep it.
const captureCanary = "CANARY-PROMPT-8f21c7-user-supplied-secret"

func newCaptureLeakHandler(rec observability.Recorder) *Handler {
	reg := NewRegistry()
	reg.Register(captureEchoProvider{})
	return NewHandler(reg, rec, nil, slog.Default())
}

// TestCaptureTrace_LeakBefore_InputMessages: the user's prompt reaches the
// persistence recorder verbatim, with no capture policy consulted.
func TestCaptureTrace_LeakBefore_InputMessages(t *testing.T) {
	rec := &captureLeakRecorder{}
	h := newCaptureLeakHandler(rec)

	resp := doChat(h, `{"model":"capture-echo-model","messages":[{"role":"user","content":"`+captureCanary+`"}]}`)
	if resp.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", resp.Code, resp.Body.String())
	}

	tr := rec.only(t)
	if !strings.Contains(tr.InputMessages, captureCanary) {
		t.Fatalf("LEAK REPRODUCTION FAILED: InputMessages no longer carries the prompt.\n"+
			"This test exists to prove the gateway persists raw prompts with no policy gate.\n"+
			"If D-2c.3 landed the capture gate, invert this test rather than deleting it.\n"+
			"got InputMessages = %q", tr.InputMessages)
	}
}

// TestCaptureTrace_LeakBefore_OutputMessages: the model's completion reaches
// the persistence recorder verbatim, same absence of a gate.
func TestCaptureTrace_LeakBefore_OutputMessages(t *testing.T) {
	rec := &captureLeakRecorder{}
	h := newCaptureLeakHandler(rec)

	resp := doChat(h, `{"model":"capture-echo-model","messages":[{"role":"user","content":"`+captureCanary+`"}]}`)
	if resp.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", resp.Code, resp.Body.String())
	}

	tr := rec.only(t)
	if !strings.Contains(tr.OutputMessages, captureCanary) {
		t.Fatalf("LEAK REPRODUCTION FAILED: OutputMessages no longer carries the completion.\n"+
			"See TestCaptureTrace_LeakBefore_InputMessages for why this is an assertion.\n"+
			"got OutputMessages = %q", tr.OutputMessages)
	}
}

// TestCaptureTrace_LeakBefore_NoPolicySeam names the gap directly rather than
// only demonstrating its effect. Metadata the spec classifies as safe is
// present, which is what makes the bodies alongside it a policy question and
// not an all-or-nothing one: a deployment that wants usage accounting without
// content retention has no way to ask for it today.
func TestCaptureTrace_LeakBefore_NoPolicySeam(t *testing.T) {
	rec := &captureLeakRecorder{}
	h := newCaptureLeakHandler(rec)

	resp := doChat(h, `{"model":"capture-echo-model","messages":[{"role":"user","content":"`+captureCanary+`"}]}`)
	if resp.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", resp.Code, resp.Body.String())
	}

	tr := rec.only(t)
	if tr.RequestModel == "" {
		t.Fatal("RequestModel empty; metadata is expected to be populated regardless of capture policy")
	}
	if tr.InputMessages == "" || tr.OutputMessages == "" {
		t.Fatal("bodies empty: a capture seam appears to exist now, so D-2c.3 has landed and these tests must be inverted")
	}
}
