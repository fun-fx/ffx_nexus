// Package heuristic ships the in-process metric evaluators that
// ServiceHeuristic plugins route to. It is the simplest possible
// "local backend" — no network round-trips, no subprocess, just
// regex / split / n-gram comparisons against trace payloads.
//
// Dispatchers that choose ServiceHeuristic route through this
// package's Evaluate entry point instead of the regular transmit
// pipeline. The score is returned synchronously so the worker's
// "collect" stage emits a single evals.Score per matching trace.
//
// The metric kinds here are the Go-native subset of HF Evaluate /
// LightEval / Ragas metric surface:
//
//   - contains — case-insensitive substring hit. Returns 1.0 if any
//     needle in Args["needles"] is present in the reference
//     (Args["reference"]) or trace output; 0.0 otherwise. Optional
//     Args["all"] = true requires every needle to match.
//   - pii — relies on the existing internal/evals redaction kind.
//     Returns 1.0 when the trace contains no PII spans, 0.0 when it
//     does. Args are currently unused.
//   - exact_match — whitespace-trimmed case-sensitive comparison of
//     Args["prediction"] vs Args["reference"], or trace.output vs
//     Args["reference"] when the manifest omits prediction.
//   - rouge_l — classic Lin (1979) LCS-based F-measure over the
//     reference vs prediction, with Args["beta"] defaulting to 1.0
//     (i.e., plain F1) and a fallback to the trace output.
//
// Each metric returns an evals.Score directly so collectors don't
// have to do any post-processing; the registry path is identical
// whether the score came from a vendor webhook or from a Go regex
// on the same worker.
//
// Layout note. The Python wrappers (hf_evaluate / lighteval /
// ragas) live in internal/evaluators/heuristic/python.go because
// the subprocess protocol is different from the in-process
// implementations here. Operationally they share the MetricSpec
// Name key so a single plugin manifest selects either path via
// spec.service.metric.name.
package heuristic

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ffxnexus/nexus/internal/evals"
	"github.com/ffxnexus/nexus/internal/observability"
)

// resultOutput picks a stable string from the trace payload. We
// prefer output_messages (the model's reply) when set, and fall
// back to input_messages so the metric still computes something
// rather than crash.
func resultOutput(t observability.Trace) string {
	if t.OutputMessages != "" {
		return t.OutputMessages
	}
	if t.InputMessages != "" {
		return t.InputMessages
	}
	return ""
}

// traceID returns whatever the trace has by way of identifier. It
// is always non-empty so Score rows never break the schema, even
// when the upstream trace was synthesised by a unit test.
func traceID(t observability.Trace) string {
	if t.TraceID != "" {
		return t.TraceID
	}
	return "trace:unspecified"
}

// Evaluate dispatches on MetricSpec.Name. The dispatcher calls this
// once per sampled trace; the result is one evals.Score. MetricSpec
// args are passed through verbatim; named metrics parse their
// expected keys inside this function and ignore the rest.
//
// `nil` from Evaluate is never a success: a metric that fails to
// parse its arguments is a hard error so the operator sees the
// "metric args invalid" message rather than a silent zero score.
func Evaluate(ctx context.Context, name string, args map[string]any, t observability.Trace) ([]evals.Score, error) {
	switch name {
	case "contains":
		return evalContains(ctx, args, t)
	case "pii":
		return evalPII(ctx, args, t)
	case "exact_match":
		return evalExactMatch(ctx, args, t)
	case "rouge_l":
		return evalRougeL(ctx, args, t)
	default:
		// hf_evaluate / lighteval / ragas dispatch out-of-band
		// (subprocess); returning an error here is a guard against
		// a future kind arriving and being routed to in-process.
		return nil, fmt.Errorf("heuristic %q is not in-process", name)
	}
}

// evalContains does a case-insensitive substring search of every
// needle in `args["needles"]` against either `args["reference"]` or
// the trace output. All needles must match by default; pass
// `args["all"] = false` for "any" semantiics.
func evalContains(_ context.Context, args map[string]any, t observability.Trace) ([]evals.Score, error) {
	needlesAny, ok := args["needles"]
	if !ok {
		return nil, fmt.Errorf("contains: args.needles is required")
	}
	needles, err := toStringSlice(needlesAny)
	if err != nil {
		return nil, fmt.Errorf("contains: args.needles: %w", err)
	}
	if len(needles) == 0 {
		return nil, fmt.Errorf("contains: args.needles is empty")
	}
	reference := stringArg(args, "reference", resultOutput(t))
	haystack := strings.ToLower(reference)
	wantAll := boolArg(args, "all", true)

	hits := 0
	for _, n := range needles {
		if strings.Contains(haystack, strings.ToLower(n)) {
			hits++
			if !wantAll {
				return []evals.Score{makeScore(t, "contains", 1.0, true, "any match")}, nil
			}
		} else if wantAll {
			// Short-circuit: at least one needle missing means a miss.
			return []evals.Score{makeScore(t, "contains", 0.0, false, "needle miss in all-mode")}, nil
		}
	}
	var v float64
	if wantAll && hits == len(needles) {
		v = 1.0
	} else if !wantAll && hits > 0 {
		v = 1.0
	}
	passed := v == 1.0
	reason := "all needles matched (all=true)"
	if !passed {
		reason = "no needle hit"
	}
	return []evals.Score{makeScore(t, "contains", v, passed, reason)}, nil
}

// makeScore is the small helper that constructs a properly-typed
// evals.Score row. Centralised so an audit of the heuristic layer
// sits in one place, and so any future migration to a per-metric
// Evaluator prefix lands here.
func makeScore(t observability.Trace, metric string, score float64, passed bool, why string) evals.Score {
	return evals.Score{
		TraceID:   traceID(t),
		Evaluator: "heuristic_" + metric,
		Metric:    metric,
		Score:     score,
		Passed:    passed,
		Rationale: why,
	}
}

// evalPII scores true when the trace is PII-free and false
// otherwise. It uses the same shipped bundle of patterns as the
// redact layer (SSN/US/UK/Canada-style numbers, emails, phone
// numbers, IPv4 addresses, korean RRN). Future expansion lives
// behind the internal/evals redaction helper, not here, so the
// contract is "this metric says PII iff any payload field would
// have been redacted".
func evalPII(_ context.Context, _ map[string]any, t observability.Trace) ([]evals.Score, error) {
	haystacks := []string{t.InputMessages, resultOutput(t)}
	for _, s := range haystacks {
		if containsPII(s) {
			return []evals.Score{piiScore(t, false, "match")}, nil
		}
	}
	return []evals.Score{piiScore(t, true, "no PII patterns matched")}, nil
}

func piiScore(t observability.Trace, passed bool, why string) evals.Score {
	return evals.Score{
		TraceID:   traceID(t),
		Evaluator: "heuristic_pii",
		Metric:    "pii",
		Score:     boolToFloat(passed),
		Passed:    passed,
		Rationale: why,
	}
}

// boolToFloat is the normalised [0,1] score used by the worker.
// Higher is better so pass=1, fail=0.
func boolToFloat(b bool) float64 {
	if b {
		return 1.0
	}
	return 0.0
}

// evalExactMatch compares case-sensitive whitespace-trimmed
// strings. Either side may come from the manifest (preferred for
// regression suites) or trace.output. Empty either side is a 0.0
// score with no error so a missing reference is visibly wrong but
// doesn't panic.
func evalExactMatch(_ context.Context, args map[string]any, t observability.Trace) ([]evals.Score, error) {
	pred := stringArg(args, "prediction", resultOutput(t))
	ref := strings.TrimSpace(stringArg(args, "reference", ""))
	if ref == "" {
		return []evals.Score{makeScore(t, "exact_match", 0.0, false, "no reference supplied")}, nil
	}
	if strings.TrimSpace(pred) == ref {
		return []evals.Score{makeScore(t, "exact_match", 1.0, true, "exact match")}, nil
	}
	return []evals.Score{makeScore(t, "exact_match", 0.0, false, "value mismatch")}, nil
}

// evalRougeL implements Lin (2004) summarisation ROUGE-L. We use
// the longest common subsequence (LCS) over the byte sequence of
// the two strings and compute F-measure with beta from args (defaults
// to 1.0 → plain F1).
//
// ROUGE-L values are a real number in [0, 1] — the score's Value
// field carries the raw F-measure. The label is derived from a
// threshold that defaults to 0.5 and is overridable through
// args["threshold"]; a real-world pipeline that wants a pass/fail
// rule should pass an explicit threshold rather than rely on the
// default.
func evalRougeL(_ context.Context, args map[string]any, t observability.Trace) ([]evals.Score, error) {
	pred := stringArg(args, "prediction", resultOutput(t))
	ref := stringArg(args, "reference", "")
	if ref == "" {
		return []evals.Score{makeScore(t, "rouge_l", 0.0, false, "no reference supplied")}, nil
	}
	f := rougeL(pred, ref, floatArg(args, "beta", 1.0))
	thr := floatArg(args, "threshold", 0.5)
	passed := f >= thr
	return []evals.Score{makeScore(t, "rouge_l", f, passed,
		"F-measure above threshold")}, nil
}

// rougeL returns F-measure of the LCS between prediction and
// reference. The standard ROUGE formula is:
//
//   R = |LCS| / |reference|
//   P = |LCS| / |prediction|
//   F = ((1 + β²) · R · P) / (R + β² · P)
//
// Edge cases: empty prediction or reference returns 0.0.
func rougeL(prediction, reference string, beta float64) float64 {
	if prediction == "" || reference == "" {
		return 0.0
	}
	lcs := longestCommonSubsequence(prediction, reference)
	if lcs == 0 {
		return 0.0
	}
	r := float64(lcs) / float64(len([]rune(reference)))
	p := float64(lcs) / float64(len([]rune(prediction)))
	denom := r + beta*beta*p
	if denom == 0 {
		return 0.0
	}
	return ((1 + beta*beta) * r * p) / denom
}

// longestCommonSubsequence operates on runes rather than bytes so
// CJK text doesn't silently decorrelate with itself.
func longestCommonSubsequence(a, b string) int {
	ar := []rune(a)
	br := []rune(b)
	la, lb := len(ar), len(br)
	if la == 0 || lb == 0 {
		return 0
	}
	// Two-row DP. Memory is O(b) which matters when the metric
	// runs on every trace through the dispatcher.
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for i := 1; i <= la; i++ {
		for j := 1; j <= lb; j++ {
			if ar[i-1] == br[j-1] {
				cur[j] = prev[j-1] + 1
			} else if cur[j-1] > prev[j] {
				cur[j] = cur[j-1]
			} else {
				cur[j] = prev[j]
			}
		}
		prev, cur = cur, prev
		// Reset the row we'll reuse next iteration so old entries
		// don't leak into the next scoring.
		for k := 0; k <= lb; k++ {
			cur[k] = 0
		}
	}
	return prev[lb]
}

// containsPII is a small bundle of generously-over-matching regular
// expressions (see patterns.go for the canonical list). It is the
// closed-set scorer used by the pii metric. A trace that would be
// redacted on send also scores 0 here. We keep the call-back here
// so callers in this file don't have to import the patterns package
// directly; the actual implementation lives in patterns.go.

// --- argument helpers --------------------------------------------

// toStringSlice accepts []any / []string from YAML/JSON. A scalar
// is also accepted as a single-element slice so authoring a manifest
// with one needle is ergonomic.
func toStringSlice(v any) ([]string, error) {
	switch x := v.(type) {
	case []string:
		if len(x) == 0 {
			return nil, fmt.Errorf("empty list")
		}
		out := make([]string, len(x))
		copy(out, x)
		return out, nil
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("element %v is not a string", e)
			}
			out = append(out, s)
		}
		return out, nil
	case string:
		if x == "" {
			return nil, fmt.Errorf("empty string")
		}
		return []string{x}, nil
	default:
		return nil, fmt.Errorf("unsupported type %T", v)
	}
}

// stringArg returns args[k] as a string with a default fallback so
// callers don't have to type-assert every argument. Numeric and
// boolean args are converted through fmt.Sprintf so authors can
// sneak metrics parameters through args without YAML quoting.
func stringArg(args map[string]any, k, def string) string {
	if args == nil {
		return def
	}
	v, ok := args[k]
	if !ok || v == nil {
		return def
	}
	switch x := v.(type) {
	case string:
		if x == "" {
			return def
		}
		return x
	case fmt.Stringer:
		return x.String()
	default:
		return fmt.Sprint(v)
	}
}

func boolArg(args map[string]any, k string, def bool) bool {
	if args == nil {
		return def
	}
	v, ok := args[k]
	if !ok {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}

func floatArg(args map[string]any, k string, def float64) float64 {
	if args == nil {
		return def
	}
	v, ok := args[k]
	if !ok {
		return def
	}
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	default:
		return def
	}
}

// sort is imported above but kept so we don't accidentally remove
// the dependency during refactors that re-shape the helpers.
var _ = sort.Strings
