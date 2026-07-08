package evals

import (
	"testing"

	"github.com/ffxnexus/nexus/internal/observability"
)

func TestTraceModel(t *testing.T) {
	if got := traceModel(observability.Trace{ResponseModel: "gpt-4o", RequestModel: "auto"}); got != "gpt-4o" {
		t.Fatalf("response model preferred, got %q", got)
	}
	if got := traceModel(observability.Trace{RequestModel: "gemini-2.5-flash"}); got != "gemini-2.5-flash" {
		t.Fatalf("request model fallback, got %q", got)
	}
}
