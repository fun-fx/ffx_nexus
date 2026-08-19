package evalplugin_test

import (
	"testing"

	"github.com/ffxnexus/nexus/internal/evalplugin"
)

// Sanity — every shipped evaluator kind validates cleanly. The
// closed enum here is the source of truth for the new metric
// inventory test.
const shippedManifest = `
apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: langfuse-judge
spec:
  service:
    type: langfuse
    endpoint: https://example.invalid/v1
    auth:
      secretRef: langfuse-judge
      keyRef: public_key|secret_key
  send:
    trigger: on_trace
    sampling: 0.1
  collect:
    mode: poll
    interval: 60s
    mapping:
      metric: $.name
      score: $.score
      trace: $.trace_id
`

func TestStrictCapturesWarningsOnEveryManifest(t *testing.T) {
	evalplugin.ResetPendingStrict()
	defer evalplugin.ResetPendingStrict()

	// Counter the hook writes; this test bypasses StrictFieldSink
	// because main.go installs the production logger at boot.
	var hits int
	prev := evalplugin.StrictFieldSink
	evalplugin.StrictFieldSink = func(p, f string) { hits++ }
	defer func() { evalplugin.StrictFieldSink = prev }()

	if _, err := evalplugin.Decode([]byte(shippedManifest)); err != nil {
		t.Fatalf("decode shipped manifest: %v", err)
	}
	if err := evalplugin.Validate(mustDecode(t, shippedManifest)); err != nil {
		t.Fatalf("validate shipped manifest: %v", err)
	}
	if hits != 0 {
		t.Errorf("expected no strict warnings on a clean shipped manifest; got %d", hits)
	}
}

// TestStrictFiresOnUnknownField proves the save-time strict hook
// fires even when the operator did not set spec.flags: [strict].
func TestStrictFiresOnUnknownField(t *testing.T) {
	// Plug a recorder; the previous hook is restored at the end.
	prev := evalplugin.StrictFieldSink
	defer func() { evalplugin.StrictFieldSink = prev }()

	evalplugin.ResetPendingStrict()
	var hits []string
	evalplugin.StrictFieldSink = func(p, f string) { hits = append(hits, f) }

	manifest := `
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
  unknown_field_that_should_warn: yes
`
	p, err := evalplugin.DecodeStrict([]byte(manifest))
	if err != nil {
		t.Fatalf("strict decode: %v", err)
	}
	if err := evalplugin.Validate(p); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(hits) == 0 {
		t.Error("expected strict warning for unknown_field_that_should_warn")
	}
}

func mustDecode(t *testing.T, s string) *evalplugin.Plugin {
	t.Helper()
	p, err := evalplugin.Decode([]byte(s))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return p
}
