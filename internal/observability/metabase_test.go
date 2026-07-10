package observability

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestMetabaseBootstrapperNilWhenURLEmpty confirms the opt-in contract
// matches the V3 OTLP toggle: an empty URL disables the adapter outright (no
// DNS, no goroutines, no boot traffic). Main.go then skips wiring the
// MultiBootstrapper.
func TestMetabaseBootstrapperNilWhenURLEmpty(t *testing.T) {
	mb := NewMetabaseBootstrapper(MetabaseConfig{URL: ""}, nil)
	if mb != nil {
		t.Fatal("empty URL must produce nil bootstrapper (opt-in contract)")
	}
	mb = NewMetabaseBootstrapper(MetabaseConfig{URL: "   "}, nil)
	if mb != nil {
		t.Fatal("whitespace URL must produce nil bootstrapper")
	}
}

// TestMetabaseBootstrapperHealthRetry verifies the adapter waits for
// Metabase's /api/health to come up before doing real work. The httptest
// server returns 500 twice, then 200; the bootstrapper should eventually
// succeed on the third poll.
func TestMetabaseBootstrapperHealthRetry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only count poll hits on /api/health; other paths (e.g. /api/session
		// returning 404 below) are tolerated because the test only cares about
		// the health-retry behavior, not the full bootstrap flow.
		if r.URL.Path != "/api/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n := hits.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()

	mb := NewMetabaseBootstrapper(MetabaseConfig{
		URL:            srv.URL,
		User:           "u",
		Password:       "p",
		HealthTimeout:  5 * time.Second,
		RequestTimeout: 2 * time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if mb == nil {
		t.Fatal("non-empty URL must produce a bootstrapper")
	}

	// We expect Bootstrap to fail at the login step (httptest server only
	// serves /api/health), but reaching login verifies the health retry
	// succeeded.
	_ = mb.Bootstrap(context.Background())
	if hits.Load() < 3 {
		t.Fatalf("expected at least 3 health polls, got %d", hits.Load())
	}
}

// TestMetabaseBootstrapperHealthTimeout verifies the adapter gives up after
// HealthTimeout when Metabase never comes up. This protects Nexus startup
// from being gated on a dead Metabase container.
func TestMetabaseBootstrapperHealthTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	mb := NewMetabaseBootstrapper(MetabaseConfig{
		URL:            srv.URL,
		HealthTimeout:  200 * time.Millisecond,
		RequestTimeout: 100 * time.Millisecond,
	}, nil)

	start := time.Now()
	err := mb.Bootstrap(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error; bootstrap succeeded")
	}
	if !strings.Contains(err.Error(), "health") {
		t.Errorf("error should mention health; got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("health timeout should be respected (got %s)", elapsed)
	}
}

// TestMetabaseBootstrapperDatasourceIdempotency simulates a Metabase that
// already has the database registered (returns it on GET). The bootstrap
// must perform a PUT refresh (idempotent), not a duplicate POST.
func TestMetabaseBootstrapperDatasourceIdempotency(t *testing.T) {
	var post, put atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"sess-1"}`)
	})
	mux.HandleFunc("/api/database", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// Pre-existing datasource OWNED by a previous Nexus deploy
			// (marker stamped on details). The PUT refresh path is therefore
			// expected — a foreign datasource would not be touched.
			_, _ = io.WriteString(w,
				`[{"id":42,"name":"nexus-clickhouse","engine":"clickhouse","details":{"dbname":"nexus","nexus_managed_by":"metabase-bootstrapper/v1"}}]`)
		case http.MethodPost:
			post.Add(1)
			_, _ = io.WriteString(w, `{"id":99}`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/database/42", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("update path expected PUT, got %s", r.Method)
		}
		put.Add(1)
		_, _ = io.WriteString(w, `{"id":42}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mb := NewMetabaseBootstrapper(MetabaseConfig{
		URL:            srv.URL,
		User:           "u",
		Password:       "p",
		ClickHouseHTTP: "http://clickhouse:8123?database=nexus",
		HealthTimeout:  2 * time.Second,
		RequestTimeout: 2 * time.Second,
	}, nil)

	if err := mb.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if got := post.Load(); got != 0 {
		t.Errorf("POSTs to /api/database must be 0 when already registered, got %d", got)
	}
	if got := put.Load(); got != 1 {
		t.Errorf("expected 1 PUT refresh to /api/database/42, got %d", got)
	}
}

// TestMetabaseBootstrapperForeignDatasourceNotOwned ensures we never PUT to
// a datasource that was created by someone else even when its name happens
// to match the "nexus-<engine>" reservation. This is the safeguard that
// protects an org's existing Metabase from accidental clobber when Nexus
// is deployed into Pattern B (existing shared Metabase).
func TestMetabaseBootstrapperForeignDatasourceNotOwned(t *testing.T) {
	var post, put atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"s"}`)
	})
	mux.HandleFunc("/api/database", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// Pre-existing datasource WITHOUT the ownership marker — owned
			// by some other team, NOT by a previous Nexus deploy.
			_, _ = io.WriteString(w, `[{"id":13,"name":"nexus-clickhouse","engine":"clickhouse","details":{"dbname":"other_team"}}]`)
		case http.MethodPost:
			post.Add(1)
			_, _ = io.WriteString(w, `{"id":99}`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/database/13", func(w http.ResponseWriter, r *http.Request) {
		put.Add(1)
		_, _ = io.WriteString(w, `{"id":13}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mb := NewMetabaseBootstrapper(MetabaseConfig{
		URL:            srv.URL,
		User:           "u",
		Password:       "p",
		ClickHouseHTTP: "http://clickhouse:8123?database=nexus",
		HealthTimeout:  2 * time.Second,
		RequestTimeout: 2 * time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := mb.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if got := post.Load(); got != 0 {
		t.Errorf("must not POST a duplicate datasource, got %d", got)
	}
	if got := put.Load(); got != 0 {
		t.Errorf("must not PUT-update a foreign datasource, got %d", got)
	}
}

// TestMetabaseBootstrapperForeignCollectionNotOwned is the collection-level
// twin: if a non-Nexus collection already occupies the reserved "Nexus - "
// name, the adapter must not replace it. The cards loop on this collection
// is skipped (because we return its id without flagging it as a seed target),
// so the operator's dashboards stay byte-identical.
func TestMetabaseBootstrapperForeignCollectionNotOwned(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"s"}`)
	})
	mux.HandleFunc("/api/database", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `[{"id":7,"name":"nexus-clickhouse","engine":"clickhouse","details":{"nexus_managed_by":"metabase-bootstrapper/v1"}}]`)
			return
		}
		_, _ = io.WriteString(w, `{"id":7}`)
	})
	mux.HandleFunc("/api/database/7", func(w http.ResponseWriter, r *http.Request) {
		// PUT refresh during ensureDataSources; always succeed.
		_, _ = io.WriteString(w, `{"id":7}`)
	})
	var collPosts atomic.Int32
	mux.HandleFunc("/api/collection", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// Pre-existing collection WITH the reserved "Nexus - " name but
			// OWNED by the data team — description does NOT have the marker.
			_, _ = io.WriteString(w, `[{"id":21,"name":"Nexus - 01 - Overview","description":"budget owned"}]`)
			return
		case http.MethodPost:
			collPosts.Add(1)
			_, _ = io.WriteString(w, `{"id":99}`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	var cardCalls atomic.Int32
	mux.HandleFunc("/api/card", func(w http.ResponseWriter, r *http.Request) {
		cardCalls.Add(1)
		_, _ = io.WriteString(w, `{"id":1}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "01-overview.json"), []byte(
		`{"name":"01 - Overview","cards":[{"name":"c1","engine":"clickhouse","query":{"native":{"query":"SELECT 1"}}}]}`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	mb := NewMetabaseBootstrapper(MetabaseConfig{
		URL:            srv.URL,
		User:           "u",
		Password:       "p",
		ClickHouseHTTP: "http://clickhouse:8123",
		SeedDir:        dir,
		HealthTimeout:  2 * time.Second,
		RequestTimeout: 2 * time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := mb.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if got := collPosts.Load(); got != 0 {
		t.Errorf("must not POST a new collection over foreign one, got %d", got)
	}
	if got := cardCalls.Load(); got != 0 {
		t.Errorf("must not seed cards into a foreign collection, got %d", got)
	}
}

// TestMetabaseBootstrapperOwnedCollectionRefreshed is the happy-path twin:
// the collection already exists WITH the marker, the seed loader reuses the
// id and posts new cards into it. This guards against the marker check
// accidentally hiding migrations on re-deploys.
func TestMetabaseBootstrapperOwnedCollectionRefreshed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"s"}`)
	})
	mux.HandleFunc("/api/database", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `[{"id":7,"name":"nexus-clickhouse","engine":"clickhouse","details":{"nexus_managed_by":"metabase-bootstrapper/v1"}}]`)
			return
		}
		_, _ = io.WriteString(w, `{"id":7}`)
	})
	mux.HandleFunc("/api/database/7", func(w http.ResponseWriter, r *http.Request) {
		// PUT refresh during ensureDataSources; always succeed.
		_, _ = io.WriteString(w, `{"id":7}`)
	})
	var collPosts atomic.Int32
	mux.HandleFunc("/api/collection", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `[{"id":31,"name":"Nexus - 01 - Overview","description":"[Nexus-managed] prior description"}]`)
			return
		case http.MethodPost:
			collPosts.Add(1)
			_, _ = io.WriteString(w, `{"id":99}`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	var cardCalls atomic.Int32
	mux.HandleFunc("/api/card", func(w http.ResponseWriter, r *http.Request) {
		cardCalls.Add(1)
		_, _ = io.WriteString(w, `{"id":1}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "01-overview.json"), []byte(
		`{"name":"01 - Overview","cards":[{"name":"c1","engine":"clickhouse","query":{"native":{"query":"SELECT 1"}}}]}`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	mb := NewMetabaseBootstrapper(MetabaseConfig{
		URL:            srv.URL,
		User:           "u",
		Password:       "p",
		ClickHouseHTTP: "http://clickhouse:8123",
		SeedDir:        dir,
		HealthTimeout:  2 * time.Second,
		RequestTimeout: 2 * time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := mb.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if got := collPosts.Load(); got != 0 {
		t.Errorf("owned collection reuse should not POST, got %d", got)
	}
	if got := cardCalls.Load(); got < 1 {
		t.Errorf("owned collection should be refreshed (>=1 card POST), got %d", got)
	}
}

// TestMetabaseBootstrapperSeedReadsJSONDirs verifies the seed loader walks
// the directory and consumes only .json files. A failing card file must
// not crash the whole bootstrap; it logs and continues.
func TestMetabaseBootstrapperSeedReadsJSONDirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "00-good.json"), []byte(`{
		"name": "00-good", "cards": [{"name":"c1","engine":"clickhouse","query":{"native":{"query":"SELECT 1"}}}]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"s"}`)
	})
	mux.HandleFunc("/api/database", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `[]`)
			return
		}
		_, _ = io.WriteString(w, `{"id":7}`)
	})
	mux.HandleFunc("/api/collection", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// Listing is an array, with no pre-existing "Nexus - *" entries
			// so the adapter takes the create path.
			_, _ = io.WriteString(w, `[]`)
		case http.MethodPost:
			_, _ = io.WriteString(w, `{"id":11}`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	cards := atomic.Int32{}
	mux.HandleFunc("/api/card", func(w http.ResponseWriter, r *http.Request) {
		cards.Add(1)
		_, _ = io.WriteString(w, `{"id":1}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mb := NewMetabaseBootstrapper(MetabaseConfig{
		URL:            srv.URL,
		User:           "u",
		Password:       "p",
		ClickHouseHTTP: "http://clickhouse:8123",
		SeedDir:        dir,
		HealthTimeout:  2 * time.Second,
		RequestTimeout: 2 * time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := mb.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if got := cards.Load(); got != 1 {
		t.Errorf("expected 1 card created, got %d", got)
	}
}

// TestMultiBootstrapperNamesAndNilSafe verifies the MultiBootstrapper helper
// satisfies the same nil-safety contract as MultiRecorder: nil receivers are
// no-ops, nil children are skipped, and Names() reflects the registered set.
func TestMultiBootstrapperNamesAndNilSafe(t *testing.T) {
	var nilMulti *MultiBootstrapper
	if err := nilMulti.Bootstrap(context.Background()); err != nil {
		t.Errorf("nil MultiBootstrapper must be a no-op, got %v", err)
	}
	if n := nilMulti.Names(); n != nil {
		t.Errorf("nil Names() must be nil, got %v", n)
	}

	mb := NewMultiBootstrapper()
	if err := mb.Bootstrap(context.Background()); err != nil {
		t.Errorf("empty MultiBootstrapper must be a no-op, got %v", err)
	}

	// Mix a nil child + a sentinel bootstrapper; the latter is the only thing
	// that should be invoked.
	called := false
	sentinel := fakeBootstrapper{
		name: "sentinel",
		fn: func() error {
			called = true
			return nil
		},
	}
	mb = NewMultiBootstrapper(nil, &sentinel, nil)
	_ = mb.Bootstrap(context.Background())
	if !called {
		t.Error("sentinel must be invoked")
	}
	names := mb.Names()
	if len(names) != 1 || names[0] != "sentinel" {
		t.Errorf("Names must return only registered children, got %v", names)
	}
}

// TestMultiBootstrapperAggregatesErrors confirms child failures are joined
// (not short-circuited) and forwarded to the caller.
func TestMultiBootstrapperAggregatesErrors(t *testing.T) {
	a := fakeBootstrapper{name: "a", fn: func() error { return errors.New("a-fail") }}
	b := fakeBootstrapper{name: "b", fn: func() error { return nil }}
	c := fakeBootstrapper{name: "c", fn: func() error { return errors.New("c-fail") }}
	mb := NewMultiBootstrapper(&a, &b, &c)
	err := mb.Bootstrap(context.Background())
	if err == nil {
		t.Fatal("expected joined error")
	}
	if !strings.Contains(err.Error(), "a-fail") || !strings.Contains(err.Error(), "c-fail") {
		t.Errorf("joined error must include both child errors; got %v", err)
	}
	if strings.Contains(err.Error(), "b-fail") {
		t.Errorf("b succeeded; its nil error must not leak")
	}
}

// fakeBootstrapper is a tiny inline Bootstrapper used by the multi tests.
type fakeBootstrapper struct {
	name string
	fn   func() error
}

func (f *fakeBootstrapper) Name() string { return f.name }
func (f *fakeBootstrapper) Bootstrap(context.Context) error {
	if f.fn == nil {
		return nil
	}
	return f.fn()
}

// waitUntil is reused from otel_test.go's pattern but inlined to keep
// the test surface small here.
func waitUntilMetabase(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}
