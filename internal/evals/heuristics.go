package evals

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/ffxnexus/nexus/internal/observability"
)

// PII detection patterns. Deliberately conservative to limit false positives;
// these are signals, not a compliance-grade DLP engine.
var (
	reEmail = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
	rePhone = regexp.MustCompile(`\b(?:\+?\d{1,3}[\s.\-]?)?(?:\(?\d{3}\)?[\s.\-]?)\d{3}[\s.\-]?\d{4}\b`)
	reSSN   = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	reCard  = regexp.MustCompile(`\b(?:\d[ \-]?){13,16}\b`)
)

// PIIEvaluator flags personally identifiable information leaked in the model
// output. It is cheap (regex only) and runs on every sampled trace.
type PIIEvaluator struct{}

// Name implements Evaluator.
func (PIIEvaluator) Name() string { return "heuristic_pii" }

// Evaluate implements Evaluator. Score 1.0 = clean, 0.0 = PII detected.
func (PIIEvaluator) Evaluate(_ context.Context, t observability.Trace) ([]Score, error) {
	text := t.OutputMessages
	var hits []string
	if reEmail.MatchString(text) {
		hits = append(hits, "email")
	}
	if reSSN.MatchString(text) {
		hits = append(hits, "ssn")
	}
	if rePhone.MatchString(text) {
		hits = append(hits, "phone")
	}
	if reCard.MatchString(text) {
		hits = append(hits, "card")
	}

	passed := len(hits) == 0
	score := 1.0
	rationale := "no PII patterns detected"
	if !passed {
		score = 0.0
		rationale = fmt.Sprintf("detected PII patterns: %v", hits)
	}
	return []Score{{
		TraceID:   t.TraceID,
		Timestamp: time.Now().UTC(),
		Evaluator: "heuristic_pii",
		Metric:    "pii_leak",
		Score:     score,
		Passed:    passed,
		Rationale: rationale,
	}}, nil
}

// CompletenessEvaluator flags empty or truncated responses. Truncation is
// inferred from finish_reason == "length".
type CompletenessEvaluator struct{}

// Name implements Evaluator.
func (CompletenessEvaluator) Name() string { return "heuristic_completeness" }

// Evaluate implements Evaluator.
func (CompletenessEvaluator) Evaluate(_ context.Context, t observability.Trace) ([]Score, error) {
	score := 1.0
	passed := true
	rationale := "response present and not truncated"

	switch {
	case len(t.OutputMessages) == 0 && t.StatusCode == 200:
		score, passed, rationale = 0.0, false, "empty response body"
	case t.FinishReason == "length":
		score, passed, rationale = 0.5, false, "response truncated (finish_reason=length)"
	}

	return []Score{{
		TraceID:   t.TraceID,
		Timestamp: time.Now().UTC(),
		Evaluator: "heuristic_completeness",
		Metric:    "completeness",
		Score:     score,
		Passed:    passed,
		Rationale: rationale,
	}}, nil
}
