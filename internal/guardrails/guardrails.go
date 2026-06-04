// Package guardrails provides sub-millisecond inline policy checks that run on
// the request hot path, before (input) and after (output) the upstream call.
//
// Unlike the async eval workers (which observe completed traces out-of-band),
// guardrails are synchronous and can block a request or redact a response. They
// are intentionally cheap: regex and length checks only, no network calls.
package guardrails

import (
	"regexp"
	"strings"
)

// Built-in PII detection patterns. Conservative by design to limit false
// positives; these are signals for blocking/redaction, not a compliance-grade
// DLP engine.
var (
	reEmail = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
	rePhone = regexp.MustCompile(`\b(?:\+?\d{1,3}[\s.\-]?)?(?:\(?\d{3}\)?[\s.\-]?)\d{3}[\s.\-]?\d{4}\b`)
	reSSN   = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	reCard  = regexp.MustCompile(`\b(?:\d[ \-]?){13,16}\b`)
)

const redactionMask = "[REDACTED]"

// Config controls which guardrails are active. The zero value is a disabled
// guard (all checks pass).
type Config struct {
	Enabled bool

	// BlockPIIInput rejects requests whose prompt contains PII patterns before
	// any upstream call is made.
	BlockPIIInput bool

	// RedactPIIOutput replaces PII patterns in non-streaming responses with a
	// redaction mask instead of blocking.
	RedactPIIOutput bool

	// MaxInputChars rejects requests whose combined prompt exceeds this many
	// characters. 0 disables the check.
	MaxInputChars int

	// DenyPatterns are raw regular expressions; a request is rejected if any
	// matches the prompt. Invalid patterns are ignored at construction time.
	DenyPatterns []string
}

// Finding is the result of a guardrail check.
type Finding struct {
	Blocked bool   // request/response should be rejected
	Rule    string // identifier of the rule that fired (e.g. "pii_input")
	Reason  string // human-readable explanation
}

// allow is the shared "passed" finding.
var allow = Finding{}

// Guard evaluates inline guardrails. A nil *Guard is a no-op (all checks pass),
// so callers can hold an optional guard without nil-checking every call site.
type Guard struct {
	cfg          Config
	denyCompiled []*regexp.Regexp
}

// New compiles the configuration into a Guard. It returns nil when guardrails
// are disabled so the gateway can treat "no guard" and "disabled" identically.
func New(cfg Config) *Guard {
	if !cfg.Enabled {
		return nil
	}
	g := &Guard{cfg: cfg}
	for _, p := range cfg.DenyPatterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if re, err := regexp.Compile(p); err == nil {
			g.denyCompiled = append(g.denyCompiled, re)
		}
	}
	return g
}

// Active reports whether any input or output guardrail is configured.
func (g *Guard) Active() bool {
	if g == nil {
		return false
	}
	return g.cfg.BlockPIIInput || g.cfg.RedactPIIOutput ||
		g.cfg.MaxInputChars > 0 || len(g.denyCompiled) > 0
}

// CheckInput evaluates the combined prompt text against input guardrails. The
// returned Finding is Blocked when the request must not reach the upstream.
func (g *Guard) CheckInput(prompt string) Finding {
	if g == nil {
		return allow
	}
	if g.cfg.MaxInputChars > 0 && len(prompt) > g.cfg.MaxInputChars {
		return Finding{Blocked: true, Rule: "max_input_chars",
			Reason: "request prompt exceeds the configured maximum length"}
	}
	if g.cfg.BlockPIIInput {
		if hits := piiHits(prompt); len(hits) > 0 {
			return Finding{Blocked: true, Rule: "pii_input",
				Reason: "request prompt contains disallowed PII: " + strings.Join(hits, ", ")}
		}
	}
	for _, re := range g.denyCompiled {
		if re.MatchString(prompt) {
			return Finding{Blocked: true, Rule: "deny_pattern",
				Reason: "request prompt matches a blocked pattern: " + re.String()}
		}
	}
	return allow
}

// RedactOutput applies output guardrails to response text. It returns the
// (possibly modified) text and whether any redaction occurred.
func (g *Guard) RedactOutput(text string) (string, bool) {
	if g == nil || !g.cfg.RedactPIIOutput || text == "" {
		return text, false
	}
	redacted := text
	for _, re := range []*regexp.Regexp{reEmail, reSSN, rePhone, reCard} {
		redacted = re.ReplaceAllString(redacted, redactionMask)
	}
	return redacted, redacted != text
}

// piiHits returns the kinds of PII detected in text, in a stable order.
func piiHits(text string) []string {
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
	return hits
}
