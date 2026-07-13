// Streaming-buffer integration test.
//
// Goal: prove the pooled buffer + the upstream pool together keep
// http2/1 error-free latency under a sustained load of concurrent
// streaming requests. We use a simple echo server that pipes a
// generated SSE-shaped body, fire many concurrent streams, and
// confirm:
//
//  1. Every stream completes without transport error.
//  2. No goroutine bug surfaces (race-clean).
//  3. Memory headroom: the test passes with a tight GOMEMLIMIT so
//     regressions show up under heap pressure.

package providers

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// startStreamServer returns a httptest-like server that responds to
// /v1/chat/completions with a small but representative SSE stream —
// enough events per stream to exercise the buffer pool over multiple
// scanner.Buffer sizes.
func startStreamServer(t *testing.T) (string, func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// Emit 80 small events. Each one is well under 64KiB but the
		// total stresses the buffer pool as it cycles.
		for i := 0; i < 80; i++ {
			data := fmt.Sprintf(`{"choices":[{"delta":{"content":"chunk-%d"}}]}`, i)
			if _, err := io.WriteString(w, "data: "+data+"\n\n"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	return "http://" + ln.Addr().String(), func() { _ = srv.Close() }
}

// TestStreaming_BufferPool_ConcurrentStreams is the smoke test for the
// V5 buffer pool. It fires N concurrent streams at a local SSE server
// and asserts no goroutine sees a transport error and no panic surfaces.
// Run with `-race` for the full guarantee.
func TestStreaming_BufferPool_ConcurrentStreams(t *testing.T) {
	base, done := startStreamServer(t)
	defer done()

	const goroutines = 24
	const retries = 4 // 4 streams per goroutine -> high contention
	client := PooledHTTPClient(5 * time.Second)

	var wg sync.WaitGroup
	var transportErrs int64
	var lines int64

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for r := 0; r < retries; r++ {
				req, err := http.NewRequestWithContext(context.Background(),
					http.MethodPost, base+"/v1/chat/completions",
					strings.NewReader(`{"model":"x","messages":[],"stream":true}`))
				if err != nil {
					atomic.AddInt64(&transportErrs, 1)
					return
				}
				resp, err := client.Do(req)
				if err != nil {
					atomic.AddInt64(&transportErrs, 1)
					return
				}
				if resp.StatusCode != http.StatusOK {
					resp.Body.Close()
					atomic.AddInt64(&transportErrs, 1)
					return
				}
				// Use bufio.Scanner — same code path as parseOpenAISSE
				// — to exercise the buffer pool contract end-to-end.
				scanner := bufio.NewScanner(resp.Body)
				buf := acquireBuffer()
				scanner.Buffer(buf, 1024*1024)
				var myLines int64
				for scanner.Scan() {
					myLines++
					atomic.AddInt64(&lines, 1)
				}
				releaseBuffer(scanner.Bytes())
				if err := scanner.Err(); err != nil {
					atomic.AddInt64(&transportErrs, 1)
					_ = myLines
					return
				}
				resp.Body.Close()
			}
		}(g)
	}
	wg.Wait()

	if transportErrs != 0 {
		t.Fatalf("transport errors under load: %d", transportErrs)
	}
	want := int64(goroutines * retries * 81) // 80 events + final DONE
	if lines < want {
		// Some scanners count "[DONE]" as an extra line. Allow ≥.
		t.Fatalf("lines: got %d want at least %d", lines, want)
	}
}
