// Package resp is the response-side wrapper around apierr that closes two gaps
// the bare apierr.Render cannot close on its own:
//
//   - Logging the cause alongside the response. The whole point of the public
//     body is to OMIT the cause, so the cause MUST land in the server log on
//     the same code path, with the same request id, or correlation breaks.
//   - Wrapping an error the caller holds into a response without typing the
//     error's underlying type. Nearly every leak in the console today is a
//     writeJSON with `err.Error()` in the body. resp.HTTP removes the template
//     and forces the cause through the gate, which is what closes the leak.
//
// resp.HTTP logs the cause and renders the body. The body never includes the
// cause. Logs never include raw prompts, response bodies, secrets, or full
// keys.
package resp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ffxnexus/nexus/internal/apierr"
)

// requestIDKey is the context key the gateway's RequestID middleware uses.
// It is unexported in the gateway package today; this mirror is INTENTIONALLY
// the same string so a context value set by gateway.RequestID reads back as
// non-empty here without depending on the gateway's internals. If the
// gateway's key ever changes, this MUST change with it — the assertion in
// internal/apierr/leak_test.go::TestRequestIDPropagatesFromHeaderAndContext
// will fail and force a coordinated update.
const requestIDKey contextKey = "request_id"

type contextKey string

// HTTP writes a public error response and records the cause in the log.
//
//	statusCode:  the HTTP envelope. If zero, apierr.HTTPStatus(code) is used.
//	code:        a public Code from apierr. MUST be one of the listed constants.
//	requestID:   the X-Request-Id for this request.
//	log:         optional. When nil, slog.Default is used so a misconfigured
//	             caller still leaves a correlated evidence trail.
//	cause:       nil is permitted. When non-nil, it is logged scrubbed of
//	             protected substrings; the body never includes it.
//
// The log line carries:
//
//	event      = "http_error"
//	code       = the public code reaching the client
//	status     = the actual HTTP envelope
//	request_id = the id also written in the body and the X-Request-Id header
//	cause      = the joined error chain, scrubbed
//
// The cause is logged even when no error occurred, so a dependency that
// ultimately succeeds can still leave a forensic trail. Callers should add
// their own context (org, user, route) via slog.With at the call site.
func HTTP(w http.ResponseWriter, r *http.Request, statusCode int, code apierr.Code, requestID string, cause error, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	if requestID == "" {
		requestID = RequestIDFromContext(r.Context())
	}
	if code == "" {
		code = apierr.CodeInternalError
	}
	if statusCode == 0 {
		statusCode = apierr.HTTPStatus(code)
	}

	log.LogAttrs(r.Context(), slog.LevelError, "http_error",
		slog.String("event", "http_error"),
		slog.String("code", string(code)),
		slog.Int("status", statusCode),
		slog.String("request_id", requestID),
		slog.String("cause", scrubChain(cause)),
	)
	if requestID != "" {
		w.Header().Set("X-Request-Id", requestID)
	}
	apierr.Render(w, statusCode, code, requestID, apierr.Message(code, ""))
}

// RequestIDFromContext returns the request id set by the gateway's RequestID
// middleware, or empty when absent.
//
// Header is read AFTER the context, so a context value always wins. This
// preserves the property that the body, the header and the log carry the SAME
// id: when the middleware sets the context first, no later handler can change
// it by echoing a different header.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok && v != "" {
		return v
	}
	return ""
}

// RequestIDFromHeader reads X-Request-Id from the request. Used by the
// middleware to seed the context, and by tests that drive the mux without the
// middleware.
func RequestIDFromHeader(r *http.Request) string { return r.Header.Get("X-Request-Id") }

// scrubChain joins each unwrapped error into a single line and scrubs the
// protected substrings. Each step is scrubbed so a leak in an inner message
// is still caught.
func scrubChain(err error) string {
	if err == nil {
		return ""
	}
	var parts []string
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		parts = append(parts, apierr.Scrub(cur.Error()))
	}
	return strings.Join(parts, " | ")
}

// RequestIDKey is exported so test code outside this package can put a value
// the same way the middleware does, and assert the response picks it up.
func RequestIDKey() contextKey { return requestIDKey }
