package console

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/core"
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
			if filter.Status != tc.want.Status || filter.Provider != tc.want.Provider ||
				filter.Q != tc.want.Q || filter.Turn != tc.want.Turn ||
				len(filter.Statuses) != len(tc.want.Statuses) {
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

func TestParseTraceSeriesQuery_PeriodAndStatus(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/traces/series?period=24h&status=ok,err", nil)
	before, since, filter, err := parseTraceSeriesQuery(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if before.IsZero() || since.IsZero() {
		t.Fatal("period should resolve since/before")
	}
	if before.Sub(since) < 23*time.Hour {
		t.Errorf("expected ~24h window, got %v", before.Sub(since))
	}
	if len(filter.Statuses) != 2 || filter.Statuses[0] != "ok" || filter.Statuses[1] != "err" {
		t.Errorf("statuses: %+v", filter.Statuses)
	}
}

func TestParseTraceSeriesQuery_ExplicitWindow(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/traces/series?before=2026-07-27T09:00:00Z&since=2026-07-20T00:00:00Z", nil)
	before, since, _, err := parseTraceSeriesQuery(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantBefore := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	wantSince := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	if !before.Equal(wantBefore) || !since.Equal(wantSince) {
		t.Errorf("window mismatch: before=%v since=%v", before, since)
	}
}

func TestParseTraceSeriesQuery_RejectsInvalidInputs(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"bad period", "/api/traces/series?period=week", "period"},
		{"bad status", "/api/traces/series?status=warning", "status"},
		{"window inverted", "/api/traces/series?before=2026-07-20T00:00:00Z&since=2026-07-27T00:00:00Z", "before"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.url, nil)
			_, _, _, err := parseTraceSeriesQuery(r)
			if err == nil {
				t.Fatalf("want error containing %q", tc.want)
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestTraceVolumeSeries_NilReaderMarksUnavailable(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest("GET", "/api/traces/series?period=1h", nil)
	rec := httptest.NewRecorder()
	srv.traceVolumeSeries(rec, req, core.User{ID: "u1", Role: core.RoleMember, OrgID: "org-a"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got observability.VolumeSeries
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Available {
		t.Error("available must be false when ClickHouse is not wired")
	}
	if got.Buckets == nil {
		t.Error("buckets must be a slice, not null")
	}
	if got.IntervalSeconds != 60 {
		t.Errorf("1h window interval want 60, got %d", got.IntervalSeconds)
	}
}

func TestParseTraceSeriesQuery_ProviderAndModel(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/traces/dashboard?period=1h&provider=openai,gemini&model=gpt-4o-mini", nil)
	_, _, filter, err := parseTraceSeriesQuery(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(filter.Providers) != 2 || filter.Providers[0] != "openai" || filter.Providers[1] != "gemini" {
		t.Errorf("providers: %+v", filter.Providers)
	}
	if len(filter.Models) != 1 || filter.Models[0] != "gpt-4o-mini" {
		t.Errorf("models: %+v", filter.Models)
	}
}

func TestTraceDashboard_NilReaderMarksUnavailable(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest("GET", "/api/traces/dashboard?period=1h", nil)
	rec := httptest.NewRecorder()
	srv.traceDashboard(rec, req, core.User{ID: "u1", Role: core.RoleMember, OrgID: "org-a"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got observability.Dashboard
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Available {
		t.Error("available must be false when ClickHouse is not wired")
	}
	if got.Volume == nil || got.Tokens == nil || got.Cost == nil || got.Latency == nil || got.Cache == nil {
		t.Error("series slices must be empty arrays, not null")
	}
	if got.Facets.Providers == nil || got.Facets.Models == nil {
		t.Error("facets must be empty arrays, not null")
	}
	if got.IntervalSeconds != 60 {
		t.Errorf("1h window interval want 60, got %d", got.IntervalSeconds)
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
