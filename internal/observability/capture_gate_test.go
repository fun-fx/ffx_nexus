package observability

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

type gateSpy struct{ got []Trace }

func (s *gateSpy) Record(t Trace)              { s.got = append(s.got, t) }
func (s *gateSpy) Close(context.Context) error { return nil }

func (s *gateSpy) only(t *testing.T) Trace {
	t.Helper()
	if len(s.got) != 1 {
		t.Fatalf("wrapped recorder saw %d traces, want 1", len(s.got))
	}
	return s.got[0]
}

const gateCanary = "CANARY-GATE-5b04e9-content-must-not-persist"

func gateTrace() Trace {
	return Trace{
		TraceID:           "t1",
		SpanID:            "s1",
		OrgID:             "org-1",
		UserID:            "user-1",
		OperationName:     "chat.completions",
		ProviderName:      "openai",
		RequestModel:      "gpt-4o-mini",
		ResponseModel:     "gpt-4o-mini",
		InputTokens:       11,
		OutputTokens:      22,
		CostUSD:           0.0042,
		LatencyMs:         123,
		StatusCode:        200,
		SessionID:         "sess-1",
		TurnID:            "turn-1",
		InputMessages:     `[{"role":"user","content":"` + gateCanary + `"}]`,
		OutputMessages:    gateCanary,
		RetrievalContexts: `["` + gateCanary + `"]`,
		EvalReference:     "the-expected-answer",
	}
}

// TestCaptureGate_StripsContentWhenDisabled: with capture off, no part of the
// request, response, or retrieval material reaches the wrapped recorder.
func TestCaptureGate_StripsContentWhenDisabled(t *testing.T) {
	spy := &gateSpy{}
	NewCaptureGate(spy, false).Record(gateTrace())

	got := spy.only(t)
	for name, v := range map[string]string{
		"InputMessages":     got.InputMessages,
		"OutputMessages":    got.OutputMessages,
		"RetrievalContexts": got.RetrievalContexts,
	} {
		if v != "" {
			t.Errorf("%s = %q, want empty: capture is off, so this field must not reach a recorder that retains", name, v)
		}
	}
}

// TestCaptureGate_PreservesMetadataWhenDisabled: stripping content must not
// turn the gate into a mute. Usage accounting, routing stats, and the cost
// dashboard all read these columns, and a deployment that declines content
// retention has not declined billing.
func TestCaptureGate_PreservesMetadataWhenDisabled(t *testing.T) {
	spy := &gateSpy{}
	in := gateTrace()
	NewCaptureGate(spy, false).Record(in)
	got := spy.only(t)

	// Everything except the stripped fields must survive byte for byte.
	want := in
	want.InputMessages = ""
	want.OutputMessages = ""
	want.RetrievalContexts = ""
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gate altered more than the content fields.\n got = %+v\nwant = %+v", got, want)
	}
}

// TestCaptureGate_PassesThroughWhenEnabled: an operator who opts in gets the
// unmodified trace, and gets it without an extra indirection — the gate
// returns inner itself so the enabled path costs nothing.
func TestCaptureGate_PassesThroughWhenEnabled(t *testing.T) {
	spy := &gateSpy{}
	rec := NewCaptureGate(spy, true)

	if rec != Recorder(spy) {
		t.Fatalf("enabled gate returned a wrapper (%T); it should return inner unchanged", rec)
	}
	rec.Record(gateTrace())
	if got := spy.only(t); !strings.Contains(got.InputMessages, gateCanary) {
		t.Fatalf("capture is on but the body did not reach the recorder: %q", got.InputMessages)
	}
}

// TestCaptureGate_NilInner: nil in, nil out, so a caller can pass an
// unconfigured recorder straight into a list that skips nil entries.
func TestCaptureGate_NilInner(t *testing.T) {
	if got := NewCaptureGate(nil, false); got != nil {
		t.Fatalf("NewCaptureGate(nil, false) = %v, want nil", got)
	}
	if got := NewCaptureGate(nil, true); got != nil {
		t.Fatalf("NewCaptureGate(nil, true) = %v, want nil", got)
	}
}

// TestCaptureGate_CallerCopyUnaffected pins the property the whole design
// rests on. The gate blanks fields on its own copy of the Trace; if Trace
// ever grew a pointer or slice field holding content, blanking would reach
// through into the caller's value and the eval worker downstream would lose
// the bodies it needs. This test fails the moment that becomes possible.
func TestCaptureGate_CallerCopyUnaffected(t *testing.T) {
	spy := &gateSpy{}
	mine := gateTrace()
	NewCaptureGate(spy, false).Record(mine)

	if !strings.Contains(mine.InputMessages, gateCanary) {
		t.Fatal("the gate reached back into the caller's Trace; recorders no longer hold independent copies, so gating one branch now starves the others")
	}
	if !strings.Contains(mine.OutputMessages, gateCanary) {
		t.Fatal("caller's OutputMessages was cleared by the gate")
	}
}

// TestCaptureGate_FanoutIsolation is the end-to-end statement of the design:
// one Trace, delivered through the same MultiRecorder, arrives stripped at
// the branch that retains and intact at the branch that scores.
//
// cmd/nexus asserts the fan-out is wired this way. This asserts that being
// wired that way actually produces the two outcomes, which is the claim the
// wiring is worth making.
func TestCaptureGate_FanoutIsolation(t *testing.T) {
	durable, inProcess := &gateSpy{}, &gateSpy{}
	multi := NewMultiRecorder(NewCaptureGate(durable, false), inProcess)

	multi.Record(gateTrace())

	if got := durable.only(t).InputMessages; got != "" {
		t.Errorf("the retaining branch received a body: %q", got)
	}
	if got := inProcess.only(t).InputMessages; !strings.Contains(got, gateCanary) {
		t.Errorf("the eval branch lost the body it needs to score: %q", got)
	}
	// Both branches must still agree on who the trace belongs to, or the
	// score the eval branch produces cannot be joined to the row the
	// durable branch wrote.
	if d, p := durable.got[0].TraceID, inProcess.got[0].TraceID; d != p {
		t.Errorf("TraceID diverged across the fan-out: durable %q, in-process %q", d, p)
	}
}

// TestCaptureGate_ContentFieldCountMatchesTrace is a drift guard. Trace grows
// over time; when it grows a field carrying customer text, the gate must
// learn about it. Counting the fields the gate clears against a declared
// constant means adding a content field without touching the gate fails here
// instead of shipping that field to ClickHouse for ninety days.
func TestCaptureGate_ContentFieldCountMatchesTrace(t *testing.T) {
	spy := &gateSpy{}
	in := gateTrace()
	NewCaptureGate(spy, false).Record(in)
	got := spy.only(t)

	cleared := 0
	rin, rgot := reflect.ValueOf(in), reflect.ValueOf(got)
	for i := 0; i < rin.NumField(); i++ {
		if rin.Field(i).Kind() != reflect.String {
			continue
		}
		if rin.Field(i).String() != "" && rgot.Field(i).String() == "" {
			cleared++
		}
	}
	if cleared != contentFieldCount {
		t.Fatalf("gate cleared %d string fields, contentFieldCount says %d.\n"+
			"If a raw-content field was added to Trace, strip it in CaptureGate.Record and bump the constant.\n"+
			"If a field was reclassified as metadata, say why in the Record comment and bump the constant.",
			cleared, contentFieldCount)
	}
}
