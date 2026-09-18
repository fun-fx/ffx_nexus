// Load baseline for the OpenAI chat SSE hot path.
//
// Same command is re-run against Elixir (NEXUS_BASE_URL). Mock upstream is
// the default so numbers are not vendor-latency. Set NEXUS_LOAD_LIVE=1 to
// hit a real provider through the gateway (split recorded in the JSON
// report).
//
// Linux: if /sys/fs/cgroup is writable, RSS is read from memory.current.
// Darwin has no cgroup; the report records process RSS via ps and marks
// cgroup=false so the two OS numbers are not compared as one series.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "load_baseline: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("load_baseline", flag.ContinueOnError)
	base := fs.String("base-url", envOr("NEXUS_BASE_URL", ""), "gateway under test; empty = mock-upstream only (no proxy)")
	streams := fs.Int("streams", 32, "concurrent SSE clients")
	seconds := fs.Duration("duration", 10*time.Second, "how long to keep streams open")
	out := fs.String("out", "", "write JSON report to this path (default stdout)")
	live := fs.Bool("live-provider", os.Getenv("NEXUS_LOAD_LIVE") == "1", "hit a real provider; default is mock upstream")
	if err := fs.Parse(args); err != nil {
		return err
	}

	target := strings.TrimSpace(*base)
	upstream := "mock"
	if *live {
		upstream = "live-provider"
		if target == "" {
			return fmt.Errorf("-live-provider requires -base-url / NEXUS_BASE_URL")
		}
	}

	var mockAddr string
	if !*live {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		defer ln.Close()
		go http.Serve(ln, http.HandlerFunc(mockSSE))
		mockAddr = "http://" + ln.Addr().String()
		if target == "" {
			target = mockAddr
		}
	}

	report := Report{
		TS:           time.Now().UTC().Format(time.RFC3339),
		GOOS:         runtime.GOOS,
		GOARCH:       runtime.GOARCH,
		Target:       target,
		Upstream:     upstream,
		MockAddr:     mockAddr,
		Streams:      *streams,
		Duration:     seconds.String(),
		Cgroup:       cgroupAvailable(),
		Notes:        []string{"P1-3 gate: Elixir TTFT p50/p99 and RSS/stream must be non-inferior to this report on the same machine and cgroup."},
		DisconnectOK: true,
	}

	ttft := make([]time.Duration, 0, *streams)
	var ttftMu sync.Mutex
	var leaks atomic.Int64
	var wg sync.WaitGroup
	deadline := time.Now().Add(*seconds)

	for i := 0; i < *streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := oneStream(target, deadline)
			if err != nil {
				leaks.Add(1)
				return
			}
			ttftMu.Lock()
			ttft = append(ttft, d)
			ttftMu.Unlock()
		}()
	}
	wg.Wait()

	report.Completed = len(ttft)
	report.Errors = int(leaks.Load())
	report.TTFT = summarize(ttft)
	report.RSSKb = rssKB()
	if report.Completed > 0 {
		report.RSSPerStreamKb = float64(report.RSSKb) / float64(report.Completed)
	}
	report.DisconnectOK = report.Errors == 0

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		enc = json.NewEncoder(f)
		enc.SetIndent("", "  ")
	}
	return enc.Encode(report)
}

type Report struct {
	TS             string      `json:"ts"`
	GOOS           string      `json:"goos"`
	GOARCH         string      `json:"goarch"`
	Target         string      `json:"target"`
	Upstream       string      `json:"upstream"`
	MockAddr       string      `json:"mock_addr,omitempty"`
	Streams        int         `json:"streams"`
	Duration       string      `json:"duration"`
	Completed      int         `json:"completed"`
	Errors         int         `json:"errors"`
	DisconnectOK   bool        `json:"disconnect_cleanup_ok"`
	Cgroup         bool        `json:"cgroup"`
	TTFT           Percentiles `json:"ttft_ns"`
	RSSKb          int64       `json:"rss_kb"`
	RSSPerStreamKb float64     `json:"rss_per_stream_kb"`
	Notes          []string    `json:"notes"`
}

type Percentiles struct {
	P50 int64 `json:"p50"`
	P95 int64 `json:"p95"`
	P99 int64 `json:"p99"`
	Max int64 `json:"max"`
}

func summarize(ds []time.Duration) Percentiles {
	if len(ds) == 0 {
		return Percentiles{}
	}
	ns := make([]int, len(ds))
	for i, d := range ds {
		ns[i] = int(d)
	}
	sort.Ints(ns)
	pct := func(p int) int64 {
		idx := (p * (len(ns) - 1)) / 100
		return int64(ns[idx])
	}
	return Percentiles{P50: pct(50), P95: pct(95), P99: pct(99), Max: int64(ns[len(ns)-1])}
}

func mockSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	fmt.Fprint(w, "data: {\"id\":\"chatcmpl-load\",\"object\":\"chat.completion.chunk\",\"created\":1720000000,\"model\":\"text-prime\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"x\"},\"finish_reason\":null}]}\n\n")
	if fl != nil {
		fl.Flush()
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\".\"}}]}\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

func oneStream(target string, deadline time.Time) (time.Duration, error) {
	url := strings.TrimRight(target, "/") + "/v1/chat/completions"
	if !strings.Contains(target, "/v1/") && strings.HasPrefix(target, "http://127.0.0.1") {
		// mock server serves the handler on any path
		url = strings.TrimRight(target, "/") + "/"
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	body := `{"model":"text-prime","stream":true,"messages":[{"role":"user","content":"load"}]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+envOr("NEXUS_HARNESS_KEY", "nxs_live_harness"))
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	_, err = br.ReadByte()
	if err != nil {
		return 0, err
	}
	ttft := time.Since(start)
	_, _ = io.Copy(io.Discard, br)
	return ttft, nil
}

func cgroupAvailable() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	_, err := os.Stat("/sys/fs/cgroup/memory.current")
	return err == nil
}

func rssKB() int64 {
	if runtime.GOOS == "linux" {
		if b, err := os.ReadFile("/sys/fs/cgroup/memory.current"); err == nil {
			n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
			return n / 1024
		}
	}
	cmd := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid()))
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return n
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
