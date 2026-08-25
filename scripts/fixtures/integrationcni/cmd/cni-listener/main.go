// scripts/fixtures/integrationcni/cmd/cni-listener/main.go
//
// Phase D-2b.25: deterministic HTTP / TCP
// listener for the D-2b CNI integration tests.
//
// cni-listener runs as a long-running server
// that opens N TCP listeners (-ports flag,
// comma-separated). For each port it serves:
//
//   GET /              -> 200 application/json
//   GET /healthz       -> 200 application/json
//   GET /readyz        -> 200 application/json
//                         {"ready":true,"port":N,...}
//
// The /readyz endpoint exists because the
// fixture's readinessProbe may route through
// the kubelet's exec probe, but the gate also
// uses an HTTP probe from inside the cluster,
// so both surfaces are served. Returning a
// real JSON body — instead of a curl -I "200
// OK" line — gives the gate a stable byte
// sequence to assert against, which is what
// makes the probe deterministic across runs.
//
// -probe <port> makes the binary exit 0 once
// the listener at <port> accepts a SYN. This
// is the form the readinessProbe exec command
// uses.
//
// Usage:
//
//   /cni-listener -ports=8080,9100,9111,3128,5432,6379,9000,9090
//   /cni-listener -probe=8080
//
// The -ports flag is a comma-separated list
// because one fixture pod may need to listen
// on more than one port (gateway: 8080+9101,
// worker: 8081+9101). The control fixture
// passes a single port.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {
	ports := flag.String("ports", "", "comma-separated list of TCP ports to listen on (e.g. 8080,9100)")
	probe := flag.String("probe", "", "exit 0 once the listener at this port accepts a SYN (readinessProbe form)")
	role := flag.String("role", "fixture", "echoed in the JSON body; lets scenario probes distinguish fixture pods by role")
	target := flag.String("target", "", "echoed in /readyz so the gate can record which fixture target answered")

	flag.Parse()

	if *probe != "" {
		probePort(*probe)
		return
	}
	if *ports == "" {
		log.Fatalf("no -ports flag passed")
	}

	podName := os.Getenv("POD_NAME")
	if podName == "" {
		podName = "cni-listener"
	}
	mounts, err := parsePorts(*ports)
	if err != nil {
		log.Fatalf("invalid -ports %q: %v", *ports, err)
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
	deadline := 60 // seconds
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
		log.Printf("[%s] listen %d: %v", pod, port, err)
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
	// /metrics: serve a minimal Prometheus exposition
	// so contract-test side checks like
	// "out contains # HELP" pass regardless of role.
	// We DO NOT serve real metrics - the contract
	// only asserts the body has a # HELP line, not
	// real numbers.
	if strings.HasPrefix(raw, "GET /metrics ") {
		promBody := metricsBody(role, target)
		reply(conn, 200, "text/plain; version=0.0.4", promBody)
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
		// bare TCP probe (some control paths use netcat)
		// In that case we just close the connection: the
		// kubelet probes for readinessProbe exec, not via
		// this socket.
		return
	}
	// Default: HTTP/1.1 200 with a JSON body. Any path
	// the gate sends is honored the same way.
	reply(conn, 200, "application/json", body)
}

func metricsBody(role, target string) string {
	// Deterministic 4-line Prometheus exposition.
	// The contract tests check that "# HELP" is
	// present in the body; we don't try to mimic
	// a real metric value because the chart's
	// fixtures don't have an opinion on values.
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
