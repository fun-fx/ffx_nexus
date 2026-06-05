package evals

import "testing"

func TestExtractPromptLastUser(t *testing.T) {
	in := `[{"role":"system","content":"You are helpful"},{"role":"user","content":"What is RAG?"}]`
	if got := extractPrompt(in); got != "What is RAG?" {
		t.Fatalf("got %q", got)
	}
}

func TestParseRetrievalContexts(t *testing.T) {
	got := parseRetrievalContexts(`["doc a","doc b"]`)
	if len(got) != 2 || got[0] != "doc a" {
		t.Fatalf("unexpected: %v", got)
	}
	if parseRetrievalContexts("") != nil {
		t.Fatal("empty should be nil")
	}
}

func TestMergeContextMetrics(t *testing.T) {
	base := []string{"answer_relevancy", "toxicity"}
	got := mergeContextMetrics(base, []string{"chunk"})
	if !containsMetric(got, "hallucination") || !containsMetric(got, "ragas_faithfulness") {
		t.Fatalf("expected context metrics appended: %v", got)
	}
	if len(mergeContextMetrics(base, nil)) != 2 {
		t.Fatal("no contexts should not append")
	}
}
