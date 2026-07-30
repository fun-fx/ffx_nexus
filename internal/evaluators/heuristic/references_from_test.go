package heuristic

import (
	"context"
	"testing"

	"github.com/ffxnexus/nexus/internal/observability"
)

func TestReferenceFor_DefaultsToEvalReference(t *testing.T) {
	tr := observability.Trace{
		TraceID:       "t",
		EvalReference: "ground-truth",
	}
	got := referenceFor(nil, tr)
	if got != "ground-truth" {
		t.Errorf("default path returned %q, want ground-truth", got)
	}
}

func TestReferenceFor_TraceEvalReferencePath(t *testing.T) {
	tr := observability.Trace{
		TraceID:        "t",
		OutputMessages: "model output",
		EvalReference:  "expected",
	}
	got := referenceFor(map[string]any{"references_from": "trace.eval_reference"}, tr)
	if got != "expected" {
		t.Errorf("trace.eval_reference path returned %q, want expected", got)
	}
}

func TestReferenceFor_TraceMetadataKey(t *testing.T) {
	tr := observability.Trace{
		TraceID:      "t",
		EvalMetadata: map[string]any{"ground_truth": "from metadata"},
	}
	got := referenceFor(map[string]any{"references_from": "trace.metadata.ground_truth"}, tr)
	if got != "from metadata" {
		t.Errorf("metadata path returned %q, want from metadata", got)
	}
}

func TestReferenceFor_TraceOutputMessages(t *testing.T) {
	tr := observability.Trace{
		TraceID:        "t",
		OutputMessages: "model reply",
	}
	got := referenceFor(map[string]any{"references_from": "trace.output_messages"}, tr)
	if got != "model reply" {
		t.Errorf("output path returned %q, want model reply", got)
	}
}

func TestReferenceFor_LiteralValueOverride(t *testing.T) {
	tr := observability.Trace{TraceID: "t"}
	got := referenceFor(map[string]any{"references_from": "literal-ref"}, tr)
	if got != "literal-ref" {
		t.Errorf("literal path returned %q, want literal-ref", got)
	}
}

func TestRougeL_UsesReferencesFrom(t *testing.T) {
	tr := observability.Trace{
		TraceID:        "t",
		OutputMessages: "the quick brown fox",
		EvalMetadata:   map[string]any{"answer": "the quick brown fox jumps"},
	}
	args := map[string]any{"references_from": "trace.metadata.answer"}
	sc, err := evalRougeL(context.Background(), args, tr)
	if err != nil {
		t.Fatalf("evalRougeL: %v", err)
	}
	if len(sc) != 1 {
		t.Fatalf("expected 1 score, got %d", len(sc))
	}
	if sc[0].Score <= 0.0 {
		t.Errorf("expected positive rouge_l, got %v", sc[0].Score)
	}
}

func TestExactMatch_UsesReferencesFrom(t *testing.T) {
	tr := observability.Trace{
		TraceID:        "t",
		OutputMessages: "hello",
		EvalMetadata:   map[string]any{"answer": "hello"},
	}
	args := map[string]any{"references_from": "trace.metadata.answer"}
	sc, err := evalExactMatch(context.Background(), args, tr)
	if err != nil {
		t.Fatalf("evalExactMatch: %v", err)
	}
	if !sc[0].Passed {
		t.Errorf("expected pass, got fail: %+v", sc[0])
	}
}
