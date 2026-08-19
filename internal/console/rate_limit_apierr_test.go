package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/apierr"
	"github.com/ffxnexus/nexus/internal/limiter"
)

// ipRateLimit's response MUST go through the apierr contract; the body
// shape MUST be { error: { code, message, request_id } } and the
// X-Request-Id header MUST be stamped. The previous shape was a
// hand-rolled `{ error: "rate limit exceeded; try again later" }` map,
// which the leak / drift detection would have flagged as bypass.
func TestIPRateLimitGoesThroughApierrContract(t *testing.T) {
	srv := newTestServer()
	// Saturate the limiter so the next call drops.
	l := limiter.NewIPLimiter(1, time.Minute)
	l.Allow("rl-test:1.1.1.1")
	// We expect a 429 here:
	h := srv.ipRateLimit("rl-test", l)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler should NOT run when ipRateLimit pre-empts")
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "1.1.1.1:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After so a customer SDK backs off")
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Errorf("X-Request-Id must be stamped on the rate-limit response; "+
			"body was %q", rec.Body.String())
	}
	// Verify the body is apierr.Body shape with code=rate_limited.
	var body struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %q (%v)", rec.Body.String(), err)
	}
	if body.Error.Code != string(apierr.CodeRateLimited) {
		t.Errorf("body.code = %q, want %q (a customer SDK branches on this constant)",
			body.Error.Code, apierr.CodeRateLimited)
	}
	if body.Error.RequestID == "" {
		t.Errorf("request_id must be present so an operator can grep the log; "+
			"the panic-recovery path proved otherwise without resp.HTTP; full body %q",
			rec.Body.String())
	}
	// Verify over-redaction: the surrounding text "rate limit" should be
	// the message, not lifted from a SQL fragment.
	if body.Error.Message == "" {
		t.Error("message must be human-readable")
	}
	// Verify the response shape is literally not the old { error: "..." } map.
	// This trips when someone hand-rolls a `writeJSON(...map[string]string{"error":"..."})`
	// and we want a clear assertion in test output.
	rawMap := map[string]string{}
	if err := json.Unmarshal(rec.Body.Bytes(), &rawMap); err == nil && rawMap["error"] != "" {
		t.Errorf("response body is the legacy hand-rolled shape `{\"error\": \"...\"}`: %q. "+
			"This is the regression class the apierr bypass detector flags; the rate-limit "+
			"middleware MUST go through resp.HTTP.", rec.Body.String())
	}
}

// silent
var _ = apierr.CodeInternalError
