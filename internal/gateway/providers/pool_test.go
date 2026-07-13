package providers

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPooledTransport_AcceptsCallerSize confirms that explicitly-sized
// pools are honored (cli/main code can override the default for stress
// tests). Defaults are covered separately under
// TestPooledTransport_ZeroFallsBackToDefault.
func TestPooledTransport_AcceptsCallerSize(t *testing.T) {
	tr := NewPooledTransport(50)
	if got, want := tr.MaxIdleConnsPerHost, 50; got != want {
		t.Fatalf("MaxIdleConnsPerHost: got %d want %d", got, want)
	}
	if got, want := tr.IdleConnTimeout, 90*time.Second; got != want {
		t.Fatalf("IdleConnTimeout: got %v want %v", got, want)
	}
	if tr.TLSHandshakeTimeout != 5*time.Second {
		t.Fatalf("TLSHandshakeTimeout: got %v want 5s", tr.TLSHandshakeTimeout)
	}
}

// TestPooledTransport_ZeroFallsBackToDefault verifies zero size -> the
// runtime-proportional default. The default is `min(100, 2*GOMAXPROCS)`
// (clamped to a 32–100 floor/ceiling).
func TestPooledTransport_ZeroFallsBackToDefault(t *testing.T) {
	tr := NewPooledTransport(0)
	if tr.MaxIdleConnsPerHost < 32 {
		t.Fatalf("default pool size should be >=32, got %d", tr.MaxIdleConnsPerHost)
	}
	if tr.MaxIdleConnsPerHost > 100 {
		t.Fatalf("default pool size should be <=100, got %d", tr.MaxIdleConnsPerHost)
	}
}

// TestPooledHTTPClient_ConcurrencyUnderLoad hammers a local httptest.Server
// with N goroutines and asserts no transport errors are observed. The
// pool shouldn't drop or coalesce connections mid-flight.
func TestPooledHTTPClient_ConcurrencyUnderLoad(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	ln, err := newLocalListener()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	srvURL := "http://" + ln.Addr().String()

	const goroutines = 64
	const perG = 25

	client := PooledHTTPClient(2 * time.Second)
	var wg sync.WaitGroup
	var errCount int64
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				req, err := http.NewRequest(http.MethodGet, srvURL, nil)
				if err != nil {
					atomic.AddInt64(&errCount, 1)
					return
				}
				resp, err := client.Do(req)
				if err != nil {
					atomic.AddInt64(&errCount, 1)
					return
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					atomic.AddInt64(&errCount, 1)
				}
			}
		}()
	}
	wg.Wait()
	if errCount != 0 {
		t.Fatalf("got %d transport errors under concurrency", errCount)
	}
}
