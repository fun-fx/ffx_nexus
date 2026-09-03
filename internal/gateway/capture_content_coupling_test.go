package gateway

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/observability"
)

// Capture coupling: the handler must keep putting bodies on the Trace.
//
// This file began as the D-2c.1 leak reproduction. It asserted that the
// gateway hands raw prompts and completions to the persistence recorder with
// no policy consulted — handler.go's "opt-in content capture" comment
// described a control that had never been written, and ClickHouse retained
// both columns for the full trace window as a result.
//
// D-2c.3 closed that, and closed it one layer lower than §7.1 anticipated.
// The gate is not in the handler. It is observability.CaptureGate, wrapping
// the ClickHouse recorder inside the fan-out that cmd/nexus assembles, and
// the reason it sits there is that the in-process evaluators read the bodies
// off the same Trace. A gate in the handler would have secured storage by
// disabling scoring.
//
// So these assertions did not invert — their meaning did. What used to be
// "we leak here" is now "the bodies must still reach the recorder chain,
// because the eval branch of that chain needs them." They are the coupling
// half of the contract; internal/observability/capture_gate_test.go and
// cmd/nexus/compose_capture_test.go are the retention half. Break either
// half and the pair stops describing anything useful:
//
//   - if these fail, evaluation is silently unscored
//   - if the retention tests fail, bodies are being persisted
//
// Git history holds the original leak reproduction, which is the audit
// trail §7.2 asked to preserve.

// captureLeakRecorder records what the handler hands to the recorder chain.
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
// request covers both directions with one marker.
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

// captureCanary is deliberately unmistakable: if it turns up somewhere the
// gate was supposed to cover — a persisted row, a dump, a vendor payload —
// it came from a request body and can be traced back to this test.
const captureCanary = "CANARY-PROMPT-8f21c7-user-supplied-secret"

func newCaptureLeakHandler(rec observability.Recorder) *Handler {
	reg := NewRegistry()
	reg.Register(captureEchoProvider{})
	return NewHandler(reg, rec, nil, slog.Default())
}

// TestCaptureTrace_InputReachesRecorderChain: the user's prompt is on the
// Trace the handler emits. The gate downstream decides who keeps it; the
// handler's job is to make sure there is something for the evaluators to
// read.
func TestCaptureTrace_InputReachesRecorderChain(t *testing.T) {
	rec := &captureLeakRecorder{}
	h := newCaptureLeakHandler(rec)

	resp := doChat(h, `{"model":"capture-echo-model","messages":[{"role":"user","content":"`+captureCanary+`"}]}`)
	if resp.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", resp.Code, resp.Body.String())
	}

	tr := rec.only(t)
	if !strings.Contains(tr.InputMessages, captureCanary) {
		t.Fatalf("InputMessages no longer carries the prompt.\n"+
			"The evaluators read the prompt off this field, so if content capture was "+
			"moved into the handler, scoring is now silently disabled for every "+
			"deployment. The gate belongs in cmd/nexus's traceFanout, on the "+
			"recorders that retain.\ngot InputMessages = %q", tr.InputMessages)
	}
}

// TestCaptureTrace_OutputReachesRecorderChain: the model's completion is on
// the Trace too, for the same reason.
func TestCaptureTrace_OutputReachesRecorderChain(t *testing.T) {
	rec := &captureLeakRecorder{}
	h := newCaptureLeakHandler(rec)

	resp := doChat(h, `{"model":"capture-echo-model","messages":[{"role":"user","content":"`+captureCanary+`"}]}`)
	if resp.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", resp.Code, resp.Body.String())
	}

	tr := rec.only(t)
	if !strings.Contains(tr.OutputMessages, captureCanary) {
		t.Fatalf("OutputMessages no longer carries the completion.\n"+
			"See TestCaptureTrace_InputReachesRecorderChain for why this is an assertion.\n"+
			"got OutputMessages = %q", tr.OutputMessages)
	}
}

// TestCaptureTrace_MetadataAccompaniesBodies records what made the split
// possible in the first place. Metadata and bodies arrive on the same Trace
// but answer to different policies: a deployment that declines content
// retention has not declined usage accounting, and the gate is what lets it
// have the second without the first. This asserts both are present at the
// handler boundary, which is the input the gate separates.
func TestCaptureTrace_MetadataAccompaniesBodies(t *testing.T) {
	rec := &captureLeakRecorder{}
	h := newCaptureLeakHandler(rec)

	resp := doChat(h, `{"model":"capture-echo-model","messages":[{"role":"user","content":"`+captureCanary+`"}]}`)
	if resp.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", resp.Code, resp.Body.String())
	}

	tr := rec.only(t)
	if tr.RequestModel == "" {
		t.Fatal("RequestModel empty; metadata must be populated regardless of capture policy, or cost and usage reporting breaks when capture is off")
	}
	if tr.InputMessages == "" || tr.OutputMessages == "" {
		t.Fatal("bodies empty at the handler boundary: content capture appears to have been moved upstream of the fan-out, which disables the evaluators")
	}
}
