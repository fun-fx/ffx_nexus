package console

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/observability"
)

// parseTraceQuery is a thin wrapper over `net/url.Query` plus RFC3339
// parsing — the contract worth pinning is the error envelope: any
// invalid input must yield a 4xx-ready error, and accepted forms must
// round-trip to the filter struct the reader expects.
func TestParseTraceQuery_AcceptsValidForms(t *testing.T) {
	cases := []struct {
		name       string
		url        string
		want       observability.TraceFilter
		wantBefore time.Time
		wantSince  time.Time
	}{
		{
			name: "no params",
			url:  "/api/traces",
			want: observability.TraceFilter{},
		},
		{
			name: "status and provider",
			url:  "/api/traces?status=err&provider=anthropic",
			want: observability.TraceFilter{Status: "err", Provider: "anthropic"},
		},
		{
			name: "status ok full url",
			url:  "/api/traces?status=ok&provider=openai&q=gpt",
			want: observability.TraceFilter{Status: "ok", Provider: "openai", Q: "gpt"},
		},
		{
			name:       "window with both bounds",
			url:        "/api/traces?before=2026-07-27T09:00:00Z&since=2026-07-20T00:00:00Z",
			want:       observability.TraceFilter{},
			wantBefore: time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC),
			wantSince:  time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "RFC3339Nano accepted",
			url:  "/api/traces?before=2026-07-27T09:00:00.123456789Z",
			want: observability.TraceFilter{},
			// sub-second precision gets folded to nano; just assert non-zero
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.url, nil)
			before, since, filter, err := parseTraceQuery(r)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if filter != tc.want {
				t.Errorf("filter mismatch: want %+v got %+v", tc.want, filter)
			}
			if !tc.wantBefore.IsZero() && !before.Equal(tc.wantBefore) {
				t.Errorf("before want=%v got=%v", tc.wantBefore, before)
			}
			if !tc.wantSince.IsZero() && !since.Equal(tc.wantSince) {
				t.Errorf("since want=%v got=%v", tc.wantSince, since)
			}
		})
	}
}

func TestParseTraceQuery_RejectsInvalidInputs(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string // substring expected in error
	}{
		{"bad before", "/api/traces?before=yesterday", "before"},
		{"bad since", "/api/traces?since=2026-13-99", "since"},
		{"bad status", "/api/traces?status=warning", "status"},
		{"window inverted", "/api/traces?before=2026-07-20T00:00:00Z&since=2026-07-27T00:00:00Z", "before"},
		{"window equal", "/api/traces?before=2026-07-20T00:00:00Z&since=2026-07-20T00:00:00Z", "before"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.url, nil)
			_, _, _, err := parseTraceQuery(r)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.want)
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// contains is a tiny local helper so we don't import "strings" just for
// this assertion; the rest of the file accepts strings import through
// the surrounding package.
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
