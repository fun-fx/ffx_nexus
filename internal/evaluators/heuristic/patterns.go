package heuristic

import (
	"regexp"

	"github.com/ffxnexus/nexus/internal/observability"
)

// piiPatterns is the canonical redaction-style list of PII patterns
// that we score against. They are deliberately permissive on
// purpose: a scorer that misses a real PII hit silently looks
// identical to a scorer that's never seen PII, while a scorer that
// flags an embedded test fixture rarely costs more than a flag.
// Operators tune args.threshold per dataset.
//
// New patterns MUST be appended, never inserted, so a regex index
// that evals.Score wanted to log/trace in the future stays stable.
//
// Keep in sync:
//
//   - internal/evaluators/external/dispatcher.go redactPayload
//     must redact the same needles this list matches, otherwise a
//     trace could leak a forbidden token to the vendor while the
//     in-process pii metric still scored 1.0.
var piiPatterns = []*regexp.Regexp{
	// SSN, US format with dashes OR contiguous digits, last-4 not OK.
	regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),
	regexp.MustCompile(`\b\d{9}\b`),
	// UK NHS-style, 10 digits in 3/3/4 groups, dashed or contiguous.
	regexp.MustCompile(`\b\d{3} \d{3} \d{4}\b`),
	// Korean Resident Registration Number: 6 digits - separator - 7 digits.
	regexp.MustCompile(`\b\d{6}-[1-4]\d{6}\b`),
	// Email, RFC-light variant — covers >99% of real cases.
	regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`),
	// Phone numbers, US and KR shape, allowing separators.
	regexp.MustCompile(`\b\d{3}-\d{3}-\d{4}\b`),
	regexp.MustCompile(`\b\d{3}-\d{4}-\d{4}\b`),
	// IPv4 quadruples (loose; matches 1.2.3.4 through 255.255.255.255).
	regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`),
}

// containsPII is the closed-set scorer used by the pii metric. It
// walks piiPatterns and returns true on the first hit so a long
// trace doesn't pay full latency for an obvious match.
//
// Returns false for an empty input — by convention a zero-byte trace
// is not PII so callers don't have to special-case it.
func containsPII(s string) bool {
	if s == "" {
		return false
	}
	for _, p := range piiPatterns {
		if p.MatchString(s) {
			return true
		}
	}
	return false
}

// RedactPII walks the trace and returns a copy with a placeholder
// substituted for each pattern hit. The dispatcher uses this
// internally to enforce the redact=pii YAML field; the heuristic
// pii METRIC does not need it (scoring on the original against
// the same patterns is sufficient).
//
// This is exported so the dispatcher's redact layer can share the
// pattern list with the metric instead of having two re-implementations
// of "what counts as PII", which is a footgun: a vendor receiving a
// redacted version of the trace AND a metric scoring the unredacted
// version against the same patterns is a debug hazard.
//
// in-process. The Trace struct keeps InputMessages and OutputMessages
// as raw text; the redact pipeline rewrites them in place. Empty
// fields are passed through unchanged so the dispatcher does not
// add a 200ms no-op per trace under normal load.
func RedactPII(t observability.Trace) observability.Trace {
	out := t
	if t.InputMessages == "" && t.OutputMessages == "" {
		return out
	}
	out.InputMessages = redact(t.InputMessages)
	out.OutputMessages = redact(t.OutputMessages)
	return out
}

func redact(s string) string {
	if s == "" {
		return s
	}
	out := s
	for _, p := range piiPatterns {
		out = p.ReplaceAllString(out, "[REDACTED:pii]")
	}
	return out
}

// IsPIIReport summarises a trace's PII exposure. Used by tests and
// health-debug endpoints to explain "why did this metric score
// 0?".
type IsPIIReport struct {
	HasPII bool
	Hits   []string // patterns that fired, namespaced
}

// InspectPII runs the same patterns against the trace and returns
// which ones matched. The names are stripped of the regex machinery
// so callers can log them without leaking a regex that an attacker
// could use to find gaps.
func InspectPII(t observability.Trace) IsPIIReport {
	payloads := []string{t.InputMessages, t.OutputMessages}
	report := IsPIIReport{}
	for _, s := range payloads {
		if s == "" {
			continue
		}
		for i, p := range piiPatterns {
			if p.MatchString(s) {
				report.HasPII = true
				report.Hits = append(report.Hits, patternName(i))
			}
		}
	}
	return report
}

// patternName is the human label for the i-th piiPatterns entry.
// Add new entries in tandem here and in piiPatterns above so they
// stay in lock-step.
func patternName(i int) string {
	switch i {
	case 0:
		return "ssn-dashed"
	case 1:
		return "ssn-contiguous"
	case 2:
		return "uk-nhs"
	case 3:
		return "kr-rrn"
	case 4:
		return "email"
	case 5:
		return "phone-us-dashed"
	case 6:
		return "phone-kr"
	case 7:
		return "ipv4"
	}
	return "unknown"
}
