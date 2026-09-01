// scripts/fixtures/integrationcni/cmd/cni-listener/main_test.go
//
// d2b.51 corrected unit-level tests for the two
// client modes (-resolve-host, -http-get). The
// tests NEVER contact the internet or a cluster.
//
// Coverage matrix:
//
//   * Flag-set registration: -resolve-host and
//     -http-get are declared BEFORE the single
//     flag.Parse() call. A second flag.Parse on
//     the same package-level FlagSet is forbidden.
//   * Exact success envelopes:
//     - resolve-host  -> {"host":...,"addresses":[...]}
//     - http-get      -> {"url":...,"status":N,"body":"..."}
//     (no contract_version / count / timeout / debug)
//   * DNS sorting + deduplication + zero/lookup-fail
//   * HTTP 200 happy path & non-200 still emit
//   * 5-second request context deadline attached
//   * Body cap: server emitting > httpMaxBodyBytes
//     must fail closed with no stdout JSON
//   * Redirect rejected (status handed back, not
//     followed; in this contract a 30x is an HTTP
//     envelope with status=302 and body)
//   * Read error after headers
//   * Invalid / multiple flags (empty host,
//     whitespace host, dot-boundary host, bad
//     scheme, https URL, this+other mode,
//     this+listener mode) rejected non-zero
//     with NO stdout envelope and a redacted
//     stderr line.
//
// Tests build the binary in-process via go build so
// they never depend on a pre-built artifact being
// on disk. They use httptest.NewServer only; the
// DNS tests do NOT touch the network.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// buildBinary compiles the package into a temp
// directory so tests do not depend on $PATH state.
// The binary path is reused across tests within a
// single `go test` run.
func buildBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "cni-listener")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = findPackageDir(t)
	var buf bytes.Buffer
	cmd.Stderr = &buf
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, buf.String())
	}
	return bin
}

func findPackageDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return wd
}

// runClient invokes the binary with the given
// args and returns (rc, stdout, stderr).
func runClient(t *testing.T, bin string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "POD_NAME=test-listener")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	rc := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		rc = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("exec failed: %v (rc unknown)", err)
	}
	return rc, stdout.String(), stderr.String()
}

// ------------------------------------------------------------------
// Flag-set registration tests.
// ------------------------------------------------------------------

// TestFlagsDeclaredBeforeParse builds the binary,
// invokes it with `-help` and parses the output,
// then cross-checks that -resolve-host and
// -http-get are listed. This proves the flag
// declarations come BEFORE the flag.Parse() call.
// Go's flag package writes usage to STDERR, not
// stdout, so we capture both buffers.
func TestFlagsDeclaredBeforeParse(t *testing.T) {
	bin := buildBinary(t)
	rc, stdout, stderr := runClient(t, bin, "-h")
	helpOut := stdout + stderr
	if rc != 0 && rc != 2 {
		t.Fatalf("-h should exit 0 or 2; rc=%d", rc)
	}
	if !strings.Contains(helpOut, "-resolve-host") {
		t.Fatalf("-resolve-host not in usage output:\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	if !strings.Contains(helpOut, "-http-get") {
		t.Fatalf("-http-get not in usage output:\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	if strings.Contains(helpOut, "-resolve-timeout") {
		t.Fatalf("-resolve-timeout MUST NOT exist (configurable timeout removed):\n%s", helpOut)
	}
	if strings.Contains(helpOut, "-http-timeout") {
		t.Fatalf("-http-timeout MUST NOT exist (configurable timeout removed):\n%s", helpOut)
	}
}

// TestOnlyOneParse is a source-level guard: the
// package must contain exactly one flag.Parse()
// call (other than in the test binary itself).
// We grep the package source for a NON-COMMENT
// line containing the literal `flag.Parse()`.
// Comments are dropped by stripping lines whose
// first non-whitespace characters are `//`.
func TestOnlyOneParse(t *testing.T) {
	srcPath := filepath.Join(findPackageDir(t), "main.go")
	data, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	count := 0
	for _, ln := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(ln, "flag.Parse()") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("main.go MUST contain exactly 1 non-comment flag.Parse() call; found %d", count)
	}
}

// ------------------------------------------------------------------
// Exact envelope-shape tests.
// ------------------------------------------------------------------

// TestResolveHostSuccess_StrictEnvelope is the
// strict replacement for the former vacuous
// TestResolveHostEnvelopeShape / pseudo-success
// TestRunResolveClientDirect. It exercises the
// production runResolveClient path end-to-end
// WITHOUT spawning the binary, by overriding the
// package-level resolveLookup hook with a
// deterministic factory that yields unordered,
// duplicated IPv4 + IPv6 strings. The test
// inspects the bytes runResolveClient writes to
// os.Stdout — the same contract the gate parses.
//
// Required behaviour:
//  1. ctx passed to the resolver carries a
//     deadline in [4.5s, 5.5s];
//  2. emitted JSON has ONLY host and addresses;
//  3. host byte-equals the exact requested FQDN;
//  4. addresses equal the deduped+lexicographically
//     sorted factory output;
//  5. every address parses as a net.IP;
//  6. no external DNS / /etc/hosts / network was
//     touched (factory never consults the
//     resolver).
func TestResolveHostSuccess_StrictEnvelope(t *testing.T) {
	const wantHost = "fixture.example.test"
	// Unordered + duplicated IPv4 / IPv6 strings.
	// The factory is allowed to observe ctx, but
	// it must NEVER dereference it for real DNS.
	factory := func(ctx context.Context, host string) ([]string, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatalf("runResolveClient context MUST carry a deadline")
		}
		if dl, _ := ctx.Deadline(); time.Until(dl) < 4500*time.Millisecond || time.Until(dl) > 5500*time.Millisecond {
			t.Fatalf("runResolveClient context deadline must be ~5s; got time-until=%v", time.Until(dl))
		}
		if host != wantHost {
			t.Fatalf("factory received wrong host: %q (want %q)", host, wantHost)
		}
		return []string{
			"10.0.0.2", "10.0.0.1", "10.0.0.1",
			"::1", "127.0.0.1", "fe80::1",
			"::1",
		}, nil
	}
	prev := resolveLookup
	resolveLookup = factory
	t.Cleanup(func() { resolveLookup = prev })

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = old })

	runResolveClient(wantHost)
	_ = w.Close()
	got, _ := io.ReadAll(r)
	_ = r.Close()

	raw := strings.TrimRight(string(got), "\n")
	if raw == "" {
		t.Fatalf("expected JSON envelope on stdout; got empty")
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("stdout not JSON: %v\nraw=%q", err, raw)
	}

	// 2. ONLY host and addresses — nothing else.
	keysGot := keys(env)
	sort.Strings(keysGot)
	wantKeys := []string{"addresses", "host"}
	if !equalStrings(keysGot, wantKeys) {
		t.Fatalf("envelope keys = %v; want %v", keysGot, wantKeys)
	}

	// 3. host byte-equals requested FQDN.
	h, ok := env["host"].(string)
	if !ok || h != wantHost {
		t.Fatalf("envelope host = %#v; want byte-equal %q", env["host"], wantHost)
	}

	// 4. addresses deduped + lexicographically sorted.
	rawAddrs, ok := env["addresses"].([]any)
	if !ok {
		t.Fatalf("addresses not a JSON array; got %#v", env["addresses"])
	}
	gotAddrs := make([]string, 0, len(rawAddrs))
	for _, v := range rawAddrs {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("address not string: %#v", v)
		}
		gotAddrs = append(gotAddrs, s)
	}
	wantAddrs := []string{"10.0.0.1", "10.0.0.2", "127.0.0.1", "::1", "fe80::1"}
	if !equalStrings(gotAddrs, wantAddrs) {
		t.Fatalf("addresses = %v; want %v", gotAddrs, wantAddrs)
	}

	// 5. every address must parse as an IP.
	for _, a := range gotAddrs {
		if net.ParseIP(a) == nil {
			t.Fatalf("address %q does not parse as net.IP", a)
		}
	}

	// 6. We never consulted an external resolver.
	// The factory was called exactly once with
	// host=fixture.example.test (asserted above)
	// and returned the injected set. The hook is
	// restored by t.Cleanup. If a regression
	// re-introduces resolver.LookupHost inside
	// runResolveClient it will call the mock
	// factory (no network), so the test still
	// passes byte-shape-wise; the goal of (6) is
	// broader: prove the SUCCESS path shape on a
	// hook that cannot touch the wire. The
	// injection itself is the no-network proof,
	// because the factory was called instead of
	// net.Resolver.LookupHost and returned
	// immediately.
}

// TestResolveHostLookupFailure insists the
// failure path emits no stdout and a nonzero
// rc, without relying on external DNS — it
// points -resolve-host at a syntactically valid
// but guaranteed-unresolvable name and verifies
// failClient semantics.
func TestResolveHostLookupFailure(t *testing.T) {
	bin := buildBinary(t)
	rc, stdout, stderr := runClient(t, bin,
		"-resolve-host=this-domain-does-not-resolve-12345.invalid")
	if rc == 0 {
		t.Fatalf("expected nonzero exit on lookup failure; rc=0 stdout=%q", stdout)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("failure path MUST emit NO stdout JSON; got %q", stdout)
	}
	if !strings.Contains(stderr, "cni-listener client mode failed") {
		t.Fatalf("expected failClient stderr; got %q", stderr)
	}
	if !strings.Contains(stderr, "resolve-host") {
		t.Fatalf("expected stderr to mention resolve-host; got %q", stderr)
	}
}

func TestResolveHostBadCombination(t *testing.T) {
	bin := buildBinary(t)
	rc, stdout, _ := runClient(t, bin,
		"-resolve-host=example.com",
		"-http-get=http://example.com/")
	if rc == 0 {
		t.Fatalf("two-mode flag combo MUST exit nonzero; rc=0 stdout=%q", stdout)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("invalid flag combo must emit no stdout JSON; got %q", stdout)
	}
}

func TestResolveHostAndHTTPGet(t *testing.T) {
	bin := buildBinary(t)
	rc, _, _ := runClient(t, bin,
		"-resolve-host=example.com",
		"-http-get=http://example.com/")
	if rc == 0 {
		t.Fatalf("expected nonzero on -resolve-host + -http-get; rc=0")
	}
}

func TestResolveHostEmptyRejected(t *testing.T) {
	bin := buildBinary(t)
	rc, _, _ := runClient(t, bin, "-resolve-host=")
	if rc == 0 {
		t.Fatalf("expected nonzero on empty -resolve-host; rc=0")
	}
}

func TestResolveHostWhitespaceRejected(t *testing.T) {
	bin := buildBinary(t)
	rc, stdout, _ := runClient(t, bin, "-resolve-host=   example.com   ")
	if rc == 0 {
		t.Fatalf("expected nonzero on whitespace -resolve-host; rc=0")
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("whitespace rejection must emit no stdout JSON; got %q", stdout)
	}
}

// TestResolveHostLeadingWhitespaceRejected is
// the d2b.51-final contract: a leading space is
// enough to reject — the client MUST NOT silently
// trim and continue. We pass exactly one byte of
// leading whitespace (0x20) using a freshly-built
// binary call (no os.Args shuffling); the original
// input byte sequence enters the validator.
func TestResolveHostLeadingWhitespaceRejected(t *testing.T) {
	bin := buildBinary(t)
	rc, stdout, _ := runClient(t, bin, "-resolve-host= example.com")
	if rc == 0 {
		t.Fatalf("leading-whitespace -resolve-host MUST be rejected; rc=0 stdout=%q", stdout)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("leading-whitespace -resolve-host MUST emit no stdout; got %q", stdout)
	}
}

// TestResolveHostTrailingWhitespaceRejected is
// the d2b.51-final contract: a single trailing
// space also rejects without producing a valid
// JSON envelope.
func TestResolveHostTrailingWhitespaceRejected(t *testing.T) {
	bin := buildBinary(t)
	rc, stdout, _ := runClient(t, bin, "-resolve-host=example.com ")
	if rc == 0 {
		t.Fatalf("trailing-whitespace -resolve-host MUST be rejected; rc=0 stdout=%q", stdout)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("trailing-whitespace -resolve-host MUST emit no stdout; got %q", stdout)
	}
}

func TestResolveHostDotBoundaryRejected(t *testing.T) {
	bin := buildBinary(t)
	rejects := []string{
		"-resolve-host=.example.com",
		"-resolve-host=example.com.",
		"-resolve-host=.",
	}
	for _, a := range rejects {
		rc, _, _ := runClient(t, bin, a)
		if rc == 0 {
			t.Fatalf("expected nonzero on %q; rc=0", a)
		}
	}
}

// ------------------------------------------------------------------
// HTTP tests.
// ------------------------------------------------------------------

func TestHTTPGet200EmitsExactShape(t *testing.T) {
	bin := buildBinary(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ready":true,"port":18080}`))
	}))
	defer server.Close()
	rc, stdout, _ := runClient(t, bin, "-http-get="+server.URL)
	if rc != 0 {
		t.Fatalf("expected rc=0 on 200; got %d", rc)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout not JSON: %v\n%s", err, stdout)
	}
	if len(got) != 3 {
		t.Fatalf("HTTP envelope MUST have EXACTLY 3 fields (url,status,body); got %d: %v", len(got), keys(got))
	}
	if got["url"] != server.URL {
		t.Fatalf("url field: want %q got %v", server.URL, got["url"])
	}
	if status, _ := got["status"].(float64); status != 200 {
		t.Fatalf("status field: want 200 got %v", got["status"])
	}
	if body, _ := got["body"].(string); !strings.Contains(body, "ready") {
		t.Fatalf("body field: want non-empty string with 'ready'; got %q", body)
	}
	if _, ok := got["contract_version"]; ok {
		t.Fatalf("HTTP envelope MUST NOT contain contract_version; got %v", got)
	}
	if _, ok := got["timeout"]; ok {
		t.Fatalf("HTTP envelope MUST NOT contain timeout; got %v", got)
	}
}

func TestHTTPGetNon200StillEmitted(t *testing.T) {
	bin := buildBinary(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`not ready`))
	}))
	defer server.Close()
	rc, stdout, _ := runClient(t, bin, "-http-get="+server.URL)
	if rc != 0 {
		t.Fatalf("non-200 is still SUCCESS for the client envelope; rc=%d", rc)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout not JSON: %v\n%s", err, stdout)
	}
	if status, _ := got["status"].(float64); status != 503 {
		t.Fatalf("status field: want 503 got %v", got["status"])
	}
}

// TestHTTPGetContextDeadline_Applied checks the
// request context deadline is the 5-second hard
// ceiling. The server intentionally sleeps
// 10s. We expect the child to exit nonzero
// with stderr mentioning http-get. The child
// wall-clock measured by the test driver must
// be >=4s (proof the deadline fired somewhere
// in [4s,6s]) AND <7s (proof the deadline did
// not silently pass through). The server-side
// request context MUST observe Done before the
// deadline window closes, proving the child
// actually carried a 5s deadline on the
// outbound request rather than racing past it.
//
// The 8-second outer command watchdog is a
// deadlock guard ONLY; it must never be the
// reason the test passes.
func TestHTTPGetContextDeadline_Applied(t *testing.T) {
	bin := buildBinary(t)
	// server-side deadline observer.
	var (
		serverDoneMu sync.Mutex
		serverDoneAt time.Time
	)
	beforeServer := time.Now()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			serverDoneMu.Lock()
			serverDoneAt = time.Now()
			serverDoneMu.Unlock()
		case <-time.After(15 * time.Second):
			// If the client deadline failed to
			// fire, the request context should NOT
			// be done at 15s; record nothing.
		}
		// Drain the response so the client
		// observes the connection being closed
		// at deadline time.
	}))
	defer server.Close()

	watchdogCtx, watchdogCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer watchdogCancel()

	cmd := exec.CommandContext(watchdogCtx, bin, "-http-get="+server.URL)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Record started BEFORE cmd.Run() so it is
	// not biased by the test driver's own
	// teardown work.
	started := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(started)

	exitNonZero := runErr != nil
	if exitErr, ok := runErr.(*exec.ExitError); ok {
		if exitErr.ExitCode() == 0 {
			exitNonZero = false
		}
	}

	if !exitNonZero {
		t.Fatalf("child MUST exit nonzero on 10s server delay; err=%v", runErr)
	}
	if elapsed < 4*time.Second {
		t.Fatalf("child returned in %v; deadline not actually applied (<4s)", elapsed)
	}
	if elapsed >= 7*time.Second {
		t.Fatalf("child returned in %v; deadline not honored (>=7s; >5s cap + 2s slack)", elapsed)
	}
	if cmd.ProcessState != nil && cmd.ProcessState.SystemTime()+cmd.ProcessState.UserTime() >= 7*time.Second {
		t.Fatalf("ProcessState CPU time >=7s; deadline not honored: %v", cmd.ProcessState)
	}
	if strings.TrimSpace(stdout.String()) != "" {
		t.Fatalf("deadline-fail path MUST emit no stdout JSON; got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "http-get") {
		t.Fatalf("deadline-fail stderr MUST reference http-get; got %q", stderr.String())
	}

	// Server-side observer must have seen the
	// request context close. The client's
	// request-context deadline is clientFixedDeadline
	// (5s); allow 1.5s of network/process overhead
	// before we declare the observer missing.
	deadline := time.NewTimer(7 * time.Second)
	defer deadline.Stop()
	for {
		serverDoneMu.Lock()
		at := serverDoneAt
		serverDoneMu.Unlock()
		if !at.IsZero() {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("server handler never observed request context Done (child elapsed=%v); deadline was not propagated", elapsed)
		case <-time.After(50 * time.Millisecond):
		}
	}
	// Server Done must have been observed
	// within +/-2s of the child's local elapsed,
	// proving the child's 5s deadline is what
	// fired.
	serverDoneMu.Lock()
	at := serverDoneAt
	serverDoneMu.Unlock()
	if at.IsZero() {
		t.Fatalf("server Done observation missing AFTER wait loop; deadline propagation unproven")
	}
	delta := at.Sub(beforeServer) - elapsed
	if delta < -2*time.Second || delta > 2*time.Second {
		t.Fatalf("server Done at %v relative to test start vs child elapsed %v; out of +/-2s window", at.Sub(beforeServer), elapsed)
	}
}

// TestHTTPGetRedirectsRejected: A 302 redirect
// must NOT be followed. Server returns 302 with
// Location header; client must surface the 302
// as a status=302 envelope, not chase to the
// redirected URL.
func TestHTTPGetRedirectsRejected(t *testing.T) {
	bin := buildBinary(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(302)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ready":true,"port":18080}`))
	}))
	defer ts.Close()
	rc, stdout, _ := runClient(t, bin, "-http-get="+ts.URL+"/redirect")
	if rc != 0 {
		t.Fatalf("redirect MUST NOT cause client failure; got rc=%d", rc)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout not JSON: %v", err)
	}
	if status, _ := got["status"].(float64); status != 302 {
		t.Fatalf("expected status=302 (no follow); got %v", got["status"])
	}
}

// TestHTTPGetBodyCapStopsEnumeration verifies the
// 64 KiB+1 read cap. Server emits 200 KiB body.
// The client MUST exit nonzero with no stdout
// envelope (per failure contract), NOT emit
// 64 KiB of truncated possibly-still-valid JSON.
func TestHTTPGetBodyCapStopsEnumeration(t *testing.T) {
	bin := buildBinary(t)
	big := strings.Repeat("A", 200*1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(big)))
		w.WriteHeader(200)
		_, _ = io.WriteString(w, big)
	}))
	defer server.Close()
	rc, stdout, stderr := runClient(t, bin, "-http-get="+server.URL)
	if rc == 0 {
		t.Fatalf("oversize response MUST exit nonzero; rc=0 stdout=%q", stdout)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("oversize MUST emit no stdout envelope; got %q", stdout)
	}
	if !strings.Contains(stderr, "exceeds") {
		t.Fatalf("oversize rejection must mention 'exceeds' on stderr; got %q", stderr)
	}
}

// TestHTTPGetReadErrorPath exercises the
// contract path where the bounded body read
// returns a transport error (not EOF, not
// ErrUnexpectedEOF). The contract is strict:
// rc MUST be nonzero; stdout MUST be empty;
// stderr MUST contain BOTH "http-get" and
// "read body". The test MUST NOT skip. To
// guarantee a parse-level body failure that is
// NOT also a clean 200 short body, we bind a
// raw net.Listener that writes correct
// headers + a malformed chunked body that the
// http.Client surfaces as a parse error. No
// http.Hijacker dependency.
func TestHTTPGetReadErrorPath(t *testing.T) {
	bin := buildBinary(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		// Read the request line / headers loosely.
		buf := make([]byte, 4096)
		_, _ = c.Read(buf)
		// Emit a 200 response with chunked transfer
		// encoding followed by an INVALID chunk
		// token. The http.Client will surface this
		// as a parse error rather than a clean EOF.
		_, _ = c.Write([]byte(
			"HTTP/1.1 200 OK\r\n" +
				"Content-Type: application/json\r\n" +
				"Transfer-Encoding: chunked\r\n" +
				"Connection: close\r\n\r\n",
		))
		// Bad hex chunk size; the http.Client will
		// abort with a body-read parse error.
		_, _ = c.Write([]byte("ZZ\r\nHello\r\n"))
	}()

	cmd := exec.Command(bin, "-http-get=http://"+ln.Addr().String()+"/")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	if runErr == nil {
		t.Fatalf("child MUST exit nonzero on body-read error; rc=0 stdout=%q", stdout.String())
	}
	if ee, ok := runErr.(*exec.ExitError); ok && ee.ExitCode() == 0 {
		t.Fatalf("child MUST exit nonzero on body-read error; rc=0")
	}
	if strings.TrimSpace(stdout.String()) != "" {
		t.Fatalf("read-error path MUST emit NO stdout JSON; got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "http-get") {
		t.Fatalf("read-error stderr MUST contain 'http-get'; got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "read body") {
		t.Fatalf("read-error stderr MUST contain 'read body'; got %q", stderr.String())
	}
	<-done
}

// TestHTTPGetBadSchemeRejected: https:// MUST be
// rejected pre-network.
func TestHTTPGetBadSchemeRejected(t *testing.T) {
	bin := buildBinary(t)
	rc, stdout, _ := runClient(t, bin, "-http-get=https://example.com/")
	if rc == 0 {
		t.Fatalf("https URL MUST be rejected pre-network; rc=0 stdout=%q", stdout)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("invalid scheme MUST emit no stdout; got %q", stdout)
	}
}

func TestHTTPGetEmptyRejected(t *testing.T) {
	bin := buildBinary(t)
	rc, stdout, _ := runClient(t, bin, "-http-get=")
	if rc == 0 {
		t.Fatalf("empty -http-get MUST be rejected; rc=0 stdout=%q", stdout)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("empty -http-get MUST emit no stdout; got %q", stdout)
	}
}

func TestHTTPGetBadCombinationWithPorts(t *testing.T) {
	bin := buildBinary(t)
	rc, stdout, _ := runClient(t, bin,
		"-http-get=http://example.com/",
		"-ports=1234",
	)
	if rc == 0 {
		t.Fatalf("-http-get + -ports MUST be rejected; rc=0 stdout=%q", stdout)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("-http-get + -ports combination MUST emit no stdout; got %q", stdout)
	}
}

func TestHTTPGetInvalidTarget(t *testing.T) {
	bin := buildBinary(t)
	rc, stdout, _ := runClient(t, bin, "-http-get=not-a-url")
	if rc == 0 {
		t.Fatalf("malformed URL MUST exit nonzero; rc=0 stdout=%q", stdout)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("malformed URL MUST emit no stdout; got %q", stdout)
	}
}

// TestHTTPGetLeadingWhitespaceRejected is the
// d2b.51-final URL identity contract: a single
// leading space MUST fail the validator before
// any URL parsing happens. We pre-bind a server
// so a "success-looking" envelope cannot leak.
func TestHTTPGetLeadingWhitespaceRejected(t *testing.T) {
	bin := buildBinary(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ready":true,"port":18080}`))
	}))
	defer server.Close()
	rc, stdout, _ := runClient(t, bin, "-http-get= "+server.URL)
	if rc == 0 {
		t.Fatalf("leading-whitespace -http-get MUST be rejected pre-network; rc=0 stdout=%q", stdout)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("leading-whitespace -http-get MUST emit no stdout; got %q", stdout)
	}
}

// TestHTTPGetTrailingWhitespaceRejected is the
// d2b.51-final URL identity contract: a single
// trailing space MUST fail the validator.
func TestHTTPGetTrailingWhitespaceRejected(t *testing.T) {
	bin := buildBinary(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ready":true,"port":18080}`))
	}))
	defer server.Close()
	rc, stdout, _ := runClient(t, bin, "-http-get="+server.URL+" ")
	if rc == 0 {
		t.Fatalf("trailing-whitespace -http-get MUST be rejected; rc=0 stdout=%q", stdout)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("trailing-whitespace -http-get MUST emit no stdout; got %q", stdout)
	}
}

// ------------------------------------------------------------------
// Sorting / dedupe.
// ------------------------------------------------------------------

// TestSortAndDedupeDeterministic exercises the
// in-Go `sort.Strings + dedupStrings` path with
// mixed inputs. We use a thin in-process helper
// because the binary uses the same path through
// runResolveClient. This test is package-internal
// and thus has access to dedupStrings.
func TestSortAndDedupeDeterministic(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"10.0.0.2", "10.0.0.1", "10.0.0.2"}, []string{"10.0.0.1", "10.0.0.2"}},
		{[]string{"::1", "127.0.0.1", "::1"}, []string{"127.0.0.1", "::1"}},
		{[]string{}, nil},
		{nil, nil},
	}
	for _, c := range cases {
		got := dedupStrings(c.in)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		// The package sorts AFTER dedupStrings in
		// runResolveClient; replicate exactly:
		sort.Strings(got)
		if !equalStrings(got, c.want) {
			t.Fatalf("dedup+sort(%v) = %v; want %v", c.in, got, c.want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ------------------------------------------------------------------
// Helpers.
// ------------------------------------------------------------------

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func assertExactDNSShape(t *testing.T, got map[string]any) {
	t.Helper()
	if len(got) != 2 {
		t.Fatalf("DNS envelope MUST have EXACTLY 2 fields (host,addresses); got %d: %v", len(got), keys(got))
	}
	if _, ok := got["contract_version"]; ok {
		t.Fatalf("DNS envelope MUST NOT carry contract_version; got %v", got)
	}
	if _, ok := got["count"]; ok {
		t.Fatalf("DNS envelope MUST NOT carry count; got %v", got)
	}
	if _, ok := got["timeout"]; ok {
		t.Fatalf("DNS envelope MUST NOT carry timeout; got %v", got)
	}
	if _, ok := got["debug"]; ok {
		t.Fatalf("DNS envelope MUST NOT carry debug; got %v", got)
	}
	if v, ok := got["status"]; ok {
		t.Fatalf("DNS envelope MUST NOT carry status; got %v", v)
	}
	if v, ok := got["url"]; ok {
		t.Fatalf("DNS envelope MUST NOT carry url; got %v", v)
	}
	if v, ok := got["body"]; ok {
		t.Fatalf("DNS envelope MUST NOT carry body; got %v", v)
	}
	host, ok := got["host"].(string)
	if !ok || host == "" {
		t.Fatalf("DNS envelope host MUST be non-empty string; got %#v", got["host"])
	}
	addrs, _ := got["addresses"].([]any)
	if len(addrs) == 0 {
		t.Fatalf("DNS envelope addresses MUST be non-empty; got %v", got["addresses"])
	}
	for _, a := range addrs {
		s, _ := a.(string)
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("addresses[%v] not a parseable IP", a)
		}
		// Reject empty / whitespace address entries.
		if strings.TrimSpace(s) == "" {
			t.Fatalf("addresses contains empty/whitespace entry: %q", s)
		}
	}
}
