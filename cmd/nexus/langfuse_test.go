package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/evaluators/external"
	"github.com/ffxnexus/nexus/internal/observability"
)

func langfuseTarget(endpoint string) external.Target {
	return external.Target{
		Endpoint: endpoint,
		Auth:     external.Credentials{Values: []string{"pk-lf-test", "sk-lf-test"}},
		Trace: observability.Trace{
			TraceID:      "0123456789abcdef0123456789abcdef",
			SpanID:       "0123456789abcdef",
			RequestModel: "gpt-4o-mini",
			OrgID:        "default",
		},
	}
}

// The legacy /api/public/ingestion endpoint is deprecated and rejects
// trace-create on Langfuse v4, so the adapter must use the OTLP route.
func TestLangfuseTransmitUsesOTLPEndpointWithBasicAuth(t *testing.T) {
	var gotPath, gotUser, gotPass, gotCT string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUser, gotPass, _ = r.BasicAuth()
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := langfuseTransmit(context.Background(), langfuseTarget(srv.URL),
		map[string]any{"input": "hello", "output": "world"})
	if err != nil {
		t.Fatalf("langfuseTransmit: %v", err)
	}
	if gotPath != langfuseOTLPPath {
		t.Errorf("path = %q, want %q", gotPath, langfuseOTLPPath)
	}
	if gotUser != "pk-lf-test" || gotPass != "sk-lf-test" {
		t.Errorf("basic auth = (%q, %q); Langfuse needs public key as user, secret as password",
			gotUser, gotPass)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}
	if _, ok := body["resourceSpans"]; !ok {
		t.Fatalf("body is not an OTLP envelope: %v", body)
	}
}

// Content must travel as GenAI semconv attributes, otherwise Langfuse
// shows a span with no input/output and its evaluators have nothing to
// judge.
func TestLangfuseTransmitCarriesRedactedPayloadAsAttributes(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := langfuseTransmit(context.Background(), langfuseTarget(srv.URL), map[string]any{
		"input":  "[REDACTED] question",
		"output": "an answer",
		"env":    "prod",
	})
	if err != nil {
		t.Fatalf("langfuseTransmit: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"gen_ai.input.messages", "[REDACTED] question",
		"gen_ai.output.messages", "an answer",
		"nexus.plugin.env",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("envelope missing %q\n%s", want, got)
		}
	}
}

func TestLangfuseTransmitFailsClearlyWithoutKeyPair(t *testing.T) {
	tgt := langfuseTarget("https://cloud.langfuse.com")
	tgt.Auth = external.Credentials{Values: []string{"only-one"}}
	err := langfuseTransmit(context.Background(), tgt, map[string]any{"input": "x"})
	if err == nil {
		t.Fatal("expected an error when only one key is present")
	}
	if !strings.Contains(err.Error(), "keyRef") {
		t.Errorf("error should tell the operator how to fix it, got %q", err)
	}
}

// A rejected envelope must surface the vendor's explanation; a bare
// status code is what made this silently unfixable before.
func TestLangfuseTransmitSurfacesVendorErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"Failed to parse OTel JSON Trace"}`))
	}))
	defer srv.Close()

	err := langfuseTransmit(context.Background(), langfuseTarget(srv.URL),
		map[string]any{"input": "x"})
	if err == nil {
		t.Fatal("expected an error on 400")
	}
	if !strings.Contains(err.Error(), "Failed to parse OTel JSON Trace") {
		t.Errorf("error lost the vendor body: %q", err)
	}
}

// v2 scores is deprecated and 404s on v4; and v3 nests the trace id
// under `subject`, which Apply cannot walk into.
func TestLangfuseFetchReadsV3AndFlattensTraceID(t *testing.T) {
	var gotPath, gotFields string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotFields = r.URL.Query().Get("fields")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"name":"hallucination","value":0.25,"dataType":"NUMERIC",
			 "comment":"low risk","subject":{"kind":"observation","id":"obs-1","traceId":"trace-1"}},
			{"name":"helpful","value":true,"dataType":"BOOLEAN",
			 "subject":{"kind":"trace","id":"trace-2"}}
		]}`))
	}))
	defer srv.Close()

	out, err := langfuseFetch(context.Background(), langfuseTarget(srv.URL))
	if err != nil {
		t.Fatalf("langfuseFetch: %v", err)
	}
	if gotPath != langfuseScoresPath {
		t.Errorf("path = %q, want %q", gotPath, langfuseScoresPath)
	}
	if !strings.Contains(gotFields, "subject") || !strings.Contains(gotFields, "details") {
		t.Errorf("fields = %q, need both subject (trace id) and details (comment)", gotFields)
	}
	if len(out) != 2 {
		t.Fatalf("got %d scores, want 2", len(out))
	}

	var first map[string]any
	if err := json.Unmarshal(out[0], &first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if first["traceId"] != "trace-1" {
		t.Errorf("traceId = %v, want trace-1 lifted out of subject", first["traceId"])
	}
	if first["value"] != 0.25 {
		t.Errorf("value = %v", first["value"])
	}
	if first["comment"] != "low risk" {
		t.Errorf("comment = %v", first["comment"])
	}

	// A trace-level score carries its id as subject.id, not traceId.
	var second map[string]any
	if err := json.Unmarshal(out[1], &second); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if second["traceId"] != "trace-2" {
		t.Errorf("trace-level traceId = %v, want trace-2", second["traceId"])
	}
	if second["value"] != float64(1) || second["label"] != "pass" {
		t.Errorf("boolean score mapped to %v / %v, want 1 / pass",
			second["value"], second["label"])
	}
}

// Scores with no joinable trace would land as orphan rows.
func TestLangfuseFetchDropsScoresWithoutTraceID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"name":"session-level","value":1,"subject":{"kind":"session","id":"sess-1"}}
		]}`))
	}))
	defer srv.Close()

	out, err := langfuseFetch(context.Background(), langfuseTarget(srv.URL))
	if err != nil {
		t.Fatalf("langfuseFetch: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected session-scoped score to be dropped, got %v", out)
	}
}

func TestLangfuseFetchSurfacesAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Invalid credentials"}`))
	}))
	defer srv.Close()

	_, err := langfuseFetch(context.Background(), langfuseTarget(srv.URL))
	if err == nil {
		t.Fatal("expected an error on 401")
	}
	if !strings.Contains(err.Error(), "Invalid credentials") {
		t.Errorf("error lost the vendor body: %q", err)
	}
}
