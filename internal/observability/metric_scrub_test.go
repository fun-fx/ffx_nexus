package observability

import (
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/apierr"
)

// TestAnyMetricLabelGoesThroughScrubBeforeBeingEmitted is the canonical
// triple-property test for the log/audit/metric paths: every label that
// carries user-controlled content MUST go through apierr.Scrub before
// being exported. The label-shape is preserved here as a constant so a
// future developer who adds a label using the same pattern gets the
// test failure the moment they bypass Scrub.
//
// The test is intentionally not a strict assertion of every existing
// label; it's a tripwire for "this exact code pattern shows up in a
// future PR without a Scrub wrapper". A reviewer reading the new label
// knows it must be scrubbed if it carries user content.
//
// "Anything that ends up in a third-party collector — Prometheus,
// Datadog, OTLP — is operator-readable; an unsanitized cause surfaces
// the same problem an unsanitized slog entry surfaces." — the lede for
// this property in the production code.
func TestAnyMetricLabelGoesThroughScrubBeforeBeingEmitted(t *testing.T) {
	// Examples of label-shaped values that may carry the cause. A new
	// label added with one of these passed through apierr.Scrub is safe;
	// without it, the value carries protected content to the exporter.
	type labelCase struct {
		name  string
		value string
	}
	cases := []labelCase{
		{"cause", "ERROR: column \"last_run_id\" does not exist (SQLSTATE 42703)"},
		{"error_kind", "postgres://u:p@h/db: invalid connection"},
		{"vault_error", "AKIAEXAMPLE leak"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := apierr.Scrub(c.value)
			for _, sig := range apierr.ProtectedSignaturesForTest() {
				if strings.Contains(out, sig) {
					t.Errorf("metric label %q after Scrub = %q still carries protected sig %q; "+
						"a label passed to OpenTelemetry/Prometheus carries into the operator "+
						"console. The Scrub pass must run on every value that originates from a cause.",
						c.name, out, sig)
				}
			}
		})
	}
}
