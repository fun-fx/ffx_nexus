package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ffxnexus/nexus/internal/evaluators/external"
)

// Confident AI — earlier releases passed this through genericProbe,
// so test 1 below pins the new shape: a real probe that attaches
// Basic auth and rejects 401 (not the previous "endpoint reachable"
// pass).

func confidentAITarget(endpoint string) external.Target {
	return external.Target{
		Endpoint: endpoint,
		Auth:     external.Credentials{Values: []string{"cid", "csec"}},
	}
}

func TestPingConfidentAI_AttachesBasicAndPasses(t *testing.T) {
	var calls int32
	var gotUser, gotPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		gotUser, gotPass, _ = r.BasicAuth()
		if gotUser == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("missing basic"))
			return
		}
		if r.URL.Path != "/v1/projects" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := PingConfidentAI(context.Background(), srv.URL, "cid", "csec"); err != nil {
		t.Fatalf("PingConfidentAI should pass with a valid pair; got %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("probe did not exactly one call")
	}
	if gotUser != "cid" || gotPass != "csec" {
		t.Errorf("Basic creds = %q/%q; want cid/csec", gotUser, gotPass)
	}
}

func TestPingConfidentAI_RejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("org membership required"))
	}))
	defer srv.Close()
	err := PingConfidentAI(context.Background(), srv.URL, "cid", "csec")
	if err == nil {
		t.Fatal("expected an error against a 403 /v1/projects response")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "org membership") {
		t.Errorf("error must surface status and snippet; got %q", err.Error())
	}
}

func TestPingConfidentAI_AttachesBearerWhenSingleKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if gotAuth == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := PingConfidentAI(context.Background(), srv.URL, "ckey", ""); err != nil {
		t.Fatalf("expect ok; got %v", err)
	}
	if gotAuth != "Bearer ckey" {
		t.Errorf("Authorization = %q; want Bearer ckey", gotAuth)
	}
}

// Braintrust — `/v1/projects` accepts Bearer only. Same probe shape
// as LangSmith's `x-api-key`, but a Bearer header so we can confirm
// Braintrust's authoring convention.

func TestPingBraintrust_AttachesBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/projects" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := PingBraintrust(context.Background(), srv.URL, "bt-key"); err != nil {
		t.Fatalf("PingBraintrust should pass; got %v", err)
	}
	if gotAuth != "Bearer bt-key" {
		t.Errorf("Authorization = %q; want Bearer bt-key", gotAuth)
	}
}

func TestPingBraintrust_Rejects401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("expired"))
	}))
	defer srv.Close()
	err := PingBraintrust(context.Background(), srv.URL, "bt-key")
	if err == nil {
		t.Fatal("expected an error against a 401 /v1/projects response")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "expired") {
		t.Errorf("error must mention status and snippet; got %q", err.Error())
	}
}

// Arize Phoenix — `/v1/traces` is the OTLP path. Phoenix returns 405
// Method Not Allowed for GETs to OTLP, which is the cheapest signal
// the endpoint is alive. Hosted Phoenix additionally rejects without
// a credential; self-host accepts the unauthenticated case.

func TestPingArizePhoenix_NoAuthAccepts405(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer srv.Close()
	// Both primary and secondary empty => self-host unauth case.
	if err := PingArizePhoenix(context.Background(), srv.URL, "", ""); err != nil {
		t.Fatalf("self-host Phoenix unauth path should accept 405; got %v", err)
	}
}

func TestPingArizePhoenix_Hosted_AttachesBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer srv.Close()
	if err := PingArizePhoenix(context.Background(), srv.URL, "phx-key", ""); err != nil {
		t.Fatalf("hosted Phoenix should accept 405 too; got %v", err)
	}
	if gotAuth != "Bearer phx-key" {
		t.Errorf("Authorization = %q; want Bearer phx-key", gotAuth)
	}
}

func TestPingArizePhoenix_Rejects401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("api key rejected"))
	}))
	defer srv.Close()
	err := PingArizePhoenix(context.Background(), srv.URL, "phx-key", "")
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error must surface status; got %q", err.Error())
	}
}

// Datadog — `/api/v1/validate` confirms the resolved DD-API-KEY.
// Earlier Releases ran Datadog through genericProbe (no auth), which
// returned "endpoint reachable" against a 403-only host.

func TestPingDatadog_AttachesAPIKey(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("DD-API-KEY")
		if r.URL.Path != "/api/v1/validate" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := PingDatadog(context.Background(), srv.URL, "dd-key"); err != nil {
		t.Fatalf("should pass; got %v", err)
	}
	if gotKey != "dd-key" {
		t.Errorf("DD-API-KEY = %q; want dd-key", gotKey)
	}
}

func TestPingDatadog_Rejects403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("key disabled"))
	}))
	defer srv.Close()
	err := PingDatadog(context.Background(), srv.URL, "dd-key")
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("error must surface status and snippet; got %q", err.Error())
	}
}

// Message helpers must distinguish "Auth accepted (with key)" from
// "endpoint reachable (no key attached)" — the operator was burned
// by the old conflation.

func TestConfidentAIMessage_DistinguishesCarry(t *testing.T) {
	if got := confidentAIMessage(nil, confidentPair{primary: "a", hasAny: true}); !strings.Contains(got, "authenticated") {
		t.Errorf("with key, message should say authenticated; got %q", got)
	}
	if got := confidentAIMessage(nil, confidentPair{}); !strings.HasPrefix(got, "Confident AI endpoint reachable") {
		t.Errorf("without key, message should clarify; got %q", got)
	}
}

func TestBraintrustMessage_DistinguishesCarry(t *testing.T) {
	if got := braintrustMessage(nil, "bk"); !strings.Contains(got, "Braintrust authenticated") {
		t.Errorf("with key, message should say authenticated; got %q", got)
	}
	if got := braintrustMessage(nil, ""); !strings.HasPrefix(got, "Braintrust endpoint reachable") {
		t.Errorf("without key, message should clarify; got %q", got)
	}
	if got := braintrustMessage(context.Canceled, ""); !strings.Contains(got, "context canceled") {
		t.Errorf("error message must surface the cause; got %q", got)
	}
}

func TestArizePhoenixMessage_DistinguishesAuthStates(t *testing.T) {
	if got := arizePhoenixMessage(nil, confidentPair{primary: "k", hasAny: true}); !strings.Contains(got, "authenticated") {
		t.Errorf("with key, message should say authenticated; got %q", got)
	}
	if got := arizePhoenixMessage(nil, confidentPair{}); !strings.Contains(got, "no auth configured") {
		t.Errorf("without auth, message should clarify; got %q", got)
	}
}

func TestDatadogMessage_DistinguishesCarry(t *testing.T) {
	if got := datadogMessage(nil, "dk"); !strings.Contains(got, "Datadog authenticated") {
		t.Errorf("with key, message should say authenticated; got %q", got)
	}
	if got := datadogMessage(nil, ""); !strings.HasPrefix(got, "Datadog endpoint reachable") {
		t.Errorf("without key, message should clarify; got %q", got)
	}
}
