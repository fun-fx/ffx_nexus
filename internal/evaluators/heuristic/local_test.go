package heuristic

import (
	"context"
	"testing"

	"github.com/ffxnexus/nexus/internal/observability"
)

func sampleTrace() observability.Trace {
	return observability.Trace{TraceID: "trace-test"}
}

func TestContainsAllMode(t *testing.T) {
	tr := sampleTrace()
	tr.OutputMessages = "Order #4152 shipped to billing@acme.com"
	out, err := Evaluate(context.Background(), "contains",
		map[string]any{
			"needles":   []string{"acme.com", "4152"},
			"reference": "",
			"all":       true,
		}, tr)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].Score != 1.0 || !out[0].Passed {
		t.Errorf("expected pass: %+v", out)
	}
}

func TestContainsMiss(t *testing.T) {
	tr := sampleTrace()
	tr.OutputMessages = "order received"
	out, err := Evaluate(context.Background(), "contains",
		map[string]any{"needles": []string{"missing"}}, tr)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if out[0].Score != 0.0 || out[0].Passed {
		t.Errorf("needle miss should report failure: %+v", out)
	}
}

func TestContainsAnyMode(t *testing.T) {
	tr := sampleTrace()
	tr.OutputMessages = "billing@acme.com"
	out, err := Evaluate(context.Background(), "contains",
		map[string]any{"needles": []string{"acme.com", "footer"}, "all": false}, tr)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !out[0].Passed {
		t.Errorf("any-match should pass with one needle hit: %+v", out)
	}
}

func TestPIIDetectsSSN(t *testing.T) {
	tr := sampleTrace()
	tr.InputMessages = "forwarding ssn 123-45-6789"
	out, err := Evaluate(context.Background(), "pii", nil, tr)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if out[0].Passed || out[0].Score != 0 {
		t.Errorf("SSN should fail: %+v", out)
	}
}

func TestPIIClear(t *testing.T) {
	tr := sampleTrace()
	tr.InputMessages = "tell me about rust async"
	tr.OutputMessages = "Rust async is a great topic."
	out, err := Evaluate(context.Background(), "pii", nil, tr)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !out[0].Passed {
		t.Errorf("clean trace should pass: %+v", out)
	}
}

func TestExactMatchPass(t *testing.T) {
	tr := sampleTrace()
	tr.OutputMessages = "yes"
	out, err := Evaluate(context.Background(), "exact_match",
		map[string]any{"reference": "yes"}, tr)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !out[0].Passed {
		t.Errorf("identical should pass: %+v", out)
	}
}

func TestExactMatchFail(t *testing.T) {
	tr := sampleTrace()
	tr.OutputMessages = "no"
	out, err := Evaluate(context.Background(), "exact_match",
		map[string]any{"reference": "yes"}, tr)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if out[0].Passed {
		t.Errorf("different should fail: %+v", out)
	}
}

func TestRougeLIdenticalGivesOne(t *testing.T) {
	tr := sampleTrace()
	out, err := Evaluate(context.Background(), "rouge_l",
		map[string]any{"prediction": "hello world", "reference": "hello world"}, tr)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if out[0].Score != 1.0 {
		t.Errorf("identical should give F=1.0: %+v", out)
	}
}

func TestRougeLDisjointGivesZero(t *testing.T) {
	tr := sampleTrace()
	out, err := Evaluate(context.Background(), "rouge_l",
		map[string]any{"prediction": "abc", "reference": "xyz"}, tr)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if out[0].Score != 0.0 {
		t.Errorf("disjoint should give F=0.0: %+v", out)
	}
}

func TestRougeLPartialMonotonicF(t *testing.T) {
	tr := sampleTrace()
	short, err := Evaluate(context.Background(), "rouge_l",
		map[string]any{"prediction": "the cat", "reference": "the cat sat on the mat"}, tr)
	if err != nil {
		t.Fatalf("short: %v", err)
	}
	full, err := Evaluate(context.Background(), "rouge_l",
		map[string]any{"prediction": "the cat sat on the mat", "reference": "the cat sat on the mat"}, tr)
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	if full[0].Score <= short[0].Score {
		t.Errorf("longer LCS should not lower F: %v vs %v", short[0].Score, full[0].Score)
	}
}

func TestEvaluateUnknownKind(t *testing.T) {
	_, err := Evaluate(context.Background(), "kaito", nil, sampleTrace())
	if err == nil {
		t.Errorf("expected error for unknown metric")
	}
}

func TestContainsMissingArgs(t *testing.T) {
	_, err := Evaluate(context.Background(), "contains", map[string]any{}, sampleTrace())
	if err == nil {
		t.Errorf("missing needles should fail loudly")
	}
}

func TestRedactPIISubstitutes(t *testing.T) {
	tr := observability.Trace{
		TraceID:        "trace-redact",
		InputMessages:  "my email is foo@example.com",
		OutputMessages: "ssn 123-45-6789 ok",
	}
	out := RedactPII(tr)
	if out.InputMessages == tr.InputMessages {
		t.Errorf("email not redacted: %q", out.InputMessages)
	}
	if out.OutputMessages == tr.OutputMessages {
		t.Errorf("ssn not redacted: %q", out.OutputMessages)
	}
}

func TestInspectPIIReturnsHits(t *testing.T) {
	tr := observability.Trace{
		InputMessages: "ssn 123-45-6789",
	}
	r := InspectPII(tr)
	if !r.HasPII || len(r.Hits) == 0 {
		t.Errorf("expected PII hits: %+v", r)
	}
}
