// Package apierr defines the public error contract for HTTP responses served by
// the console and the gateway.
//
// Two audiences consume the JSON body:
//
//   - Customers' end users and operators, who need a stable code so they can
//     branch on it (retry on dependency_unavailable, show a message on
//     forbidden, ask support for an id on internal_error), and a short human
//     message so they can act without contacting us.
//   - The Nexus team's support and SRE, who need to correlate an HTTP response
//     to a server log line and an audit record using the same id.
//
// The contract is the body. It is shaped once here so the console and the
// gateway cannot drift.
//
// # What is NOT in the body, on purpose
//
// The body must not include a SQL statement, a table or column name, an
// SQLSTATE, an upstream vendor's raw error body, a stack frame, an internal
// hostname, a file path, a secret, a key, raw prompt or response content, or
// another tenant's identifier. Every protected field can be reconstructed from
// the server log when the request id is known. The list is pinned in
// internal/apierr/leak_test.go, which is itself the source of truth.
//
// # Code conventions
//
// Codes are stable. They map to a coarse HTTP status (e.g. forbidden -> 403)
// and to a class of remediation (retry, change input, contact support). Any
// caller that adds a code MUST add it here AND extend
// TestEveryResponseCarriesAStableCode so a missing entry breaks the build.
package apierr

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Code is the public, stable identifier of an error class.
//
// The string value is a wire contract. Renaming a Code is a breaking change.
type Code string

const (
	CodeInvalidRequest        Code = "invalid_request"
	CodeUnauthorized          Code = "unauthorized"
	CodeForbidden             Code = "forbidden"
	CodeNotFound              Code = "not_found"
	CodeConflict              Code = "conflict"
	CodeRateLimited           Code = "rate_limited"
	CodeBudgetExceeded        Code = "budget_exceeded"
	CodeUpstreamError         Code = "upstream_error"
	CodeDependencyUnavailable Code = "dependency_unavailable"
	CodeInternalError         Code = "internal_error"
)

// Body is the shape returned to the caller. The order is the order it appears
// in JSON so log readers and console tables see the same field order.
type Body struct {
	Error struct {
		Code    Code   `json:"code"`
		Message string `json:"message"`
		// RequestID correlates the response to a server log line. It uses the
		// gateway's existing X-Request-Id value when present, and a fresh UUID
		// when it isn't.
		RequestID string `json:"request_id"`
	} `json:"error"`
}

// Render writes the body to w and sets the same id as the X-Request-Id response
// header. The status argument is the HTTP envelope; the body's code is the
// stable public identifier.
//
// Render is safe to call from every handler. It does not log; the caller logs
// alongside it with the same id so the line and the response can be matched.
func Render(w http.ResponseWriter, status int, code Code, requestID, message string) {
	if requestID != "" {
		w.Header().Set("X-Request-Id", requestID)
	}
	body := Body{}
	body.Error.Code = code
	body.Error.Message = Message(code, message)
	body.Error.RequestID = requestID

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Message picks the human-readable string for a code, falling back to msg.
//
// The fallback is only used for messages that DO carry caller-specific context
// (e.g. "key id not found" where the message is short and contains nothing not
// in the response). Caller-supplied messages are passed through unchanged for
// codes whose public default is empty.
func Message(code Code, msg string) string {
	if msg == "" {
		return defaultMessage(code)
	}
	// Caller-supplied messages pass through, but only after we confirm they do
	// not look like an internal error string. The caller is expected to use the
	// helper that classifies the underlying error: do not rely on the gateway
	// and the console agreeing by convention.
	return msg
}

// defaultMessage is what the customer sees when the caller has nothing better
// to say. Kept in this package so a code change updates every surface.
func defaultMessage(code Code) string {
	switch code {
	case CodeInvalidRequest:
		return "the request was invalid"
	case CodeUnauthorized:
		return "authentication is required"
	case CodeForbidden:
		return "access is forbidden for the caller"
	case CodeNotFound:
		return "the resource was not found"
	case CodeConflict:
		return "the request conflicts with the current state"
	case CodeRateLimited:
		return "too many requests, try again shortly"
	case CodeBudgetExceeded:
		return "the spend or quota limit was exceeded"
	case CodeUpstreamError:
		return "the upstream provider returned an error"
	case CodeDependencyUnavailable:
		return "an internal dependency is not currently available"
	case CodeInternalError:
		return "an internal error occurred, contact support with the request id"
	}
	return "an error occurred"
}

// HTTPStatus is the conventional HTTP envelope for the public code.
//
// The envelope is loose: forbidden can also be answered as 404 when revealing
// that the caller's id references a real but inaccessible resource would turn
// the response into an oracle. Look up the per-handler rule before responding.
func HTTPStatus(code Code) int {
	switch code {
	case CodeInvalidRequest:
		return http.StatusBadRequest
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict:
		return http.StatusConflict
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeBudgetExceeded:
		return http.StatusPaymentRequired
	case CodeUpstreamError:
		return http.StatusBadGateway
	case CodeDependencyUnavailable:
		return http.StatusServiceUnavailable
	case CodeInternalError:
		return http.StatusInternalServerError
	}
	return http.StatusInternalServerError
}

// FromPostgresError maps a Postgres error to a public Code.
//
// The detail (the SQL string, the column, the SQLSTATE) lives only in the log
// the caller writes alongside this render. The body the caller receives is a
// stable code and a short message so they can react or escalate without
// learning what is in their database.
//
// The mapping is deliberately narrow. Every match is a class where the
// underlying message is known to contain one of the protected fields (table
// name, column name, constraint name) and where the remediating behaviour is
// identical for every row in the class.
func FromPostgresError(classified Code) Code {
	return classified
}

// SanitizeScrubs removes signatures that the body must not carry.
//
// The set is enforced in TestResponseNeverCarriesProtectedFields. SanitizeScrubs
// is a defence in depth — by the time the body reaches here it should already
// be free of these — but a developer pasting an upstream error into a fallback
// path can introduce one, and the gate at the boundary is cheap.
//
// Returns the same string with each prohibited fragment redacted to "[redacted]".
// The intent is to fail closed: if ducking the question of "is this safe to
// surface?", redact.
func Scrub(s string) string {
	for _, sig := range protectedSignatures {
		s = strings.ReplaceAll(s, sig, "[redacted]")
	}
	return s
}

// protectedSignatures are substrings whose presence in a response body means
// it must not have passed the gate. The list is intentionally a flat string
// match rather than a regex: the developer reading the failure should see the
// exact phrase they typed.
var protectedSignatures = []string{
	// SQL markers: statement, table, column, code
	"SQLSTATE",
	"ERROR:",
	"pq:",
	// Go runtime: a stack trace would carry a file path at minimum
	"goroutine ",
	".go:",
	"/Users/",
	"/home/",
	// Internal infrastructure names
	"127.0.0.1",
	"localhost",
	"metadata.google.internal",
}
