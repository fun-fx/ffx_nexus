package console

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/evalplugin"
)

// stubKeys implements EvalPluginKeys for tests. It mirrors the
// cmd/nexus/consoleKeyResolver shape (sync + map) so a real
// resolver can drop in.
type stubKeys struct {
	mu   sync.Mutex
	data map[string]map[string]string
}

func newStubKeys() *stubKeys { return &stubKeys{data: make(map[string]map[string]string)} }

func (s *stubKeys) Get(plugin string) (map[string]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src, ok := s.data[plugin]
	if !ok || len(src) == 0 {
		return nil, false
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out, true
}

func (s *stubKeys) Set(plugin string, kv map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := make(map[string]string, len(kv))
	for k, v := range kv {
		if v != "" {
			c[k] = v
		}
	}
	if len(c) == 0 {
		delete(s.data, plugin)
		return
	}
	s.data[plugin] = c
}

func (s *stubKeys) Clear(plugin string)                                          { s.Set(plugin, nil) }
func (s *stubKeys) Has(plugin string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	src, ok := s.data[plugin]
	return ok && len(src) > 0
}

// stubPluginSource is a tiny EvalPluginSource that returns a single
// fixed plugin row keyed by name. Tests rely on the row id being the
// same as the name so the regress suite matches URL path inputs.
type stubPluginSource struct {
	rows map[string]*evalplugin.PluginRecord
}

type stubConfigForKeys struct {
	src  *stubPluginSource
	keys *stubKeys
}

func newStubPluginSource() *stubPluginSource {
	return &stubPluginSource{rows: make(map[string]*evalplugin.PluginRecord)}
}

func (s *stubPluginSource) add(rec *evalplugin.PluginRecord) {
	cp := *rec
	s.rows[rec.Name] = &cp
}

func (s *stubPluginSource) List(_ context.Context, _ string) ([]evalplugin.PluginRecord, error) {
	out := make([]evalplugin.PluginRecord, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, *r)
	}
	return out, nil
}

func (s *stubPluginSource) Get(_ context.Context, id string) (*evalplugin.PluginRecord, error) {
	for _, r := range s.rows {
		if r.ID == id {
			cp := *r
			return &cp, nil
		}
	}
	return nil, evalplugin.ErrPluginNotFound
}

func (s *stubPluginSource) Save(_ context.Context, r *evalplugin.PluginRecord) error {
	if r.ID == "" {
		r.ID = "id-" + r.Name
	}
	cp := *r
	s.rows[r.Name] = &cp
	return nil
}

func (s *stubPluginSource) Delete(_ context.Context, id string) error {
	for k, r := range s.rows {
		if r.ID == id {
			delete(s.rows, k)
			return nil
		}
	}
	return nil
}

func (s *stubPluginSource) Lookup(_ context.Context, name string) (*evalplugin.PluginRecord, error) {
	r, ok := s.rows[name]
	if !ok {
		return nil, evalplugin.ErrPluginNotFound
	}
	cp := *r
	return &cp, nil
}

// newKeysTestMux wires the keys endpoints under /api/eval/plugins/*
// for tests. It mirrors the production path block in server.go.
func newKeysTestMux(s *Server) http.Handler {
	r := chi.NewRouter()
	if s.pluginKeys != nil {
		r.Get("/api/eval/plugins/{name}/keys", func(w http.ResponseWriter, r *http.Request) {
			s.getPluginKeys(w, r, core.User{ID: "u1", Role: core.RoleAdmin})
		})
		r.Put("/api/eval/plugins/{name}/keys", func(w http.ResponseWriter, r *http.Request) {
			s.putPluginKeys(w, r, core.User{ID: "u1", Role: core.RoleAdmin})
		})
		r.Delete("/api/eval/plugins/{name}/keys", func(w http.ResponseWriter, r *http.Request) {
			s.deletePluginKeys(w, r, core.User{ID: "u1", Role: core.RoleAdmin})
		})
	}
	r.Get("/api/eval/plugins/{name}/keys-unwired", func(w http.ResponseWriter, r *http.Request) {
		s.getPluginKeys(w, r, core.User{ID: "u1", Role: core.RoleAdmin})
	})
	return r
}

// newKeysTestServer wires the keys route with a fresh stub source and
// stub resolver. Returns the mime mux alongside.
func newKeysTestServer(t *testing.T, name string, manifest string) (*Server, http.Handler, *stubKeys) {
	t.Helper()
	srv := newTestServer()
	src := newStubPluginSource()
	srv.SetEvalPlugins(src)
	src.add(&evalplugin.PluginRecord{
		OrgID:    "",
		Name:     name,
		SpecYAML: manifest,
		Enabled:  true,
	})
	keys := newStubKeys()
	srv.SetPluginKeys(keys)
	return srv, newKeysTestMux(srv), keys
}

const langfuseManifest = `apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: langfuse-judge
spec:
  service:
    type: langfuse
    endpoint: https://cloud.langfuse.com
    auth:
      secretRef: langfuse-judge
      keyRef: public_key|secret_key
  send: { trigger: on_trace, sampling: 0.5, payload: { input: "{{.input}}" }, redact: [pii] }
  collect: { mode: poll, interval: 60s, mapping: { metric: "$.name", score: "$.score", trace: "$.trace_id" } }
`

func TestGetPluginKeys_EmptyPluginReportsNames(t *testing.T) {
	srv, mux, _ := newKeysTestServer(t, "langfuse-judge", langfuseManifest)
	_ = srv
	req := httptest.NewRequest(http.MethodGet, "/api/eval/plugins/langfuse-judge/keys", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Plugin     string `json:"plugin"`
		Configured bool   `json:"configured"`
		Required   []string  `json:"required_key_names"`
		Keys       []pluginKeysEntry `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Plugin != "langfuse-judge" {
		t.Errorf("plugin name mismatch: %q", out.Plugin)
	}
	if out.Configured {
		t.Errorf("empty plugin should not be configured")
	}
	wantNames := []string{"public_key", "secret_key"}
	if !equalStrings(out.Required, wantNames) {
		t.Errorf("required names mismatch: got %v want %v", out.Required, wantNames)
	}
	if !equalStrings2(out.Keys, wantNames) {
		t.Errorf("keys shape mismatch: got %+v", out.Keys)
	}
	for _, k := range out.Keys {
		if k.Set {
			t.Errorf("every key should report set=false on empty plugin: %+v", k)
		}
	}
}

func TestPutAndGetPluginKeys_RoundTrip(t *testing.T) {
	_, mux, _ := newKeysTestServer(t, "langfuse-judge", langfuseManifest)

	body := bytes.NewReader([]byte(`{"keys":{"public_key":"pk-lf-abc","secret_key":"sk-lf-def"}}`))
	putReq := httptest.NewRequest(http.MethodPut, "/api/eval/plugins/langfuse-judge/keys", body)
	putRec := httptest.NewRecorder()
	mux.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT want 200, got %d (%s)", putRec.Code, putRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/eval/plugins/langfuse-judge/keys", nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET want 200, got %d", getRec.Code)
	}
	var out struct {
		Configured bool `json:"configured"`
		Keys       []pluginKeysEntry `json:"keys"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Configured {
		t.Errorf("expected configured=true after PUT")
	}
	got := map[string]bool{}
	for _, k := range out.Keys {
		got[k.Name] = k.Set
	}
	if !got["public_key"] || !got["secret_key"] {
		t.Errorf("keys should report set=true: %+v", got)
	}
}

func TestDeletePluginKeys_ClearsState(t *testing.T) {
	_, mux, keys := newKeysTestServer(t, "langfuse-judge", langfuseManifest)
	keys.Set("langfuse-judge", map[string]string{"public_key": "pk", "secret_key": "sk"})

	req := httptest.NewRequest(http.MethodDelete, "/api/eval/plugins/langfuse-judge/keys", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if keys.Has("langfuse-judge") {
		t.Errorf("expected resolver to be cleared")
	}
}

func TestGetPluginKeys_NotWiredReturns503(t *testing.T) {
	srv := newTestServer()
	mux := newKeysTestMux(srv)
	// pluginKeys intentionally not set.
	req := httptest.NewRequest(http.MethodGet, "/api/eval/plugins/langfuse-judge/keys-unwired", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not wired") {
		t.Errorf("body should mention wiring state, got %q", rec.Body.String())
	}
}

func TestGetPluginKeys_PluginNotFound(t *testing.T) {
	srv := newTestServer()
	srv.SetEvalPlugins(newStubPluginSource())
	srv.SetPluginKeys(newStubKeys())
	mux := newKeysTestMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/api/eval/plugins/ghost/keys", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

func TestPutPluginKeys_NoKeysReturns400(t *testing.T) {
	_, mux, _ := newKeysTestServer(t, "langfuse-judge", langfuseManifest)
	body := bytes.NewReader([]byte(`{"keys":{}}`))
	req := httptest.NewRequest(http.MethodPut, "/api/eval/plugins/langfuse-judge/keys", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for empty keys, got %d", rec.Code)
	}
}

func TestPutPluginKeys_MalformedJSONReturns400(t *testing.T) {
	_, mux, _ := newKeysTestServer(t, "langfuse-judge", langfuseManifest)
	body := bytes.NewReader([]byte(`not json`))
	req := httptest.NewRequest(http.MethodPut, "/api/eval/plugins/langfuse-judge/keys", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for malformed JSON, got %d", rec.Code)
	}
}

func TestSplitKeyRef(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"public_key", []string{"public_key"}},
		{"  public_key  |  secret_key ", []string{"public_key", "secret_key"}},
		{"a||b", []string{"a", "b"}},
		{"|", nil},
	}
	for _, c := range cases {
		got := splitKeyRef(c.in)
		if !equalStrings(got, c.want) {
			t.Errorf("splitKeyRef(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRequiredKeyNames_NilSafe(t *testing.T) {
	got := requiredKeyNames(nil)
	if got != nil {
		t.Errorf("nil → nil; got %v", got)
	}
}

func TestStripEmpty(t *testing.T) {
	got := stripEmpty(map[string]string{"a": "1", "b": "", "c": "3"})
	if len(got) != 2 || got["a"] != "1" || got["c"] != "3" {
		t.Errorf("stripEmpty leaked empty string: %+v", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStrings2(es []pluginKeysEntry, names []string) bool {
	if len(es) != len(names) {
		return false
	}
	for i := range es {
		if es[i].Name != names[i] {
			return false
		}
	}
	return true
}

// pin symbols so unused-import lints do not flap
var _ = io.EOF
var _ = bytes.NewReader
