// Load generator for bench runs. Maintains C concurrent goroutines
// that issue requests in a tight loop, recording per-request latency.
// After `--duration` it snapshots stats: total / errors / p50 / p99 /
// throughput, and exits.
//
// We write this ourselves instead of using wrk because we want to
// measure: (a) gateway behaviour with our exact request shape, (b)
// per-status-code distribution, (c) the *worst* p99, not the steady
// one. wrk stabilises more aggressively than we want.
//
// Flags:
//
//	-url      target endpoint
//	-c        concurrency
//	-d        duration
//	-stream   stream mode (consume chunked response, no read-to-end)
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:8090/v1/chat/completions", "target URL")
	concurrency := flag.Int("c", 100, "concurrent workers")
	duration := flag.Duration("d", 30*time.Second, "test duration")
	stream := flag.Bool("stream", false, "streaming mode")
	flag.Parse()

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:          4096,
			MaxIdleConnsPerHost:   1024,
			IdleConnTimeout:       90 * time.Second,
			DisableCompression:    true,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		Timeout: 60 * time.Second,
	}

	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`
	if *stream {
		body = `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`
	}

	stop := time.After(*duration)

	var (
		latencies []time.Duration
		latMu     sync.Mutex
		okN       int64
		errN      int64
		statusN   [600]int64
	)

	push := func(d time.Duration, code int) {
		latMu.Lock()
		latencies = append(latencies, d)
		latMu.Unlock()
		if code >= 200 && code < 400 {
			atomic.AddInt64(&okN, 1)
		} else {
			atomic.AddInt64(&errN, 1)
		}
		if code >= 0 && code < 600 {
			atomic.AddInt64(&statusN[code], 1)
		}
	}

	var wg sync.WaitGroup
	worker := func(id int) {
		defer wg.Done()
		reqBytes := []byte(body)
		for {
			select {
			case <-stop:
				return
			default:
			}
			req, err := http.NewRequest(http.MethodPost, *url, bytes.NewReader(reqBytes))
			if err != nil {
				atomic.AddInt64(&errN, 1)
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			t0 := time.Now()
			resp, err := client.Do(req)
			if err != nil {
				push(time.Since(t0), -1)
				continue
			}
			if *stream {
				// drain just enough to count it as a successful stream
				_, _ = io.Copy(io.Discard, resp.Body)
			}
			code := resp.StatusCode
			resp.Body.Close()
			push(time.Since(t0), code)

			// Cooperative yield: do NOT spin. Sleep a tick to let the
			// scheduler serve other workers. Without this on low
			// latency upstream, a hot worker can starve its peers.
			select {
			case <-stop:
				return
			default:
			}
			time.Sleep(100 * time.Microsecond)
			_ = id
		}
	}

	start := time.Now()
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go worker(i)
	}
	// Bound how long we'll wait for workers to drain in-flight
	// requests after the deadline. Without this, a wedged connection
	// can keep the binary alive forever (the deadline is "stop
	// accepting new requests", not "force-kill goroutines").
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		fmt.Fprintln(os.Stderr, "[loadgen] WARNING: workers did not exit within 5s of deadline; binary exiting anyway")
	}
	elapsed := time.Since(start)

	latMu.Lock()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	n := len(latencies)
	p := func(q float64) time.Duration {
		if n == 0 {
			return 0
		}
		return latencies[int(float64(n)*q)]
	}
	p50 := p(0.50)
	p90 := p(0.90)
	p99 := p(0.99)
	max := time.Duration(0)
	if n > 0 {
		max = latencies[n-1]
	}
	latMu.Unlock()

	total := okN + errN
	rps := float64(total) / elapsed.Seconds()

	out := strings.Builder{}
	fmt.Fprintf(&out, "concurrency=%d duration=%s n=%d ok=%d err=%d rps=%.1f\n",
		*concurrency, elapsed.Round(time.Millisecond), total, okN, errN, rps)
	fmt.Fprintf(&out, "latency p50=%v p90=%v p99=%v max=%v\n", p50, p90, p99, max)
	fmt.Fprintf(&out, "status histogram:\n")
	for code := 200; code < 600; code++ {
		if c := statusN[code]; c > 0 {
			fmt.Fprintf(&out, "  %d: %d\n", code, c)
		}
	}
	fmt.Print(out.String())
	_ = os.Stdout.Sync()
}
