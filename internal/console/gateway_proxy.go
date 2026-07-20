package console

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// loopbackGatewayURL turns a listen address like ":8080" into an in-process
// reverse-proxy target. The console and gateway share a pod, so Playground and
// /v1/models discovery can stay same-origin on the public console hostname
// while Cursor uses the dedicated api.* hostname.
func loopbackGatewayURL(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		// Bare ":8080" — SplitHostPort needs a host component.
		if strings.HasPrefix(listenAddr, ":") {
			return "http://127.0.0.1" + listenAddr
		}
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// SetGatewayProxy wires /v1/* on the console mux to the co-located gateway
// listener. Empty or invalid addresses are ignored.
func (s *Server) SetGatewayProxy(listenAddr string) {
	targetURL := loopbackGatewayURL(listenAddr)
	if targetURL == "" {
		return
	}
	target, err := url.Parse(targetURL)
	if err != nil {
		s.log.Warn("gateway proxy disabled; invalid listen address", "addr", listenAddr, "err", err)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		s.log.Error("gateway proxy error", "path", r.URL.Path, "err", err)
		http.Error(w, "gateway unavailable", http.StatusBadGateway)
	}
	s.gatewayProxy = proxy
	s.log.Info("console /v1 proxy enabled", "target", targetURL)
}

// SetPublicGatewayURL is the user-facing gateway base URL shown in the console
// (curl snippets, onboarding copy). Example: https://api.ffx.ai
func (s *Server) SetPublicGatewayURL(raw string) {
	s.publicGatewayURL = strings.TrimRight(strings.TrimSpace(raw), "/")
}
