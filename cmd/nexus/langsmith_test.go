package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ffxnexus/nexus/internal/evaluators/external"
)

func langsmithTarget(endpoint string) external.Target {
	return external.Target{
		Endpoint: endpoint,
		Auth:     external.Credentials{Values: []string{"lsv2_langsmith_test_key"}},
	}
}

// LangSmith ingest rejects "Authorization: Bearer" — the API key
// must travel in `x-api-key`. Trace ingestion also enforces 2xx;
// anything else is a real failure, not a connectivity blip.
func TestLangsmithTransmitUsesXAPIKeyHeader(t *testing.T) {
	var gotPath, gotKey, gotCT string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotCT = r.Header.Get("Content-Type")
		// Refuse the old Bearer header explicitly — a regression
		// that re-introduces Authorization: Bearer would show up as
		// 401 against the live vendor, and the test mocks that
		// consequence.
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("use x-api-key"))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := langsmithTransmit(context.Background(), langsmithTarget(srv.URL),
		map[string]any{"input": "hi", "output": "hello"}); err != nil {
		t.Fatalf("langsmithTransmit: %v", err)
	}
	if gotPath != "/otel/v1/traces" {
		t.Errorf("path = %q, want /otel/v1/traces", gotPath)
	}
	if gotKey != "lsv2_langsmith_test_key" {
		t.Errorf("x-api-key = %q; want the resolved LangSmith key", gotKey)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}
}

// Non-2xx on the OTLP ingest path must surface as a clear failure,
// not a silent success with no rows in the vendor project.
func TestLangsmithTransmitReportsNonOKIngest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid api key"))
	}))
	defer srv.Close()

	err := langsmithTransmit(context.Background(), langsmithTarget(srv.URL),
		map[string]any{"input": "x", "output": "y"})
	if err == nil {
		t.Fatal("expected an error on a 401 ingest response")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("error should include status and snippet; got %q", err.Error())
	}
}

// The previous probe returned nil against a 401-only host. A
// connectivity check that ignores the HTTP status is exactly the
// false positive "Test" used to give operators.
func TestPingLangsmithRejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	err := PingLangsmith(context.Background(), srv.URL, "lsv2_anything")
	if err == nil {
		t.Fatal("expected an error against a 401 /api/v1/info response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error must surface the status code; got %q", err.Error())
	}
}

// A connected, key-bearing probe must succeed and must reach the
// vendor with the correct header. Without the header fix the
// vendor would 401 here, and the test guards that.
func TestPingLangsmithAttachesXAPIKeyAndPasses(t *testing.T) {
	var calls int32
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		gotKey = r.Header.Get("x-api-key")
		if gotKey == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("missing x-api-key"))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := PingLangsmith(context.Background(), srv.URL, "lsv2_real"); err != nil {
		t.Fatalf("PingLangsmith should pass with a key against a 200 host; got %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("probe did not hit the test server exactly once")
	}
	if gotKey != "lsv2_real" {
		t.Errorf("x-api-key = %q; want lsv2_real", gotKey)
	}
}

// The result message must tell the operator whether a key was
// actually carried — that is the difference between "connectivity"
// and "connectivity + auth verified", which was previously conflated.
func TestLangSmithMessageDistinguishesCarry(t *testing.T) {
	if got := langSmithMessage(nil, "lsv2_x"); !strings.Contains(got, "Auth accepted") {
		t.Errorf("with a key, message should say auth accepted; got %q", got)
	}
	if got := langSmithMessage(nil, ""); !strings.HasPrefix(got, "LangSmith endpoint reachable") {
		t.Errorf("without a key, message should clarify; got %q", got)
	}
	if got := langSmithMessage(io.EOF, ""); !strings.Contains(got, "EOF") {
		t.Errorf("error message must surface the underlying cause; got %q", got)
	}
}
