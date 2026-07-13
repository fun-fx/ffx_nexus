// Mock LLM upstream service — emulates an OpenAI-compatible chat
// completion endpoint with a configurable response delay, error rate,
// and (optional) SSE streaming. Used by bench/ scripts to measure
// gateway throughput without paying for live provider calls.
//
// The server is intentionally *minimal*: no auth check (the gateway
// points it at a fake API key in tests), no JSON validation beyond
// what `net/http` requires to parse a body, no fancy logging.
//
// Flags:
//
//	-addr      listen address (default ":18080")
//	-latency   baseline per-request artificial response delay (default 50ms)
//	-ttft     time to first token per chunk (default 25ms)
//	-chunk-ms  delay between stream chunks (default 35ms)
//	-tokens    how many stream chunks to emit (default 8)
//	-error-rate probability of HTTP 500 (default 0.0)
//	-stream    enable SSE streaming responses (default false)
//	-workers   bounded worker pool size (default 0 = unbounded).
//	           When set, an integer semaphore caps concurrent in-flight
//	           request handling so we simulate a real upstream whose GPU
//	           pool is finite. Bench scripts can sweep this knob to study
//	           gateway behaviour under explicit upstream concurrency caps.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", ":18080", "listen address")
	latencyMS := flag.Int("latency", 50, "baseline per-request artificial response delay (ms)")
	ttftMS := flag.Int("ttft", 25, "time to first token (stream start) in ms")
	chunkMS := flag.Int("chunk-ms", 35, "delay between stream chunks (ms)")
	tokens := flag.Int("tokens", 8, "number of stream chunks to emit")
	errRate := flag.Float64("error-rate", 0.0, "fraction of requests returning 500")
	stream := flag.Bool("stream", false, "respond with SSE chunks")
	workers := flag.Int("workers", 0, "bounded worker pool size (0 = unbounded)")
	flag.Parse()

	rand.Seed(time.Now().UnixNano())

	var sem chan struct{}
	if *workers > 0 {
		sem = make(chan struct{}, *workers)
		log.Printf("mock-upstream gated by %d-slot worker pool", *workers)
	}

	mux := http.NewServeMux()
	// Wildcard match — accept any path the gateway might synthesise
	// (it picks /chat/completions, /embeddings, /moderations,
	// /images/generations depending on the request). The benchmark
	// only ever sends /v1/chat/completions but we want zero 404 foot-
	// guns if upstream path resolution ever moves.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Bounded worker pool: block on entry, release on exit. The
		// gateway sees slow-to-respond connections instead of fatals
		// — that matches a real provider whose GPU pool is saturated.
		if sem != nil {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-r.Context().Done():
				http.Error(w, "client cancelled", http.StatusGatewayTimeout)
				return
			}
		}

		log.Printf("[mock] %s %s ua=%q", r.Method, r.URL.Path, r.Header.Get("User-Agent"))
		// Only handle POST; non-POST is for noise-free health-checks.
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		time.Sleep(time.Duration(*latencyMS) * time.Millisecond)
		if rand.Float64() < *errRate {
			http.Error(w, "mock upstream error", http.StatusInternalServerError)
			return
		}
		if *stream && strings.Contains(r.URL.Path, "chat") {
			if err := writeSSE(r.Context(), w, *ttftMS, *chunkMS, *tokens); err != nil {
				log.Printf("[mock] stream write error: %v", err)
			}
			return
		}
		writeJSON(w)
	})

	log.Printf("mock-upstream listening on %s (latency=%dms error-rate=%.2f stream=%v workers=%d)", *addr, *latencyMS, *errRate, *stream, *workers)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// asyncSleep returns immediately when ctx is cancelled, otherwise it waits
// for the configured duration. Streaming iteration relies on this so the
// gateway can disconnect mid-stream without us blocking forever.
func asyncSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// shared ticker pool: a single time.Ticker fanout via a broadcast chan
// keeps per-chunk sleep allocations off the hot path when -tokens grows.
// Currently unused — writeSSE uses asyncSleep — but kept as a pointer
// placeholder so a future tuning pass can wire it in without an import churn.
var tickerC <-chan time.Time

func writeSSE(parent context.Context, w http.ResponseWriter, ttftMS, chunkMS, tokens int) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	// 1) Honour the time-to-first-token so streaming latency is realistic.
	if err := asyncSleep(parent, time.Duration(ttftMS)*time.Millisecond); err != nil {
		return err
	}
	for i := 0; i < tokens; i++ {
		data := fmt.Sprintf(`{"choices":[{"delta":{"content":"chunk-%d"}}]}`, i)
		if _, err := io.WriteString(w, "data: "+data+"\n\n"); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		if chunkMS > 0 {
			if err := asyncSleep(parent, time.Duration(chunkMS)*time.Millisecond); err != nil {
				return err
			}
		}
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

func writeJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "mock-model",
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": "Hello from mock",
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     10,
			"completion_tokens": 4,
			"total_tokens":      14,
		},
	})
}
