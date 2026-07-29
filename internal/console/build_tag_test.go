package console

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWriteJSONResponseHeaders verifies that every JSON response
// the dashboard API sends carries the X-Nexus-Source and
// X-Nexus-Build markers. Operators inspecting a misbehaving
// 502 page rely on these to decide whether the body that reached
// their browser came from Nexus itself or from an upstream CDN,
// ingress WAF, or auth proxy.
//
// The pinned package-level responseBuildTag ("dev" without a
// SetBuildTag call) must be reflected verbatim so a missing
// header or stale "dev" tag is immediately obvious from the
// browser devtools.
func TestWriteJSONResponseHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]any{"ok": true})

	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := rec.Header().Get("X-Nexus-Source"); got != "nexus-console" {
		t.Fatalf("X-Nexus-Source = %q, want nexus-console", got)
	}
	if got := rec.Header().Get("X-Nexus-Build"); got != responseBuildTag {
		t.Fatalf("X-Nexus-Build = %q, want %q", got, responseBuildTag)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// TestSetBuildTagOverridesResponseTag ensures callers (notably
// cmd/nexus/main.go, which sets the tag from the linker-injected
// build identity) can swap in a non-default value before any
// response is rendered. Without this the body and the header
// would disagree, and operators would lose the ability to tell
// which binary is in front of them just by looking at network
// traffic.
func TestSetBuildTagOverridesResponseTag(t *testing.T) {
	const original = "dev"
	if responseBuildTag != original {
		t.Fatalf("precondition: responseBuildTag = %q, want %q", responseBuildTag, original)
	}

	defer SetBuildTag(original) // restore

	SetBuildTag("release-2026-07-29-abcd1234")
	if responseBuildTag != "release-2026-07-29-abcd1234" {
		t.Fatalf("responseBuildTag = %q, want release tag", responseBuildTag)
	}

	// Empty input is a no-op so partial init code paths that
	// pass "" cannot accidentally clear a real tag.
	SetBuildTag("")
	if responseBuildTag != "release-2026-07-29-abcd1234" {
		t.Fatalf("responseBuildTag = %q after empty SetBuildTag, want unchanged", responseBuildTag)
	}
}
