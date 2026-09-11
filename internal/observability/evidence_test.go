package observability

import "testing"

func TestMarshalJSONFieldEmpty(t *testing.T) {
	if got := MarshalJSONField([]ProviderAttempt(nil)); got != "" {
		t.Fatalf("nil slice: got %q", got)
	}
	if got := MarshalJSONField([]ProviderAttempt{}); got != "" {
		t.Fatalf("empty slice: got %q", got)
	}
}

func TestGuardrailRuleFromAction(t *testing.T) {
	if got := GuardrailRuleFromAction("input_blocked:pii_input"); got != "pii_input" {
		t.Fatalf("got %q", got)
	}
	if got := GuardrailRuleFromAction("output_redacted"); got != "" {
		t.Fatalf("no rule: got %q", got)
	}
}

func TestUnmarshalRoundTrip(t *testing.T) {
	in := []PolicyReason{{Code: ReasonFallback, RuleID: "chain", Detail: "openai→anthropic"}}
	raw := MarshalJSONField(in)
	var out []PolicyReason
	UnmarshalJSONField(raw, &out)
	if len(out) != 1 || out[0].Code != ReasonFallback {
		t.Fatalf("round trip: %+v", out)
	}
}
