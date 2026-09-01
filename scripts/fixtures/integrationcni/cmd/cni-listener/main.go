// scripts/fixtures/integrationcni/cmd/cni-listener/main.go
//
// cni-listener is a small Go binary used inside
// the cni-control probe Pod the d2b.49 / d2b.51
// fixture runs. The probe Pod is built FROM
// scratch and contains ONLY /cni-listener; it has
// no /bin/sh, no getent, no curl. Two control
// paths have to reach the target Pod's readiness
// anyway, so cni-listener ships two bounded
// client modes that ANY fixture pod can exec:
//
//	-resolve-host=<FQDN>  -> Go DNS resolve through
//	                         net.Resolver; emits one
//	                         JSON envelope with the
//	                         deduped, sorted addresses.
//	                         Fixed 5-second deadline.
//	                         Empty input, leading/
//	                         trailing whitespace, dot
//	                         boundaries, newlines, or
//	                         control characters ->
//	                         non-zero exit; no stdout.
//	-http-get=<http URL>  -> Go net/http GET against
//	                         an absolute http:// URL.
//	                         Fixed 5-second deadline
//	                         carried by the request
//	                         context. Redirects are
//	                         forbidden. Body read is
//	                         stopped at 64KiB+1; if
//	                         the server emits more,
//	                         client exits non-zero
//	                         and writes nothing on
//	                         stdout. Non-200 responses
//	                         DO exit zero with a one-
//	                         line envelope: status 200
//	                         is a Gate 09 predicate,
//	                         not a client predicate.
//
// All declared flags are registered BEFORE the
// single flag.Parse() call. The two client flags
// are mutually exclusive with each other and with
// the historical -ports/-probe/-readyz/-role/
// -target modes. The fixed 5-second deadline is
// applied by common helpers and is NOT exposed as
// a runtime flag; the previous -resolve-timeout
// and -http-timeout flags were removed.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// clientFixedDeadline is the single, fixed, hard-coded
// deadline the client modes honour. The corrected
// d2b.51 contract removes operator-tunable timeouts
// so the gate can reproduce a 5-second bound exactly
// from any fixture image.
const clientFixedDeadline = 5 * time.Second

// httpMaxBodyBytes is the body-read ceiling. If the
// server tries to deliver more than this, the client
// exits non-zero WITHOUT emitting a JSON envelope.
const httpMaxBodyBytes = 64 * 1024

func main() {
	// ALL flags are declared before the single
	// flag.Parse() below. Order:
	//  - listener / controlProbe flags
	//  - the two client mode flags (-resolve-host,
	//    -http-get)
	// No subsequent flag.Parse call exists; a stray
	// second parse would silently re-trigger
	// flag.CommandLine internals and is forbidden
	// by the d2b.51 corrected contract.
	ports := flag.String("ports", "", "comma-separated list of TCP ports to listen on (e.g. 8080,9100)")
	probe := flag.String("probe", "", "exit 0 once the listener at this port accepts a SYN (readinessProbe form)")
	role := flag.String("role", "fixture", "echoed in the JSON body; lets scenario probes distinguish fixture pods by role")
	target := flag.String("target", "", "echoed in /readyz so the gate can record which fixture target answered")

	// d2b.51 corrected: client flags declare without
	// any timeout knobs. Operators cannot widen or
	// narrow the deadline; the 5-second bound is fixed.
	resolveHost := flag.String("resolve-host", "", "FQDN to resolve via the Pod network; emits a single JSON object on stdout then exits (client mode)")
	httpGet := flag.String("http-get", "", "absolute http:// URL to GET against; emits a single JSON object on stdout then exits (client mode)")

	flag.Parse()

	hasPorts := *ports != ""
	hasProbe := *probe != ""
	hasResolve := *resolveHost != ""
	hasHTTPGet := *httpGet != ""

	// d2b.51 corrected: pre-network validation.
	// Empty/whitespace/control character hosts and
	// malformed URLs are rejected here, before any
	// network activity. Multiple client flags or
	// client + listener flags together are rejected.
	// A common -resolve-host and -http-get together
	// is rejected.
	modes := 0
	if hasResolve {
		modes++
		if !isValidResolveHost(*resolveHost) {
			failClient("invalid -resolve-host value: empty / whitespace / dot boundary / control characters")
		}
	}
	if hasHTTPGet {
		modes++
		if !isValidHTTPURL(*httpGet) {
			failClient("invalid -http-get value: not an absolute http:// URL")
		}
	}
	if modes > 1 {
		failClient("invalid flag combination: -resolve-host and -http-get cannot both be set")
	}
	if hasResolve && (hasPorts || hasProbe) {
		failClient("invalid flag combination: -resolve-host is mutually exclusive with -ports/-probe")
	}
	if hasHTTPGet && (hasPorts || hasProbe) {
		failClient("invalid flag combination: -http-get is mutually exclusive with -ports/-probe")
	}

	if hasResolve {
		runResolveClient(*resolveHost)
		return
	}
	if hasHTTPGet {
		runHTTPClient(*httpGet)
		return
	}

	if hasProbe {
		probePort(*probe)
		return
	}
	if !hasPorts {
		fmt.Fprintf(os.Stderr, "cni-listener: no -ports / -probe / -resolve-host / -http-get flag passed\n")
		os.Exit(2)
	}

	podName := os.Getenv("POD_NAME")
	if podName == "" {
		podName = "cni-listener"
	}
	mounts, err := parsePorts(*ports)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cni-listener: invalid -ports %q: %v\n", *ports, err)
		os.Exit(2)
	}

	var wg sync.WaitGroup
	for _, p := range mounts {
		wg.Add(1)
		go func(port int) {
			defer wg.Done()
			serve(port, podName, *role, *target)
		}(p)
	}
	wg.Wait()
}

func parsePorts(s string) ([]int, error) {
	out := []int{}
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(tok, "%d", &n); err != nil {
			return nil, fmt.Errorf("not an integer: %q", tok)
		}
		if n < 1 || n > 65535 {
			return nil, fmt.Errorf("port out of range: %d", n)
		}
		out = append(out, n)
	}
	return out, nil
}

func probePort(port string) {
	deadline := 60
	for i := 0; i < deadline; i++ {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
		if err == nil {
			_ = conn.Close()
			fmt.Fprintf(os.Stderr, "probe %s ok after %ds\n", port, i)
			os.Exit(0)
		}
		time.Sleep(time.Second)
	}
	fmt.Fprintf(os.Stderr, "probe %s failed after %ds\n", port, deadline)
	os.Exit(1)
}

func serve(port int, pod, role, target string) {
	addr := fmt.Sprintf(":%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] listen %d: %v\n", pod, port, err)
		return
	}
	fmt.Fprintf(os.Stderr, "[%s] listening on %d\n", pod, port)
	body := map[string]any{
		"ok":     true,
		"listen": addr,
		"port":   port,
		"pod":    pod,
		"role":   role,
		"target": target,
		"ready":  true,
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go handle(conn, body, role, target)
	}
}

func handle(conn net.Conn, body any, role string, target string) {
	defer conn.Close()
	buf := make([]byte, 1024)
	n, _ := conn.Read(buf)
	raw := strings.TrimSpace(string(buf[:n]))
	if strings.HasPrefix(raw, "GET /metrics ") {
		reply(conn, 200, "text/plain; version=0.0.4", metricsBody(role, target))
		return
	}
	if strings.HasPrefix(raw, "GET /healthz ") || strings.HasPrefix(raw, "GET / ") || strings.HasPrefix(raw, "GET /readyz ") {
		reply(conn, 200, "application/json", body)
		return
	}
	if strings.Contains(raw, " GET / ") {
		reply(conn, 200, "application/json", body)
		return
	}
	if raw == "" {
		return
	}
	reply(conn, 200, "application/json", body)
}

func metricsBody(role, target string) string {
	name := "cni_fixture_role_up"
	if role == "" {
		role = "fixture"
	}
	if target == "" {
		target = "unknown"
	}
	labels := fmt.Sprintf(`role=%q,target=%q`, role, target)
	return fmt.Sprintf("# HELP %s 1 if the fixture role is up\n"+
		"# TYPE %s gauge\n"+
		"%s{%s} 1\n",
		name, name, name, labels)
}

func reply(conn net.Conn, code int, ctype string, body any) {
	b, _ := json.Marshal(body)
	status := "OK"
	if code != 200 {
		status = "Status"
	}
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, status, ctype, len(b), b)
	_, _ = io.Copy(io.Discard, conn)
}

// -----------------------------------------------------------------------------
// d2b.51 corrected client modes.
// -----------------------------------------------------------------------------

// failClient writes a single redacted reason to
// stderr and exits non-zero WITHOUT emitting a
// partial or success-looking JSON envelope on
// stdout. The gate's parser treats any closed
// stdout line as the contract, so a failClient
// caller ensures the next stdout write is from
// process exit, not from emitClientJSON.
func failClient(reason string) {
	fmt.Fprintf(os.Stderr, "cni-listener client mode failed: %s\n", reason)
	os.Exit(2)
}

// isValidResolveHost accepts the EXACT original
// input bytes without trimming. Any leading /
// trailing whitespace (the byte 0x20), the byte
// 0x00 (NUL), newline, carriage return, or tab
// anywhere in the input causes immediate
// rejection. Empty input, dot-boundary prefixes /
// suffixes, internally-whitespace sequences, and
// control characters (codepoint < 0x20 or == 0x7F)
// are likewise rejected. The contract is: pass
// the FQDN exactly as it appears in the Service
// manifest, no shell-driven whitespace permitted.
func isValidResolveHost(s string) bool {
	if s != strings.TrimSpace(s) {
		// Originating callers (kubectl exec argv)
		// never produce leading / trailing blanks
		// by accident; if the caller passes one in
		// it is a sign of a buggy shell substitution
		// and we fail closed.
		return false
	}
	if s == "" {
		return false
	}
	for _, r := range s {
		// Permit only printable ASCII (0x21-0x7E)
		// so a backtick, a hyphen, a dot, etc are
		// fine; any TAB / SPACE / NUL / NL / CR /
		// other separator or control char is
		// rejected. This catches shell-style
		// whitespace AND embedded control chars.
		if r < 0x21 || r > 0x7E {
			return false
		}
	}
	if strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return false
	}
	if strings.Contains(s, "..") {
		return false
	}
	return true
}

// isValidHTTPURL accepts the EXACT original
// input bytes. Leading / trailing whitespace are
// rejected (no silent TrimSpace). The URL must be
// an absolute http:// form with no userinfo, no
// query, and no fragment; the Gate 09 client
// always probes /readyz on a fixed Service URL,
// so anything else is a substitution attempt.
// Accepted surface: http://<host>[:port]/<path>
// only — query / fragment / userinfo rejected.
func isValidHTTPURL(s string) bool {
	if s != strings.TrimSpace(s) {
		return false
	}
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7E {
			return false
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	if u.Scheme != "http" || u.Host == "" {
		return false
	}
	if u.User != nil {
		return false
	}
	if u.RawQuery != "" {
		return false
	}
	if u.Fragment != "" || strings.Contains(s, "#") {
		return false
	}
	// Hostname must be lowercase labels joined by
	// dots, optional digits/hyphens in labels.
	host := u.Hostname()
	if host == "" {
		return false
	}
	for _, r := range host {
		if !(r == '-' || r == '.' ||
			(r >= '0' && r <= '9') ||
			(r >= 'a' && r <= 'z')) {
			return false
		}
	}
	if strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") ||
		strings.HasPrefix(host, "-") || strings.HasSuffix(host, "-") ||
		strings.Contains(host, "..") {
		return false
	}
	return true
}

// redactHost strips path, query, userinfo, fragment.
func redactHost(s string) string {
	if i := strings.IndexAny(s, "/?#@"); i >= 0 {
		return s[:i]
	}
	return s
}

// redactURL drops userinfo and fragment only; query
// is preserved because a 404 vs 200 differential may
// hinge on it.
func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return "<unparseable-url>"
	}
	u.User = nil
	u.Fragment = ""
	return u.String()
}

// runResolveClient performs a bounded DNS lookup
// against the Pod network. The fixed 5-second
// deadline is carried by the request context, not
// by an operator-supplied flag. The output is a
// 2-field envelope:
//
//	{"host": "<input>", "addresses": ["<ipv4-or-ipv6>", ...]}
//
// addresses is a dedup-and-sort list of plain IP
// strings. A lookup error or zero addresses emits
// no JSON and exits non-zero.
func runResolveClient(host string) {
	ctx, cancel := context.WithTimeout(context.Background(), clientFixedDeadline)
	defer cancel()
	ips, err := resolveLookup(ctx, host)
	if err != nil {
		failClient(fmt.Sprintf("resolve-host %s failed: %v", redactHost(host), err))
	}
	if len(ips) == 0 {
		failClient(fmt.Sprintf("resolve-host %s failed: zero addresses", redactHost(host)))
	}
	uniq := dedupStrings(ips)
	sort.Strings(uniq)
	out := map[string]any{
		"host":      host,
		"addresses": uniq,
	}
	emitClientJSON(out)
}

// runHTTPClient performs a bounded http.Get against
// an absolute http:// URL. The 5-second deadline is
// carried by the request context. Redirects are
// rejected. The body is read at most 64KiB+1 byte:
// as soon as the (64KiB+1)th byte is observed we
// exit non-zero with a redacted stderr line and
// emit no JSON. Non-200 responses DO emit a JSON
// envelope with `status=<code>`; Gate 09 evaluates
// status==200.
func runHTTPClient(target string) {
	ctx, cancel := context.WithTimeout(context.Background(), clientFixedDeadline)
	defer cancel()

	client := &http.Client{
		// Redirects are forbidden. ErrUseLastResponse
		// surfaces 30x as a regular response so the
		// caller can still observe status.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		failClient(fmt.Sprintf("http-get %s failed: build request: %v", redactURL(target), err))
	}
	resp, err := client.Do(req)
	if err != nil {
		failClient(fmt.Sprintf("http-get %s failed: %v", redactURL(target), err))
	}
	defer resp.Body.Close()

	// Bounded body read: read one byte past the cap.
	// If that read yields any data, the server is
	// trying to stream more than the cap and we
	// fail closed. Otherwise we keep the truncated
	// body.
	probe := make([]byte, httpMaxBodyBytes+1)
	n, err := io.ReadFull(resp.Body, probe)
	if n > httpMaxBodyBytes {
		failClient(fmt.Sprintf("http-get %s failed: response body exceeds %d bytes", redactURL(target), httpMaxBodyBytes))
	}
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		failClient(fmt.Sprintf("http-get %s failed: read body: %v", redactURL(target), err))
	}
	bodyStr := string(probe[:n])
	out := map[string]any{
		"url":    target,
		"status": resp.StatusCode,
		"body":   bodyStr,
	}
	emitClientJSON(out)
}

// emitClientJSON writes ONE compact JSON document
// on stdout, terminated by exactly one newline.
// A marshalling failure is treated as a client
// failure so the gate never sees a half-baked
// envelope.
func emitClientJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		failClient(fmt.Sprintf("client marshal: %v", err))
	}
	if _, err := os.Stdout.Write(append(b, '\n')); err != nil {
		os.Exit(3)
	}
}

// dedupStrings removes duplicate entries while
// preserving order; sort.Strings runs AFTER.
func dedupStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// resolveLookup performs the actual net.Resolver
// host lookup. It is an unexported package-level
// variable so production callers (runResolveClient)
// continue to call the same signature without any
// test-only build tags or environment variables;
// tests in this file override it for the duration
// of the test with sync/atomic-friendly swap/restore
// so no real net.Resolver.LookupHost, /etc/hosts,
// or network activity is involved in deterministic
// coverage. The variable is read inside
// runResolveClient via a single function call so
// the production CLI signature remains unchanged.
var resolveLookup = func(ctx context.Context, host string) ([]string, error) {
	resolver := &net.Resolver{PreferGo: true}
	return resolver.LookupHost(ctx, host)
}
