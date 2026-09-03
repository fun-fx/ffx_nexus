package observability

import (
	"encoding/json"
	"strings"
	"testing"
)

// D-2c.1 boundary defence for the first-party OTLP exporter.
//
// otel.go documents this contract on OTLPEnvelope:
//
//	// Adapters use it to carry prompt/completion content, which this
//	// envelope deliberately omits: the first-party exporter ships only
//	// metadata, so anything sensitive has to be opted in by the caller
//	// after it has passed the plugin's redaction step.
//
// The omission is real, and it is the one content boundary in this
// codebase that was decided correctly. Nothing asserted it. A refactor
// that mapped InputMessages onto a span attribute — the obvious thing to
// reach for when a dashboard wants prompt text — would have shipped
// customer prompts to every configured OTLP receiver with a green test
// suite, which is how the sibling defect in the ClickHouse write path
// survived (see internal/gateway/capture_leak_before_test.go).
//
// Both directions are pinned, because the contract is not "never send
// bodies" but "send them only when the caller asks". A test that only
// checked absence would be satisfied by an envelope that had quietly lost
// the opt-in path too.

const boundaryCanary = "CANARY-BODY-3d90ab-must-not-egress-by-default"

func boundaryTrace() Trace {
	return Trace{
		TraceID:        "t-boundary",
		SpanID:         "s-boundary",
		OperationName:  "chat.completions",
		RequestModel:   "gpt-4o-mini",
		ProviderName:   "openai",
		StatusCode:     200,
		InputMessages:  `[{"role":"user","content":"` + boundaryCanary + `"}]`,
		OutputMessages: boundaryCanary,
	}
}

func renderEnvelope(t *testing.T, extra map[string]string) string {
	t.Helper()
	b, err := json.Marshal(OTLPEnvelope([]Trace{boundaryTrace()}, extra))
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(b)
}

// TestOTLPEnvelope_OmitsBodiesByDefault: with no caller opt-in, no part of
// the request or response body appears anywhere in the envelope.
func TestOTLPEnvelope_OmitsBodiesByDefault(t *testing.T) {
	out := renderEnvelope(t, nil)

	if strings.Contains(out, boundaryCanary) {
		t.Fatalf("the first-party OTLP envelope now carries request/response content.\n"+
			"otel.go promises it ships metadata only and that bodies are the caller's\n"+
			"explicit opt-in via extraAttributes. If exporting bodies by default is\n"+
			"intended, that is a capture-policy decision (D-2c.3), not an exporter\n"+
			"change, and this test is the thing that is supposed to stop it.\n"+
			"envelope = %s", out)
	}

	// Metadata must survive: this exporter is still expected to be useful.
	for _, want := range []string{"gpt-4o-mini", "openai", "chat.completions"} {
		if !strings.Contains(out, want) {
			t.Fatalf("envelope lost metadata %q; omitting bodies must not mean omitting everything\nenvelope = %s", want, out)
		}
	}
}

// TestOTLPEnvelope_CarriesBodiesOnCallerOptIn: the documented opt-in path
// still works, so vendor adapters that legitimately forward content (after
// their own redaction step) are not silently broken by the assertion above.
func TestOTLPEnvelope_CarriesBodiesOnCallerOptIn(t *testing.T) {
	out := renderEnvelope(t, map[string]string{"gen_ai.input.messages": boundaryCanary})

	if !strings.Contains(out, boundaryCanary) {
		t.Fatalf("extraAttributes no longer reach the envelope; the opt-in path that\n"+
			"cmd/nexus/langfuse.go depends on is gone, which would make body export\n"+
			"impossible rather than deliberate.\nenvelope = %s", out)
	}
}

// TestOTLPEnvelope_OptInIsPerCallNotSticky guards the shape of the seam: the
// opt-in must be an argument, not exporter state. If a previous call could
// leave bodies enabled for later ones, a single adapter forwarding content
// would turn every unrelated export into a leak.
func TestOTLPEnvelope_OptInIsPerCallNotSticky(t *testing.T) {
	if got := renderEnvelope(t, map[string]string{"gen_ai.input.messages": boundaryCanary}); !strings.Contains(got, boundaryCanary) {
		t.Fatalf("opt-in call did not carry the body; envelope = %s", got)
	}
	if got := renderEnvelope(t, nil); strings.Contains(got, boundaryCanary) {
		t.Fatalf("a body opted into by an earlier call reappeared in a later call with no\n"+
			"extraAttributes, so the exporter is holding content across calls.\nenvelope = %s", got)
	}
}
