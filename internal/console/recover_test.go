package console

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The console had no panic recovery, unlike the gateway. A nil dereference in a
// handler therefore closed the connection with no status line — curl reports
// "Empty reply from server" and a load balancer records a backend failure, so
// the crash reads as a network problem and the stack trace is only in pod logs.
//
// A replayed invite token was exactly this bug in production code, which is why
// these tests exist alongside the invite fix rather than as generic hygiene.
func TestRecoverPanicsReturns500(t *testing.T) {
	srv := newTestServer()

	h := srv.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		var p *struct{ X int }
		_ = p.X // nil dereference, the same shape as the invite bug
	}))

	rec := httptest.NewRecorder()
	// Must not propagate: an escaping panic is what net/http turns into a
	// dropped connection.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic escaped the middleware: %v", r)
			}
		}()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/boom", nil))
	}()

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}

	var body struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	bodyBytes := rec.Body.Bytes()
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("response is not apierr.Body JSON (%q): %v", rec.Body.String(), err)
	}
	if body.Error.RequestID == "" {
		t.Errorf("no request_id in body %q; resp.HTTP must stamp it on the recovered-panic response",
			string(bodyBytes))
	}
	if body.Error.Code == "" {
		t.Errorf("no apierr.Code in body %q; panic recovery bypassed the contract", string(bodyBytes))
	}

	// The panic value and stack belong in the log, not in a response handed to
	// whoever triggered the crash.
	for _, leak := range []string{"nil pointer", "goroutine", "runtime error", ".go:"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("response body leaks crash internals (%q): %s", leak, rec.Body.String())
		}
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("X-Request-Id not stamped on the recovered-panic response; " +
			"a customer that pastes the response into a support report cannot " +
			"join it to the server log line")
	}
}

// ErrAbortHandler is net/http's documented way of saying "the client is gone,
// abandon this response". Swallowing it would suppress a signal net/http needs.
func TestRecoverPanicsRepropagatesErrAbortHandler(t *testing.T) {
	srv := newTestServer()
	h := srv.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	var got any
	func() {
		defer func() { got = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil))
	}()

	if got != http.ErrAbortHandler {
		t.Fatalf("ErrAbortHandler must be re-panicked for net/http, got %v", got)
	}
}

// The middleware must be mounted, not merely defined: recoverPanics can be
// perfect and still useless if Mux() forgets it. Rather than adding a
// panic-on-demand route to production code, this runs a panicking handler
// through the real middleware chain that Mux() assembled.
func TestMuxInstallsPanicRecovery(t *testing.T) {
	mux, ok := NewServer(nil, nil, nil, slog.Default()).Mux().(*chi.Mux)
	if !ok {
		t.Fatal("Mux() no longer returns *chi.Mux; update this test's seam")
	}

	h := mux.Middlewares().Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler exploded")
	}))

	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Mux() does not install recoverPanics: panic escaped (%v)", r)
			}
		}()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/anything", nil))
	}()

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 from the recovered panic, got %d", rec.Code)
	}
}

// A request id supplied by the customer's ingress is echoed back so one
// identifier spans their logs and ours, but it is attacker-controlled input on
// its way into a log line and a JSON body.
func TestRequestIDFromSanitizesCallerSuppliedHeader(t *testing.T) {
	tests := []struct {
		name, header, wantNot string
	}{
		{"log injection via newline", "abc\ndef level=INFO msg=\"forged\"", "\n"},
		{"log injection via carriage return", "abc\r\nrogue", "\r"},
		{"quote breaking out of a JSON string", `abc"def`, `"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
			r.Header.Set("X-Request-Id", tc.header)
			got := requestIDFrom(r)
			if strings.Contains(got, tc.wantNot) {
				t.Errorf("sanitised id still contains %q: %q", tc.wantNot, got)
			}
			if got == "" {
				t.Error("sanitising must not yield an empty id")
			}
		})
	}

	t.Run("bounded length", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
		r.Header.Set("X-Request-Id", strings.Repeat("a", 5000))
		if got := requestIDFrom(r); len(got) > 64 {
			t.Errorf("unbounded request id: %d chars", len(got))
		}
	})

	t.Run("absent header still yields an id", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
		if requestIDFrom(r) == "" {
			t.Error("want a generated id when the caller supplied none")
		}
	})
}
