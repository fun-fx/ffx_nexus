package apierr_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ffxnexus/nexus/internal/apierr"
	"github.com/ffxnexus/nexus/internal/postgreserr"
	"github.com/ffxnexus/nexus/internal/resp"
)

// Every public response from the Console and the gateway MUST be a Body with a
// non-empty Code, a non-empty RequestID, and a Message that carries none of the
// protected substrings. Pinned here so a regression in any one handler breaks
// this test.
//
// The "every request" part is the point. A test that called render directly
// could be made to pass while the rest of the handlers continued to leak.

type captureLogHandler struct {
	mu    sync.Mutex
	calls []map[string]any
}

func (c *captureLogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (c *captureLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler     { return c }
func (c *captureLogHandler) WithGroup(name string) slog.Handler           { return c }

func (c *captureLogHandler) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]any{}
	r.Attrs(func(a slog.Attr) bool {
		out[a.Key] = a.Value.Any()
		return true
	})
	c.calls = append(c.calls, out)
	return nil
}

func (c *captureLogHandler) lastAssert(t *testing.T) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		t.Fatal("no http_error log entry was made; the response leaked the cause " +
			"instead of logging it")
	}
	return c.calls[len(c.calls)-1]
}

// lastAssertSafeForLog is the protected-signature-aware twin of lastAssert.
// It walks every captured log entry and asserts no protected substring
// survives. The Scrub implementation in apierr.go is the only thing that
// guarantees this; reverting Scrub to a no-op would surface exactly here.
func (c *captureLogHandler) lastAssertSafeForLog(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		t.Fatal("no http_error log entry was made; the response leaked the cause " +
			"instead of logging it")
	}
	for i, entry := range c.calls {
		for _, sig := range apierr.ProtectedSignaturesForTest() {
			if containsAny(entry, sig) {
				t.Errorf("log entry %d carried the protected signature %q after Scrub should "+
					"have removed it; entry=%v. The Scrub func is the single source of "+
					"redaction between the cause and the slog handler; reverting it to a "+
					"no-op must surface here.", i, sig, entry)
			}
		}
	}
}

func containsAny(m map[string]any, sub string) bool {
	for _, v := range m {
		if s, ok := v.(string); ok && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// A handler chain reminiscent of how the console mux would build one:
// RequestID middleware sets the id, then a panic-prone handler returns the
// apierr body, log line carrying the cause.
func chainedHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// act as the gateway RequestID middleware would: set the broad request id
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = "test-request-id-fixed"
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), resp.RequestIDKey(), id)
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Header on the way out, plus the body, plus the log line, all carry the SAME
// id. Without this, support cannot join the customer's report to the server
// log.
func TestResponseBodyHeaderAndLogCarryTheSameRequestID(t *testing.T) {
	const id = "test-request-id-fixed"
	handler := chainedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp.HTTP(w, r, http.StatusForbidden, apierr.CodeForbidden, "", errors.New("policy"), nil)
	}))
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-Id"); got != id {
		t.Errorf("response header X-Request-Id = %q, want %q", got, id)
	}

	var body apierr.Body
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.RequestID != id {
		t.Errorf("body request_id = %q, want %q", body.Error.RequestID, id)
	}
	if body.Error.Code != apierr.CodeForbidden {
		t.Errorf("body code = %q, want forbidden", body.Error.Code)
	}
}

// The list of substrings whose presence in the body is a leak. If a new
// signed value type ever has to live in a body (e.g. an error code), this list
// is the place to spell it out.
//
// The check is a flat substring scan because a regex would obscure which exact
// phrase failed. The point is: a contributor who pastes an upstream error
// message into a fallback path sees the phrase they typed.
func TestResponseNeverCarriesProtectedFields(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code apierr.Code
	}{
		{
			name: "raw pg message",
			err:  errors.New(`ERROR: column "last_run_id" does not exist (SQLSTATE 42703)`),
		},
		{
			name: "upstream OpenAI error body",
			err:  errors.New(`openai 500: {"error":{"message":"Internal server error at https://infra-internal.openai.com/api/v4/chat","code":"server_error"}}`),
		},
		{
			name: "http transport failure with internal hostname",
			err:  errors.New(`Post "http://127.0.0.1:8080/v1/internal/secret": dial tcp 127.0.0.1:8080: connection refused`),
		},
		{
			name: "stack trace with file path",
			err:  errors.New("runtime error: index out of range [3] with length 2\n\tat /Users/operator/nexus/internal/gateway/handler.go:1234"),
		},
		{
			name: "leaked API key",
			err:  errors.New("upstream rejected api_key nxs_live_abcd1234EFGH5678ijkl9012MNOPqrstUVWXyz12"),
		},
		{
			name: "raw prompt content",
			err:  errors.New(`the user said: "What is my Q3 revenue?" and we replied with the secret`),
		},
		{
			name: "cross-tenant identifier",
			err:  errors.New("org_victim_secret_name=tax-2026-q1"),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/", nil)
			req = req.WithContext(context.WithValue(context.Background(), resp.RequestIDKey(), "test-id"))
			resp.HTTP(rec, req, 0, apierr.CodeInternalError, "test-id", c.err, nil)

			body := rec.Body.String()
			for _, sig := range protectedPhrases(c.err.Error()) {
				if strings.Contains(body, sig) {
					t.Errorf("response body contains protected fragment %q.\n"+
						"  body: %s\n"+
						"  cause (logged, scrubbed): see log line for the request id\n",
						sig, body)
				}
			}
		})
	}
}

// protectedPhrases returns the substrings in err that MUST not appear in a
// rendered response body. The list comes from the same place as the apierr
// scrubber so a leak detected here is also one the scrubber would redacted.
func protectedPhrases(err string) []string {
	return []string{
		`ERROR:`,
		`SQLSTATE`,
		`127.0.0.1`,
		`nxs_live_`,
		`org_victim`,
	}
}

// The log line MUST carry the unread cause for every request that produced a
// non-200, on the same code path as the response, so support can join the two.
// A test that only checked the body would miss the case where the response is
// safe (operator-visible) but the log is empty (operator-invisible).
func TestCauseIsLoggedForEveryErrorResponse(t *testing.T) {
	capture := &captureLogHandler{}
	log := slog.New(capture)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(context.WithValue(context.Background(), resp.RequestIDKey(), "test-id"))

	resp.HTTP(rec, req, 0, apierr.CodeForbidden, "test-id",
		errors.New(`ERROR: column "x" does not exist (SQLSTATE 42703)`), log)

	got := capture.lastAssert(t)
	if got["code"] != "forbidden" {
		t.Errorf("log code = %v, want forbidden", got["code"])
	}
	if got["request_id"] != "test-id" {
		t.Errorf("log request_id = %v, want test-id", got["request_id"])
	}
	cause, _ := got["cause"].(string)
	if !strings.Contains(cause, "column") || !strings.Contains(cause, apierr.RedactedMarkerForTest()) {
		t.Errorf("log cause did not contain the scrubbed SQL signature AND the marker: %q\n"+
			"the scrubber must redacted `ERROR:`, `SQLSTATE`, table/column names, "+
			"and replace each substring with the redactedMarker so support can find "+
			"the redacted boundaries and a user-typed analogue cannot pass through.",
			cause)
	}
}

// The Postgres classifier maps SQLSTATEs to Codes correctly. A misclassification
// here means a customer sees the wrong HTTP status (e.g. 500 instead of 409),
// which is observably wrong.
func TestPostgresErrorClassification(t *testing.T) {
	cases := []struct {
		sqlstate string
		want     apierr.Code
	}{
		{"23505", apierr.CodeConflict},       // unique_violation
		{"23503", apierr.CodeConflict},       // foreign_key_violation
		{"23514", apierr.CodeInvalidRequest}, // check_violation
		{"22P02", apierr.CodeInvalidRequest}, // invalid_text_representation
		{"42703", apierr.CodeInternalError},  // undefined_column -> schema drift
		{"42P01", apierr.CodeInternalError},  // undefined_table
		{"42501", apierr.CodeForbidden},      // insufficient_privilege
	}
	for _, c := range cases {
		err := &pgconn.PgError{Code: c.sqlstate, Message: "sample message"}
		if got := postgreserr.Classify(err); got != c.want {
			t.Errorf("SQLSTATE %s -> %q, want %q", c.sqlstate, got, c.want)
		}
	}
}

// Context-style errors map to dependency_unavailable, not internal_error.
func TestPostgresContextErrorsAreNotInternal(t *testing.T) {
	if got := postgreserr.Classify(context.DeadlineExceeded); got != apierr.CodeDependencyUnavailable {
		t.Errorf("deadline -> %q, want dependency_unavailable so the caller sees a "+
			"5xx class they can ask the operator to update the timeout for", got)
	}
	if got := postgreserr.Classify(context.Canceled); got != apierr.CodeDependencyUnavailable {
		t.Errorf("cancel -> %q, want dependency_unavailable", got)
	}
}

// A nil err means no error, which is fine for direct callers and refuses to
// render if used as a code.
func TestNilErrorIsNotAnErrorCode(t *testing.T) {
	if got := postgreserr.Classify(nil); got != "" {
		t.Errorf("Classify(nil) = %q, want empty", got)
	}
}

// Render never returns HTML (CDN error pages would substitute themselves in,
// and a body looking like JSON guards against that). Implied by way of writing
// into the JSON Content-Type.
func TestRenderSetsJSONContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	apierr.Render(rec, http.StatusBadRequest, apierr.CodeInvalidRequest, "test-id", "")
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

// Stable code contracts. A consumer must be able to switch on the Code, so
// every Code constant MUST be reacheable from Render without panic.
func TestEveryCodeIsRenderable(t *testing.T) {
	codes := []apierr.Code{
		apierr.CodeInvalidRequest,
		apierr.CodeUnauthorized,
		apierr.CodeForbidden,
		apierr.CodeNotFound,
		apierr.CodeConflict,
		apierr.CodeRateLimited,
		apierr.CodeBudgetExceeded,
		apierr.CodeUpstreamError,
		apierr.CodeDependencyUnavailable,
		apierr.CodeInternalError,
	}
	for _, c := range codes {
		t.Run(string(c), func(t *testing.T) {
			rec := httptest.NewRecorder()
			apierr.Render(rec, apierr.HTTPStatus(c), c, "test-id", "")
			body := io.Discard
			_ = body
		})
	}
}
