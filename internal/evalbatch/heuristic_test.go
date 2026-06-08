package evalbatch

import (
	"context"
	"testing"
)

func TestHeuristicEvaluatorScoresCleanOutput(t *testing.T) {
	h := NewHeuristicEvaluator()
	c := Case{ID: "c1", Input: "hi", Output: "The capital of France is Paris."}

	scores, err := h.Evaluate(context.Background(), c.trace())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, s := range scores {
		got[s.Metric] = s.Score
	}
	if got["pii_leak"] != 1.0 {
		t.Fatalf("clean output should score pii_leak=1.0, got %v", got["pii_leak"])
	}
	if got["completeness"] != 1.0 {
		t.Fatalf("non-empty output should score completeness=1.0, got %v", got["completeness"])
	}
}

func TestHeuristicEvaluatorFlagsPII(t *testing.T) {
	h := NewHeuristicEvaluator()
	c := Case{ID: "c2", Input: "contact", Output: "Email me at jane.doe@example.com please."}

	scores, err := h.Evaluate(context.Background(), c.trace())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range scores {
		if s.Metric == "pii_leak" && s.Score != 0.0 {
			t.Fatalf("output with email should score pii_leak=0.0, got %v", s.Score)
		}
	}
}

func TestHeuristicEvaluatorDeterministic(t *testing.T) {
	h := NewHeuristicEvaluator()
	c := Case{ID: "c3", Input: "q", Output: "a stable answer"}

	first, _ := h.Evaluate(context.Background(), c.trace())
	second, _ := h.Evaluate(context.Background(), c.trace())
	if len(first) != len(second) {
		t.Fatalf("score count not stable: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Metric != second[i].Metric || first[i].Score != second[i].Score {
			t.Fatalf("scores not deterministic at %d: %+v vs %+v", i, first[i], second[i])
		}
	}
}
