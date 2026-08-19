package console

import (
	"context"

	"github.com/ffxnexus/nexus/internal/apierr"
	"github.com/ffxnexus/nexus/internal/resp"
)

// requestIDFromContext extracts the X-Request-Id-equivalent the gateway's
// RequestID middleware stored. Empty string when the request didn't pass
// through the middleware (e.g. a one-shot CLI invocation).
func requestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v := ctx.Value(resp.RequestIDKey()); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// scrubDetail runs apierr.Scrub over an audit "detail" string so a SQL
// fragment, stack trace, or DSN that an operator allowed into the field
// cannot leak back through the audit feed an admin reads.
//
// The audit_detail column is the only place we may legitimately see the
// *unsanitized* cause (operators need it to triage); this scrub pass
// exists so that operator-friendly surfaces (admin's GET /api/audit, the
// CSV export, dashboards) cannot inadvertently share the unsanitized
// form. The cause still survives in the slog entry where it belongs.
func scrubDetail(s string) string {
	return apierr.Scrub(s)
}

// scrubTargetID is the same Scrub pass for actor/target/org identifiers.
// A cross-org UUID echo is a pre-existing failure class: an attacker
// probing for valid ids can swing a 404 into a confirmation if the body
// echoes the input. The audit feed faces the same risk: scrubbing the
// stored value is a second-line defence.
func scrubTargetID(s string) string {
	if s == "" {
		return ""
	}
	return apierr.Scrub(s)
}
