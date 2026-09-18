// Load baseline for the OpenAI chat SSE hot path.
//
// Same command is re-run against Go and Elixir (NEXUS_BASE_URL). Mock
// upstream is the default so numbers are not vendor-latency.
//
//	# persistent mock (gate script)
//	go run ./scripts/load_baseline -mock-listen 127.0.0.1:19090
//
//	# hit a running gateway; RSS is the server PID, not this client
//	go run ./scripts/load_baseline -base-url http://127.0.0.1:18080 \
//	  -pid $SERVER_PID -streams 1000 -duration 15s -out /tmp/go-sse.json
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
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	base := fs.String("base-url", os.Getenv("NEXUS_BASE_URL"), "gateway under test; empty = mock-upstream only")
	streams := fs.Int("streams", 32, "concurrent SSE clients")
	seconds := fs.Duration("duration", 10*time.Second, "how long to keep streams open")
	out := fs.String("out", "", "write JSON report to this path (default stdout)")
	live := fs.Bool("live-provider", os.Getenv("NEXUS_LOAD_LIVE") == "1", "hit a real provider; default is mock upstream")
	pid := fs.Int("pid", 0, "server process to sample RSS from (0 = this process)")
	model := fs.String("model", envOr("NEXUS_LOAD_MODEL", "gpt-4o-mini"), "chat model id")
	mockListen := fs.String("mock-listen", "", "if set, only serve the mock SSE upstream on this addr and block")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if addr := strings.TrimSpace(*mockListen); addr != "" {
		return serveMock(addr)
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
	if !*live && target == "" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		defer ln.Close()
		go http.Serve(ln, http.HandlerFunc(mockSSE))
		mockAddr = "http://" + ln.Addr().String()
		target = mockAddr
	}

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        *streams + 32,
			MaxIdleConnsPerHost: *streams + 32,
			MaxConnsPerHost:     *streams + 32,
			ForceAttemptHTTP2:   false,
			DisableCompression:  true,
		},
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
		ServerPID:    *pid,
		Model:        *model,
		Notes:        []string{"P1-3 gate: Elixir TTFT p50/p99 ≤ Go×1.10; RSS/stream ≤ Go×1.20; errors=0."},
		DisconnectOK: true,
	}

	var peak atomic.Int64
	stopRSS := make(chan struct{})
	go sampleRSS(*pid, &peak, stopRSS)

	ttft := make([]time.Duration, 0, *streams)
	var ttftMu sync.Mutex
	var errs atomic.Int64
	var wg sync.WaitGroup
	deadline := time.Now().Add(*seconds)

	for i := 0; i < *streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := oneStream(client, target, *model, deadline)
			if err != nil {
				errs.Add(1)
				return
			}
			ttftMu.Lock()
			ttft = append(ttft, d)
			ttftMu.Unlock()
		}()
	}
	wg.Wait()
	close(stopRSS)

	report.Completed = len(ttft)
	report.Errors = int(errs.Load())
	report.TTFT = summarize(ttft)
	report.RSSKb = peak.Load()
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

func serveMock(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "load_baseline: mock SSE on http://%s\n", ln.Addr().String())
	srv := &http.Server{Handler: http.HandlerFunc(mockSSE)}
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		<-c
		_ = srv.Close()
	}()
	err = srv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
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
	ServerPID      int         `json:"server_pid,omitempty"`
	Model          string      `json:"model"`
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
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	fmt.Fprint(w, "data: {\"id\":\"chatcmpl-load\",\"object\":\"chat.completion.chunk\",\"created\":1720000000,\"model\":\"gpt-4o-mini\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"x\"},\"finish_reason\":null}]}\n\n")
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

func oneStream(client *http.Client, target, model string, deadline time.Time) (time.Duration, error) {
	url := strings.TrimRight(target, "/") + "/v1/chat/completions"
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	body := fmt.Sprintf(`{"model":%q,"stream":true,"messages":[{"role":"user","content":"load"}]}`, model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+envOr("NEXUS_HARNESS_KEY", "nxs_live_harness"))
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("status %d: %s", resp.StatusCode, b)
	}
	br := bufio.NewReader(resp.Body)
	_, err = br.ReadByte()
	if err != nil {
		return 0, err
	}
	ttft := time.Since(start)
	_, _ = io.Copy(io.Discard, br)
	return ttft, nil
}

func sampleRSS(pid int, peak *atomic.Int64, stop <-chan struct{}) {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if v := rssKB(pid); v > peak.Load() {
			peak.Store(v)
		}
		select {
		case <-stop:
			if v := rssKB(pid); v > peak.Load() {
				peak.Store(v)
			}
			return
		case <-tick.C:
		}
	}
}

func cgroupAvailable() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	_, err := os.Stat("/sys/fs/cgroup/memory.current")
	return err == nil
}

func rssKB(pid int) int64 {
	if pid <= 0 {
		pid = os.Getpid()
	}
	if runtime.GOOS == "linux" {
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "VmRSS:") {
					f := strings.Fields(line)
					if len(f) >= 2 {
						n, _ := strconv.ParseInt(f[1], 10, 64)
						return n
					}
				}
			}
		}
	}
	cmd := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid))
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
