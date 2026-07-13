package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeCap is a deterministic Concurrency cap used by tests. Counter
// `rejected` tallies the requests denied by Acquire; `accepted` tallies
// the requests that acquired a slot, and `inFlightMax` records the peak
// simultaneous holdings.
type fakeCap struct {
	mu          sync.Mutex
	inflight    int
	capacity    int
	rejected    int64
	accepted    int64
	inFlightMax int64
}

func (f *fakeCap) Acquire(_ context.Context, _ string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inflight >= f.capacity {
		f.rejected++
		return false
	}
	f.inflight++
	f.accepted++
	if int64(f.inflight) > f.inFlightMax {
		f.inFlightMax = int64(f.inflight)
	}
	return true
}

func (f *fakeCap) Release(_ context.Context, _ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inflight > 0 {
		f.inflight--
	}
}

func (f *fakeCap) snapshot() (rej, acc, peak int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rejected, f.accepted, f.inFlightMax
}

// TestConcurrencyMiddleware_NilCap_Passthrough verifies that a nil cap
// doesn't reject anything (default behaviour, callers not opted in).
func TestConcurrencyMiddleware_NilCap_Passthrough(t *testing.T) {
	var c CapIface
	hits := atomic.Int64{}
	h := Concurrency(c)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	for i := 0; i < 5; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyVKeyID, "vk"))
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
	}
	if got := hits.Load(); got != 5 {
		t.Fatalf("hits: got %d want 5", got)
	}
}

// TestConcurrencyMiddleware_OverCap_Returns429 is the basic contract —
// once the cap is full, additional requests are denied with 429.
func TestConcurrencyMiddleware_OverCap_Returns429(t *testing.T) {
	cap := &fakeCap{capacity: 2}
	release := make(chan struct{})
	hold := make(chan struct{})
	hits := atomic.Int64{}
	h := Concurrency(cap)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-hold
		w.WriteHeader(http.StatusOK)
		<-release
	}))

	// Two requests fill the cap. We must not call w.WriteHeader (the
	// handler is blocked holding release) before both goroutines have
	// acquired. Use a sync.WaitGroup to coordinate.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req = req.WithContext(context.WithValue(req.Context(), ctxKeyVKeyID, "vk"))
			h.ServeHTTP(rr, req)
		}()
	}

	// Wait for both handlers to be holding their slots. Poll briefly.
	for {
		_, acc, _ := cap.snapshot()
		if acc >= 2 {
			break
		}
	}

	// Third request hits the cap and must 429.
	rr3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, "/", nil)
	req3 = req3.WithContext(context.WithValue(req3.Context(), ctxKeyVKeyID, "vk"))
	h.ServeHTTP(rr3, req3)
	if rr3.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd request: got %d want 429", rr3.Code)
	}

	// Unblock the two held goroutines.
	close(hold)
	close(release)
	wg.Wait()

	rej, acc, peak := cap.snapshot()
	if rej != 1 {
		t.Fatalf("rejected: got %d want 1", rej)
	}
	if acc != 2 {
		t.Fatalf("accepted: got %d want 2", acc)
	}
	if peak != 2 {
		t.Fatalf("peak inflight: got %d want 2", peak)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("handler hits: got %d want 2", got)
	}
}
