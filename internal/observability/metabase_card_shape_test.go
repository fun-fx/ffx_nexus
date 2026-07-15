package observability

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEnsureCardDatasetQueryField verifies the POST /api/card payload
// shape that Metabase 0.50 expects:
//
//	"dataset_query": { "type": "native", "database": <id>, "native": {…} }
//
// Earlier code sent "query" instead of "dataset_query" which the server
// silently stripped on round-trip, surfacing
//
//	{"dataset_query":"값은 지도이어야 합니다.",
//	 "specific-errors":{"dataset_query":["missing required key",…]}}
//
// on every card create. The smoke verification was done in prod after
// PR #97 landed; this test pins the shape so a future refactor doesn't
// regress it.
func TestEnsureCardDatasetQueryField(t *testing.T) {
	capture := struct {
		called bool
		body   []byte
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/card" || r.Method != http.MethodPost {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		capture.called = true
		capture.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":33}`))
	}))
	defer srv.Close()

	mb := &MetabaseBootstrapper{
		cfg:    MetabaseConfig{URL: srv.URL},
		client: srv.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := mb.ensureCard(context.Background(), "session", 17, 7, "Daily requests", "line",
		map[string]any{"query": "SELECT 1"},
		map[string]any{"graph.line_marker": "dot"}); err != nil {
		t.Fatalf("ensureCard: %v", err)
	}
	if !capture.called {
		t.Fatalf("POST /api/card was never called")
	}
	var got map[string]any
	if err := json.Unmarshal(capture.body, &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, ok := got["query"]; ok {
		t.Errorf("regression: payload has old \"query\" field instead of \"dataset_query\": %s", capture.body)
	}
	dq, ok := got["dataset_query"].(map[string]any)
	if !ok {
		t.Fatalf("payload has no dataset_query map: %s", capture.body)
	}
	if dq["type"] != "native" {
		t.Errorf("dataset_query.type = %v, want \"native\"", dq["type"])
	}
	// json.Unmarshal into a map decodes numbers as float64, not int.
	if id, _ := dq["database"].(float64); id != 7 {
		t.Errorf("dataset_query.database = %v, want 7", dq["database"])
	}
	if dq["native"] == nil {
		t.Errorf("dataset_query.native is missing")
	}
	vs, ok := got["visualization_settings"].(map[string]any)
	if !ok {
		t.Errorf("visualization_settings not a map: %s", capture.body)
	}
	if vs["graph.line_marker"] != "dot" {
		t.Errorf("visualization.graph.line_marker = %v, want dot", vs["graph.line_marker"])
	}
}

// TestEnsureCardRecoveryPUT exercises the 400 → list → PUT recovery
// path so an upgrade that updates the bundled JSONs round-trips
// idempotently into the same card (instead of leaving stale dashboards).
func TestEnsureCardRecoveryPUT(t *testing.T) {
	var (
		gotPutBody []byte
		gotPutID   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/card" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":{"name":"already exists"}}`))
		case strings.HasPrefix(r.URL.Path, "/api/card") && r.Method == http.MethodGet:
			// Listing path: /api/card?collection_id=N
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":"33","name":"Daily requests"}]`))
		case strings.HasPrefix(r.URL.Path, "/api/card/") && r.Method == http.MethodPut:
			gotPutID = strings.TrimPrefix(r.URL.Path, "/api/card/")
			gotPutBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":33}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mb := &MetabaseBootstrapper{
		cfg:    MetabaseConfig{URL: srv.URL},
		client: srv.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := mb.ensureCard(context.Background(), "session", 17, 7, "Daily requests", "line",
		map[string]any{"query": "SELECT 1", "template-tags": map[string]any{}},
		map[string]any{"graph.line_marker": "dot"}); err != nil {
		t.Fatalf("ensureCard (recovery): %v", err)
	}
	if gotPutID != "33" {
		t.Errorf("PUT path id = %q, want 33", gotPutID)
	}
	var got map[string]any
	if err := json.Unmarshal(gotPutBody, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got["dataset_query"]; !ok {
		t.Errorf("PUT payload missing dataset_query: %s", gotPutBody)
	}
}

// TestEnsureCardEmptyVizStillObject pins the non-nil-map guard: an
// empty visualization map must serialize as {} and not as null. The
// Metabase validator rejects the latter.
func TestEnsureCardEmptyVizStillObject(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	mb := &MetabaseBootstrapper{
		cfg:    MetabaseConfig{URL: srv.URL},
		client: srv.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := mb.ensureCard(context.Background(), "session", 17, 7, "Test", "table",
		map[string]any{"query": "SELECT 1"}, nil); err != nil {
		t.Fatalf("ensureCard: %v", err)
	}
	if !strings.Contains(string(captured), `"visualization_settings":{}`) {
		t.Errorf("visualization_settings should be {}, got: %s", captured)
	}
}
