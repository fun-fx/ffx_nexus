package evals

import (
	"context"
	"testing"

	"github.com/ffxnexus/nexus/internal/observability"
)

func TestPIIEvaluator(t *testing.T) {
	pii := PIIEvaluator{}
	ctx := context.Background()

	clean, _ := pii.Evaluate(ctx, observability.Trace{OutputMessages: "The weather is sunny today."})
	if !clean[0].Passed || clean[0].Score != 1.0 {
		t.Fatalf("clean text should pass, got %+v", clean[0])
	}

	dirty, _ := pii.Evaluate(ctx, observability.Trace{OutputMessages: "Contact me at john.doe@example.com"})
	if dirty[0].Passed || dirty[0].Score != 0.0 {
		t.Fatalf("email should fail PII, got %+v", dirty[0])
	}

	ssn, _ := pii.Evaluate(ctx, observability.Trace{OutputMessages: "SSN is 123-45-6789"})
	if ssn[0].Passed {
		t.Fatalf("ssn should fail PII, got %+v", ssn[0])
	}
}

func TestCompletenessEvaluator(t *testing.T) {
	c := CompletenessEvaluator{}
	ctx := context.Background()

	ok, _ := c.Evaluate(ctx, observability.Trace{OutputMessages: "Here is a full answer.", StatusCode: 200})
	if !ok[0].Passed {
		t.Fatalf("non-empty answer should pass, got %+v", ok[0])
	}

	empty, _ := c.Evaluate(ctx, observability.Trace{OutputMessages: "", StatusCode: 200})
	if empty[0].Passed || empty[0].Score != 0.0 {
		t.Fatalf("empty answer should fail, got %+v", empty[0])
	}

	trunc, _ := c.Evaluate(ctx, observability.Trace{OutputMessages: "partial", FinishReason: "length", StatusCode: 200})
	if trunc[0].Passed || trunc[0].Score != 0.5 {
		t.Fatalf("truncated answer should fail with 0.5, got %+v", trunc[0])
	}
}

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		in    string
		score float64
	}{
		{`{"score": 0.8, "rationale": "good"}`, 0.8},
		{"Sure! {\"score\": 0.4, \"rationale\": \"meh\"} done", 0.4},
		{"```json\n{\"score\": 1.0, \"rationale\": \"perfect\"}\n```", 1.0},
	}
	for _, c := range cases {
		v, err := parseVerdict(c.in)
		if err != nil {
			t.Fatalf("parse %q: %v", c.in, err)
		}
		if v.Score != c.score {
			t.Fatalf("want score %v, got %v (in=%q)", c.score, v.Score, c.in)
		}
	}

	if _, err := parseVerdict("no json here"); err == nil {
		t.Fatal("expected error for non-JSON content")
	}
}

func TestClamp01(t *testing.T) {
	if clamp01(-0.5) != 0 || clamp01(1.5) != 1 || clamp01(0.5) != 0.5 {
		t.Fatal("clamp01 out of range")
	}
}
