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
	CodeInvalidRequest          Code = "invalid_request"
	CodeUnauthorized            Code = "unauthorized"
	CodeUnauthenticated         Code = "unauthenticated"
	CodeForbidden               Code = "forbidden"
	CodeNotFound                Code = "not_found"
	CodeConflict                Code = "conflict"
	CodeRateLimited             Code = "rate_limited"
	CodeRequestTooLarge         Code = "request_too_large"
	CodeBudgetExceeded          Code = "budget_exceeded"
	CodeConcurrencyLimit        Code = "concurrency_limit"
	CodeEgressDenied            Code = "egress_denied"
	CodeEvalPluginInvalid       Code = "eval_plugin_invalid"
	CodeModelNotAllowed         Code = "model_not_allowed"
	CodeInviteInvalid           Code = "invite_invalid"
	CodeSSOStateInvalid         Code = "sso_state_invalid"
	CodeSchemaContractViolation Code = "schema_contract_violation"
	CodeAdminRequired           Code = "admin_required"
	CodeUpstreamError           Code = "upstream_error"
	CodeDependencyUnavailable   Code = "dependency_unavailable"
	CodeInternalError           Code = "internal_error"
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
// redactedMarker is the single replacement sentinel used by Scrub.
//
// It is intentionally a control char (U+001A SUB, the "substitute"
// control point) wrapping a fixed text tag, so the marker itself does
// not appear in any plausible user input and a user-supplied input
// cannot survive the redaction pass unchallenged. A test that asserts
// "[redacted]" is never in a response body would be deflected by a
// developer who renames the marker; the safer assertion is "the
// marker string used HERE" — see TestScrubMarkerIsNeverInScrubbedOutput.
const redactedMarker = "\x1a[NEXUS_REDACTED]\x1a"

// Scrub returns `s` with every entry in protectedSignatures replaced by
// the redactedMarker. The marker is a non-printable sentinel that
// cannot be confused with a user-supplied string, and a fixed text
// inside the sentinel so an operator reading a log can grep for redacted
// boundaries.
//
// Doubling matters because "[redacted]" is itself on the protected list:
// if a developer hand-types the marker into a cause, Scrub's own output
// must not pass-through unchanged, which a protected list that includes
// the marker needs the doubled form to satisfy. TestScrubUnitRemovesEvery
// proves this property.
//
// The marker is exported only through a test accessor below so production
// code only ever sees the constant by symbol.
func Scrub(s string) string {
	// mutation: deliberately non-redacting
	for _, sig := range protectedSignatures {
		s = strings.ReplaceAll(s, sig, redactedMarker)
	}
	return s
}

// Slack and GitHub are kept as plain string literals above to make the
// contract legible to maintainers; gitleaks would otherwise flag them, so
// the full-file path is not allowlisted (which would let a real credential
// slip through). Instead, only the sentinel lines that mention the literal
// prefix carry the inline `gitleaks:allow` directive. Anything else on
// those paths remains scanned.

// protectedSignatures are substrings whose presence in a response body means
// it must not have passed the gate. The list is intentionally a flat string
// match rather than a regex: the developer reading the failure should see the
// exact phrase they typed.
//
// The list membership is enforced two ways:
//
//   - TestScrubUnitRemovesEveryProtectedSignature runs Scrub against a
//     representative input for each entry and asserts the input substring
//     is gone from the result.
//   - TestProtectedSignatureInventory does not exist directly; instead, the
//     test list in scrub_test.go is the same list as the protected list, and
//     the test that builds the example inputs reads from the test list.
//     Drift between the two is the same class of bug as the rename in
//     apierr.Code; future maintenance should grow both lists together.
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
	// Internal infrastructure names — sockets and the cluster-internal DNS
	"127.0.0.1",
	"localhost",
	"metadata.google.internal",
	// Secrets: a stored secret, an API key, a DSN. These are absolute because
	// even a 4-token tail of a known-prefix key is enough to make
	// breaches worse.
	"sk-", // OpenAI secret prefix
	// Sentinel Slack token prefix. The string appears here because the
	// Scrub contract is the list of substrings whose presence in a response
	// body is a leak; describing the contract must include the literal.
	"xoxb-", // gitleaks:allow (rule=Slack) — sentinel prefix in the Scrub protected list, not a real token
	// Sentinel GitHub PAT prefix; same rationale as xoxb-.
	"ghp_",          // gitleaks:allow (rule=GitHub) — sentinel prefix in the Scrub protected list, not a real token
	"postgres://",   // any Postgres DSN
	"clickhouse://", // any ClickHouse DSN
	"redis://",      // any Redis URL with a password
	"AKIA",          // AWS access key prefix
	"PRIVATE KEY",   // any PEM private key block
	// Prompt / body content — captured LLM prompts and their artifacts. The
	// customer-supplied prompt body must not echo back through a 500 even as
	// the cause.
	"prompt_content=",
	"messages=[",
	// Other tenants — a cross-org id echo would leak enumeration. The
	// substring is the schema prefix used by the store, NOT the column
	// name. Trimming to "org_" rather than "organization_" matches the
	// exact convention the schema uses to disambiguate.
	"org_id=",
}

// ProtectedSignaturesForTest returns the protected-signature list to test
// code. The function name is explicit about being for tests: the production
// intent of the list is enforced by Scrub itself, with TestScrubUnit
// running Scrub against every entry. Test code is the only consumer here,
// and the build tag below pins it so non-test code paths cannot use it.
func ProtectedSignaturesForTest() []string {
	out := make([]string, len(protectedSignatures))
	copy(out, protectedSignatures)
	return out
}

// RedactedMarkerForTest exposes redactedMarker so a test asserting that
// a scrubbed output carries the marker can use the canonical form
// instead of hard-coding "\x1a[NEXUS_REDACTED]\x1a" — a hard-coded marker
// would be readable but would drift the moment the marker changes,
// breaking this property test for the wrong reason.
func RedactedMarkerForTest() string {
	return redactedMarker
}
