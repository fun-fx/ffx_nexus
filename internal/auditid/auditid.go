// Package auditid is the single source of truth for the correlation id used
// in audit_log rows. An audit row without a non-empty correlation id is a
// defect: support cannot join a customer-reported error to either the server
// log or the audit record without it.
//
// Two paths feed auditid:
//
//   - HTTP path: the gateway's RequestID middleware seeds the context with a
//     server-generated id (prefix "req-"). The X-Request-Id header carries the
//     same id out to the client. The audit row therefore carries the same
//     id the customer sees in their error response.
//
//   - Background path: a worker / scheduler / boot-time job calls
//     auditid.WithJob(ctx, "scheduler.weekly-cleanup") before running. The
//     resulting id is prefixed "job-" and is stable across that job's lifetime.
//
// In both paths, auditid.FromContext always returns a non-empty string. A
// caller cannot produce an audit row with empty id even with deliberate effort:
// Store.Audit consults FromContext, not the caller's argument.
//
// The X-Request-Id header in the HTTP request is also a candidate for the
// audit row, but it is untrusted input (attacker can set it for log injection
// or id collisions). The header value is *separately* recorded as
// "client_request_id" in the audit row after a length and charset check; the
// correlation id itself remains server-generated.
package auditid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"

	"github.com/ffxnexus/nexus/internal/resp"
)

// clientRequestIDMaxLen is the longest client-supplied X-Request-Id value we
// will record. 128 chars is well past any reasonable nginx / envoy forwarder
// while still bounded for storage.
const clientRequestIDMaxLen = 128

// clientRequestIDAllowed is the alphabet for client-supplied ids. We do not
// allow control characters, whitespace, brackets, slashes, or anything an
// attacker could use to inject log lines. A failed match means the header is
// ignored and stored as "".
const clientRequestIDAllowed = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"

type ctxKey int

const (
	jobKey ctxKey = iota + 1
	clientKey
)

// WithJob stamps the context with a job correlation id of the form
// "job-<origin>-<rand>". The same id will be returned by FromContext for any
// code in the call chain that asks for it, and any audit row written through
// the chain will carry the same id.
//
// origin is a short, lower-case identifier of the producer ("scheduler.weekly-cleanup",
// "boot.eval-seed", "worker.benchmark.reset"). It must begin with a letter
// and contain only letters, digits, dots, underscores or dashes. Anything that
// fails the check is replaced with "anon" rather than rejected — we want the
// audit row to be written, not dropped.
func WithJob(ctx context.Context, origin string) context.Context {
	if !validOrigin(origin) {
		origin = "anon"
	}
	id := "job-" + origin + "-" + randomHex(8)
	return context.WithValue(ctx, jobKey, id)
}

// WithClientRequestID stamps the client-side X-Request-Id header value, if it
// passes the length and charset filter. Empty / invalid values are stored as
// "" — the server-generated correlation id remains the audit join key.
func WithClientRequestID(ctx context.Context, header string) context.Context {
	if !validClientRequestID(header) {
		return ctx
	}
	return context.WithValue(ctx, clientKey, header)
}

// FromContext resolves the correlation id the audit row should carry. It
// consults, in order:
//
//  1. The HTTP request id set by resp.RequestIDFromContext (prefix "req-"),
//     produced by the gateway middleware.
//  2. The job id set by WithJob (prefix "job-").
//  3. As a last resort, a freshly-generated "srv-<hex>" id so callers can
//     never produce an audit row with an empty correlation.
//
// The returned string is always non-empty and always server-controlled.
// Client input never reaches this function as a final value.
func FromContext(ctx context.Context) string {
	if ctx == nil {
		return newServerID()
	}
	if v, ok := ctx.Value(resp.RequestIDKey()).(string); ok && v != "" {
		return v
	}
	if v, ok := ctx.Value(jobKey).(string); ok && v != "" {
		return v
	}
	return newServerID()
}

// ClientRequestID returns the sanitised client-supplied X-Request-Id, or "" if
// the header was empty / invalid. The server-controlled correlation id (see
// FromContext) is always preferred for the join key; this value is recorded
// for forensics only.
func ClientRequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(clientKey).(string); ok {
		return v
	}
	return ""
}

// NewServerID returns a brand new server-controlled id. Exposed so Store.Audit
// can fall back to this when no context is available at all (e.g. unit tests).
func NewServerID() string { return newServerID() }

func newServerID() string { return "srv-" + randomHex(12) }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand should not fail on a healthy OS. If it does, write a
		// recognisable no-rand marker so audit and observability see the
		// anomaly instead of silently producing "".
		return "norand0000000000"
	}
	return hex.EncodeToString(b)
}

func validOrigin(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

func validClientRequestID(s string) bool {
	if s == "" {
		return false
	}
	if len(s) > clientRequestIDMaxLen {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune(clientRequestIDAllowed, r) {
			return false
		}
	}
	return true
}
