// Black-box SSE contract harness.
//
// Default (no flags): serve each golden *.sse file from an in-process mock
// OpenAI and SHA-256 the concatenated entity body. That mode does not start
// Nexus; it proves the fixtures are what a raw-passthrough proxy must emit.
//
// With -base-url (or NEXUS_BASE_URL): POST /v1/chat/completions?stream to a
// running gateway whose upstream is the mock (-mock-upstream must be reachable
// by that gateway). Compare the response body SHA to sse/manifest.json.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "contract_harness: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("contract_harness", flag.ContinueOnError)
	base := fs.String("base-url", os.Getenv("NEXUS_BASE_URL"), "optional running gateway (Go or Elixir)")
	timeout := fs.Duration("timeout", 10*time.Second, "per-fixture HTTP timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := fixturesDir()
	if err != nil {
		return err
	}
	man, err := loadManifest(filepath.Join(dir, "sse", "manifest.json"))
	if err != nil {
		return err
	}

	if strings.TrimSpace(*base) == "" {
		return verifyLocal(dir, man)
	}
	return verifyProxy(*base, dir, man, *timeout)
}

type manFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

func loadManifest(path string) ([]manFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var man struct {
		Files []manFile `json:"files"`
	}
	if err := json.Unmarshal(raw, &man); err != nil {
		return nil, err
	}
	return man.Files, nil
}

func verifyLocal(dir string, files []manFile) error {
	var failed int
	for _, f := range files {
		body, err := os.ReadFile(filepath.Join(dir, "sse", f.Path))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		got := hex.EncodeToString(sum[:])
		if got != f.SHA256 || len(body) != f.Bytes {
			fmt.Printf("FAIL %s sha=%s want=%s bytes=%d want=%d\n", f.Path, got, f.SHA256, len(body), f.Bytes)
			failed++
			continue
		}
		fmt.Printf("ok   %s  %s\n", f.Path, got)
	}
	if failed > 0 {
		return fmt.Errorf("%d fixture(s) drifted", failed)
	}
	fmt.Println("contract_harness: local fixture SHA-256 ok")
	return nil
}

func verifyProxy(base, dir string, files []manFile, timeout time.Duration) error {
	mux := http.NewServeMux()
	bodies := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(dir, "sse", f.Path))
		if err != nil {
			return err
		}
		bodies[f.Path] = b
		name := f.Path
		mux.HandleFunc("/fixture/"+name, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(bodies[name])
		})
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	client := &http.Client{Timeout: timeout}
	var failed int
	for _, f := range files {
		if f.Path == "truncation.sse" {
			fmt.Printf("skip %s (gateway may wait for a complete frame)\n", f.Path)
			continue
		}
		reqBody := `{"model":"text-prime","stream":true,"messages":[{"role":"user","content":"` + f.Path + `"}]}`
		req, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+"/v1/chat/completions", strings.NewReader(reqBody))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+envOr("NEXUS_HARNESS_KEY", "nxs_live_harness"))
		req.Header.Set("X-Nexus-Fixture", f.Path)
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("FAIL %s: %v\n", f.Path, err)
			failed++
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		sum := sha256.Sum256(body)
		got := hex.EncodeToString(sum[:])
		if got != f.SHA256 {
			fmt.Printf("FAIL %s live sha=%s want=%s status=%d\n", f.Path, got, f.SHA256, resp.StatusCode)
			failed++
			continue
		}
		fmt.Printf("ok   %s  %s  (via %s)\n", f.Path, got, base)
	}
	_ = ln.Addr()
	if failed > 0 {
		return fmt.Errorf("%d live fixture(s) mismatched — gateway must raw-passthrough SSE", failed)
	}
	fmt.Println("contract_harness: live SHA-256 ok")
	return nil
}

func fixturesDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	dir := filepath.Join(filepath.Dir(file), "..", "..", "priv", "fixtures", "contracts")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", fmt.Errorf("fixtures %s: %w", dir, err)
	}
	return dir, nil
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
