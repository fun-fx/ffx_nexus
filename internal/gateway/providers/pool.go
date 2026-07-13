// Package providers — pooled HTTP transport for high-concurrency calls.
//
// Go's net/http default transport keeps at most TWO idle connections per
// host (`DefaultMaxIdleConnsPerHost = 2`). On a heavily concurrent
// gateway that almost guarantees a fresh TCP+TLS setup on every new in
// flight request after the warmup. Provider APIs (OpenAI / Anthropic /
// Gemini / ...) themselves limit TCP and TLS handshakes, so we lose
// seconds on cold connections and hit rate limits much sooner than a
// tuned pool would.
//
// pool.go builds a tuned `*http.Transport` shared by every provider
// client. One pool per process is fine: each provider keeps its own
// client pointer and reuses it, but they all point into the same
// transport.

package providers

import (
	"net"
	"net/http"
	"runtime"
	"time"
)

// DefaultPoolSize returns the default MaxIdleConnsPerHost for the
// provider pool. We size it as `min(100, 2*GOMAXPROCS)` so it scales
// with the process goroutine budget without spinning up thousands of
// idle sockets on a small box. The cap is firm: above 100 idle conns
// per host the kernel buffers start hurting on retrains.
func DefaultPoolSize() int {
	n := 2 * runtime.GOMAXPROCS(0)
	if n < 32 {
		n = 32
	}
	if n > 100 {
		n = 100
	}
	return n
}

// NewPooledTransport returns an http.Transport tuned for high-concurrency
// outbound requests to LLM provider APIs. Callers MAY still hang an
// http.Client on it with their own CheckRedirect policy / per-call Timeout.
func NewPooledTransport(size int) *http.Transport {
	if size <= 0 {
		size = DefaultPoolSize()
	}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// Total idle conn cap across all hosts.
		MaxIdleConns: size * 4,
		// Per host cap — the main concurrency lever. Default is 2, which
		// melts on a hot gateway.
		MaxIdleConnsPerHost: size,
		// IdleConnTimeout governs how long an idle conn stays in the
		// pool before being reaped. 90s mirrors Go's stdlib default for
		// HTTP/1.1 keepalive but coexists well with provider idle
		// limits on the LLM side (they prune at 60–120s).
		IdleConnTimeout: 90 * time.Second,
		// TLS handshake cap — keep tight to avoid Tail-Too-Long stalls
		// when an upstream is sick.
		TLSHandshakeTimeout: 5 * time.Second,
		// ResponseHeaderTimeout — applies to LLM streaming as well; we
		// let it sit at 30s so a slow first packet doesn't drop the
		// request, but a totally hung connection eventually fails fast.
		ResponseHeaderTimeout: 30 * time.Second,
		// ExpectContinueTimeout unset: providers don't use 100-continue.
		ExpectContinueTimeout: 1 * time.Second,
		// ForceAttemptHTTP2: stay modern w/o OBSOLETE — TLS ALPN is the
		// canonical multiplexer. Most LLM APIs are HTTP/1.1 today but
		// OpenAI's edge will move to h2 opportunistically.
		ForceAttemptHTTP2: true,
	}
}

// PooledHTTPClient returns an http.Client using NewPooledTransport with
// the default size. The timeout is the per-call cap so a single retry
// can't strand forever; the transport keeps the connection pool warm
// across them.
func PooledHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: NewPooledTransport(DefaultPoolSize()),
	}
}

// PooledHTTPClientWithSize is for callers that want to override the
// pool size (e.g. a unit test that wants only a couple of conns so it
// can measure "first request still works after pool flush").
func PooledHTTPClientWithSize(timeout time.Duration, size int) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: NewPooledTransport(size),
	}
}
