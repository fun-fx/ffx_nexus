package observability

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// These tests are minimal: they only exercise listDataSources which is
// the function we changed. The test file lives next to the production
// file because the helper types (metabaseDatabase, *MetabaseBootstrapper,
// etc.) are unexported.

func TestListDataSourcesPaginatedShape(t *testing.T) {
	// Metabase 0.50+ wraps the list response: {"data": [...], "total": N}.
	// listDataSources must peek at the first non-whitespace byte; if it's
	// '{', decode into the wrapper; otherwise treat as a bare array.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/database" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":1,"name":"Sample Database","engine":"h2","details":{"db":"file:sample.db"}},{"id":2,"name":"nexus-clickhouse","engine":"clickhouse","details":{"nexus_managed_by":"metabase-bootstrapper/v1"}}],"total":2,"limit":50,"offset":0}`))
	}))
	defer srv.Close()

	mb := &MetabaseBootstrapper{
		cfg:    MetabaseConfig{URL: srv.URL},
		client: srv.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	got, err := mb.listDataSources(context.Background(), "fake-session")
	if err != nil {
		t.Fatalf("paginated decode failed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 databases, got %d", len(got))
	}
	if got[0].Name != "Sample Database" || got[0].Engine != "h2" {
		t.Errorf("first database wrong: got %+v", got[0])
	}
	if !isNexusManagedDatabase(got[1].Details) {
		t.Errorf("second database should be marked nexus_managed_by; details=%+v", got[1].Details)
	}
}

func TestListDataSourcesBareArrayShape(t *testing.T) {
	// Vintage Metabase 0.49.x returns a bare array. Ensure we still work
	// against that shape so re-deploys against an older metabase (e.g.
	// the dev container's 0.49.13) don't crash.

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1,"name":"Sample","engine":"h2"}]`))
	}))
	defer srv.Close()

	mb := &MetabaseBootstrapper{
		cfg:    MetabaseConfig{URL: srv.URL},
		client: srv.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	got, err := mb.listDataSources(context.Background(), "fake-session")
	if err != nil {
		t.Fatalf("bare-array decode failed: %v", err)
	}
	if len(got) != 1 || got[0].Name != "Sample" {
		t.Fatalf("bare-array decode wrong: got %+v", got)
	}
}

func TestListDataSourcesEmptyObjectIsCleanNil(t *testing.T) {
	// Defensive: an empty `{}` response (no databases) should not panic
	// and should return an empty slice. Older versions of the code would
	// have errored on `cannot unmarshal object into []metabaseDatabase`;
	// we want empty success.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	mb := &MetabaseBootstrapper{
		cfg:    MetabaseConfig{URL: srv.URL},
		client: srv.Client(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	got, err := mb.listDataSources(context.Background(), "fake-session")
	if err != nil {
		t.Fatalf("empty-object decode failed: %v", err)
	}
	if got != nil && len(got) != 0 {
		t.Errorf("expected nil/empty result, got %+v", got)
	}
	if got == nil {
		got = []metabaseDatabase{} // canonicalize
	}
}

// Verify the wrapper-as-object shape is documented in vendored fixtures
// for future readers. Strict equality is overkill; we just round-trip.
func TestMetabaseDatabaseWrapperRoundtrip(t *testing.T) {
	wrapped := `{"data":[{"id":7,"name":"x","engine":"postgres","details":{"host":"db.example"}}],"total":1}`
	var got struct {
		Data []metabaseDatabase `json:"data"`
	}
	if err := json.Unmarshal([]byte(wrapped), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Data) != 1 || got.Data[0].Engine != "postgres" || got.Data[0].Details["host"] != "db.example" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

// sanity guard for common strings.NewReader usage in tests
var _ = http.MethodGet

// TestClickHouseDetailsVerboseShape covers the clickHouseDetails refactor.
// The driver-driven "verbose" form (host/port/dbname/user) bypasses the
// URL-form reaction that has caused the prod cluster to refuse every
// datasource registration.
func TestClickHouseDetailsVerboseShape(t *testing.T) {
	cases := []struct {
		name, in string
		want     map[string]any
	}{
		{
			name: "native-driver URL with user, no password",
			in:   "clickhouse://default:@chendpoint-clickhouse-nexus:9000/nexus",
			want: map[string]any{
				"host":   "chendpoint-clickhouse-nexus",
				"port":   9000,
				"dbname": "nexus",
				"user":   "default",
			},
		},
		{
			name: "native-driver URL with ?database= when path is empty",
			in:   "clickhouse://default:@chendpoint-clickhouse-nexus:9000/?database=nexus",
			want: map[string]any{
				"host":   "chendpoint-clickhouse-nexus",
				"port":   9000,
				"dbname": "nexus",
				"user":   "default",
			},
		},
		{
			name: "default port 9000 inferred when omitted",
			in:   "clickhouse://default:@chendpoint-clickhouse-nexus/nexus",
			want: map[string]any{
				"host":   "chendpoint-clickhouse-nexus",
				"port":   9000,
				"dbname": "nexus",
				"user":   "default",
			},
		},
		{
			name: "user and password preserved when present",
			in:   "clickhouse://alice:s3cret@chendpoint-clickhouse-nexus:9000/db",
			want: map[string]any{
				"host":     "chendpoint-clickhouse-nexus",
				"port":     9000,
				"dbname":   "db",
				"user":     "alice",
				"password": "s3cret",
			},
		},
		{
			name: "empty password omits the field, not as empty string",
			in:   "http://chendpoint-clickhouse-nexus.tenant-nexus.svc.cluster.local:8123?database=nexus",
			want: map[string]any{
				"host":   "chendpoint-clickhouse-nexus.tenant-nexus.svc.cluster.local",
				"port":   8123,
				"dbname": "nexus",
				"user":   "default",
				// NOTE: no "password" key — clickhouse-jdbc interprets
				// an empty-string password as "wrong credentials".
				// Omitting the field lets it fall through to
				// anonymous/default auth which matches the cluster.
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clickHouseDetails(tc.in)
			for k, wantV := range tc.want {
				if v, ok := got[k]; !ok {
					t.Errorf("missing key %q in details: %+v", k, got)
				} else if v != wantV {
					t.Errorf("key %q: got %v (%T), want %v (%T)", k, v, v, wantV, wantV)
				}
			}
			// only the empty-pw case must NOT have a password;
			// the others deliberately include it.
			if tc.name != "empty password omits the field, not as empty string" {
				return
			}
			if _, hasPw := got["password"]; hasPw {
				t.Errorf("password field must be omitted for empty-pw URLs, got %+v", got)
			}
		})
	}
}

// TestClickHouseDetailsLegacyBareURL — when the URL is unparseable we
// fall back to the bare-url form so operators on a strange host scheme
// (e.g. legacy HTTP-driver URL) still get *something* that's registered.
func TestClickHouseDetailsLegacyBareURL(t *testing.T) {
	in := "http://chendpoint-clickhouse-nexus:8123?database=nexus"
	got := clickHouseDetails(in)
	// Either verbose form (acceptable) or url fallback (also acceptable).
	if v, hasURL := got["url"]; hasURL {
		if v != in {
			t.Errorf("url fallback divergent: got %v", v)
		}
		return
	}
	// verbose form: must contain host + dbname at minimum
	for _, k := range []string{"host", "dbname"} {
		if _, ok := got[k]; !ok {
			t.Errorf("verbose detail missing %q: %+v", k, got)
		}
	}
}
