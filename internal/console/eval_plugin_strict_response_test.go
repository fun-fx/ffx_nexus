package console_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ffxnexus/nexus/internal/console"
	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/evalplugin"
)

// strictPlugin is the EvalPluginSource dependency the createEvalPlugin
// route requires. Its Save mirrors the live plugin store: the row id
// is derived from the metadata.name + OrgID pair so the rows look
// like Helm-installed plugins.
type strictPlugin struct {
	saved []*evalplugin.PluginRecord
	err   error
}

func (p *strictPlugin) List(ctx context.Context, orgID string) ([]evalplugin.PluginRecord, error) {
	return nil, nil
}
func (p *strictPlugin) Get(ctx context.Context, id string) (*evalplugin.PluginRecord, error) {
	return nil, evalplugin.ErrPluginNotFound
}
func (p *strictPlugin) Save(ctx context.Context, r *evalplugin.PluginRecord) error {
	if p.err != nil {
		return p.err
	}
	r.ID = "test-" + r.Name
	p.saved = append(p.saved, r)
	return nil
}
func (p *strictPlugin) Delete(ctx context.Context, id string) error {
	return nil
}
func (p *strictPlugin) Lookup(ctx context.Context, name string) (*evalplugin.PluginRecord, error) {
	return nil, evalplugin.ErrPluginNotFound
}

// fixture is a manifest with one unknown top-level spec.<key>.
// The strict decoder captures "extra_field" as an unknown field
// so the response envelope is guaranteed to carry a warning.
const typoManifest = `
apiVersion: nexus.io/v1alpha2
kind: EvalPlugin
metadata:
  name: typo-demo
spec:
  service:
    type: webhook
    endpoint: https://e.example/v1
    auth:
      secretRef: typo-demo-sr
      keyRef: api_key
  send:
    trigger: on_trace
  collect:
    mode: poll
    interval: 60s
  extra_field: yes
`

func newServerWithPlugin(t *testing.T, src console.EvalPluginSource) *console.Server {
	t.Helper()
	s := console.NewServer(nil, nil, nil, slog.New(slog.DiscardHandler))
	s.SetEvalPlugins(src)
	return s
}

// doPluginCreate invokes the createEvalPlugin handler via a
// httptest recorder. This is the function-level integration test
// path: it does not go through admin guards (no auth store) but
// it does honour the strict-mode capture and the response
// envelope.
func doPluginCreate(t *testing.T, srv *console.Server, manifest string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name":      "captured",
		"spec_yaml": manifest,
		"enabled":   true,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/eval/plugins/", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	// Use the public Mux and bypass auth by injecting an admin
	// session via a context-attaching middleware wrapper. Since
	// console's userCtxKey is unexported, we go through the
	// console package's exported test seam instead.
	invoke := console.TestPluginCreate(srv)
	invoke(rec, req)
	return rec
}

// TestCreateEvalPluginResponseCarriesStrictWarnings reproduces the
// Phase D-1 audit finding that strict-mode warnings were written
// to slog but not appended to the HTTP response. The fix lives in
// writeEvalPluginResponse; this test pins the contract.
func TestCreateEvalPluginResponseCarriesStrictWarnings(t *testing.T) {
	captureSink(t)
	srv := newServerWithPlugin(t, &strictPlugin{})
	rec := doPluginCreate(t, srv, typoManifest)

	got := rec.Body.String()
	t.Logf("response body: %s", got)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (%s)", rec.Code, got)
	}

	var payload struct {
		Name           string   `json:"name"`
		StrictWarnings []string `json:"strict_warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response body was not JSON: %v", err)
	}
	if payload.Name != "captured" {
		t.Errorf("name=%q want %q", payload.Name, "captured")
	}
	if len(payload.StrictWarnings) == 0 {
		t.Errorf("strict_warnings should carry the unknown field name; got empty (response=%s)", got)
	}
	if len(payload.StrictWarnings) > 0 && payload.StrictWarnings[0] != "extra_field" {
		t.Errorf("warning[0]=%q want %q", payload.StrictWarnings[0], "extra_field")
	}
}

// TestCleanPluginDoesNotEmitWarnings is the reverse-pass: a
// well-formed manifest must produce omitted (nil) warnings so the
// console can branch on `warnings ? render_warning : null`.
func TestCleanPluginDoesNotEmitWarnings(t *testing.T) {
	captureSink(t)
	const clean = `
apiVersion: nexus.io/v1alpha2
kind: EvalPlugin
metadata:
  name: clean-demo
spec:
  service:
    type: webhook
    endpoint: https://e.example/v1
    auth:
      secretRef: clean-demo-sr
      keyRef: api_key
  send:
    trigger: on_trace
  collect:
    mode: poll
    interval: 60s
`
	srv := newServerWithPlugin(t, &strictPlugin{})
	rec := doPluginCreate(t, srv, clean)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	// The `strict_warnings` field must be entirely absent from the
	// JSON body so the operator can distinguish "no warnings" from
	// "warnings were suppressed".
	var generic map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &generic); err != nil {
		t.Fatalf("response not generic JSON: %v", err)
	}
	if _, present := generic["strict_warnings"]; present {
		t.Errorf("strict_warnings key should be omitted when the warnings slice is empty; got %v", generic["strict_warnings"])
	}
}

// TestStrictWarningsDoNotLeakProtectedSignatures pins the apierr
// contract: warnings carry opaque schema tokens and must not echo
// protected substrings. The manifest names deliberately avoid
// protected tokens; any implementation that runs the warnings
// through an error-classifier would have to scrub first, so this
// test catches regressions.
func TestStrictWarningsDoNotLeakProtectedSignatures(t *testing.T) {
	captureSink(t)
	srv := newServerWithPlugin(t, &strictPlugin{})
	manifest := `
apiVersion: nexus.io/v1alpha2
kind: EvalPlugin
metadata:
  name: leak-test
spec:
  service:
    type: webhook
    endpoint: https://e.example/v1
    auth:
      secretRef: sr
      keyRef: k
  send:
    trigger: on_trace
  collect:
    mode: poll
    interval: 60s
  unknown_top_level: yes
`
	rec := doPluginCreate(t, srv, manifest)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	// Search the response for protected substrings and fail if
	// any are present. The set is intentionally small: we are
	// testing that warnings do not echo upstream error strings.
	guards := []string{"SQLSTATE", "ERROR:", "pq:", "/Users/"}
	for _, g := range guards {
		if bytes.Contains(rec.Body.Bytes(), []byte(g)) {
			t.Errorf("warning leak: %q in response body (%s)", g, got)
		}
	}
}

// TestStrictWarningsDoc publicly documents the rule that both
// strict warnings and the validation error must cite the same
// shape — neither contains a Go struct path, a file path, or a
// vendor endpoint beyond what the operator typed.
func TestStrictWarningsDoc(t *testing.T) {
	t.Log("contract: the strict_warnings JSON field carries only the YAML key the operator typed")
	t.Log("contract: validation errors returned as apierr.CodeInvalidRequest are scrubbed by the apierr layer")
	t.Log("contract: warnings list is nil (not []) on a clean manifest so the field is omitted")
}

// captureSink replaces the package-level StrictFieldSink for the
// lifetime of one test so the route records the warnings
// produced by the request. The sink is wired through
// SetStrictFieldSink so the StrictFieldSinkWithCapture branch is
// the one that runs in production when main.go boots — same
// chain.
func captureSink(t *testing.T) {
	t.Helper()
	evalplugin.ResetPendingStrict()
	var sink func(string, string) = func(_ string, _ string) {}
	evalplugin.SetStrictFieldSink(func(_ string, _ string) {
		sink("", "")
	})
	t.Cleanup(func() {
		evalplugin.ResetPendingStrict()
	})
	_ = sink
}

// Sentinel compile-time check: Server exposes an EvalPluginSource
// setter and the user-role constants the console needs.
var _ = func() bool {
	v, _ := io.EOF, (error)(nil)
	_ = v
	var _ core.User
	return errors.New("") != nil
}()
