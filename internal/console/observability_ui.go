package console

import (
	"net/http"
	"strings"

	"github.com/ffxnexus/nexus/internal/core"
)

// Observability deep links for the console UI.
//
// The console has always shipped the client half of this feature: web/src/api.ts
// declares fetchUIObservability() against GET /api/ui/observability and the
// Sidebar renders an "Open in Grafana" entry from the result. The server half
// was never implemented, so the fetch 404'd on every page load and the link
// silently never appeared. This file closes that gap rather than deleting the
// client code, because "point the console at the Grafana we already run" is a
// standing request from self-hosting operators.
//
// Scope is deliberately tiny: Nexus stores no Grafana state, holds no Grafana
// credential, and makes no server-side call to Grafana. The endpoint is a pure
// function of NEXUS_PUBLIC_GRAFANA_URL. That is what keeps the guarantee that a
// Grafana outage cannot degrade the gateway — there is no code path from a
// model request to Grafana at all.

// Dashboard UIDs must match the `uid` of the bundled dashboards in
// deploy/helm/nexus/files/grafana-dashboards/. Grafana resolves /d/<uid>/<slug>
// by uid alone and treats the slug as cosmetic, so a renamed dashboard title
// does not break these links.
const (
	grafanaUIDOverview = "nexus-01-overview"
	grafanaUIDSpend    = "nexus-02-llm-spend"
	grafanaUIDEval     = "nexus-03-eval-quality"
)

type uiObservabilityGrafana struct {
	Base     string `json:"base"`
	Overview string `json:"overview"`
	Spend    string `json:"spend"`
	Eval     string `json:"eval"`
}

type uiObservabilityOTLP struct {
	Enabled  bool   `json:"enabled"`
	Endpoint string `json:"endpoint,omitempty"`
}

type uiObservabilityPrometheus struct {
	Enabled bool   `json:"enabled"`
	Listen  string `json:"listen,omitempty"`
	Path    string `json:"path"`
}

type uiObservabilityFlag struct {
	Configured bool `json:"configured"`
}

type uiObservabilityTraces struct {
	Clickhouse bool `json:"clickhouse"`
}

type uiObservabilityMCP struct {
	Enabled    bool `json:"enabled"`
	Clickhouse bool `json:"clickhouse"`
}

type uiObservability struct {
	Grafana    *uiObservabilityGrafana   `json:"grafana,omitempty"`
	OTLP       uiObservabilityOTLP       `json:"otlp"`
	Prometheus uiObservabilityPrometheus `json:"prometheus"`
	Metabase   uiObservabilityFlag       `json:"metabase"`
	Traces     uiObservabilityTraces     `json:"traces"`
	MCP        uiObservabilityMCP        `json:"mcp"`
	LocalMode  bool                      `json:"local_mode"`
}

// SetObservabilitySinks records boot-time gateway sinks so the console
// Observability page can show whether OTLP, Prometheus, and Metabase are
// actually wired — without implying the UI can flip them. Those knobs still
// live in env / Helm; a Save that did not survive a restart would be a lie.
func (s *Server) SetObservabilitySinks(otlpEnabled bool, otlpEndpoint, metricsAddr, metabaseURL string) {
	s.otlpEnabled = otlpEnabled
	s.otlpEndpoint = strings.TrimSpace(otlpEndpoint)
	s.metricsAddr = strings.TrimSpace(metricsAddr)
	s.metabaseConfigured = strings.TrimSpace(metabaseURL) != ""
}

// SetPublicGrafanaURL records the operator's Grafana base URL. Trailing
// slashes are trimmed so link composition never produces a double slash.
// Anything that is not an absolute http(s) URL is rejected and logged rather
// than stored: a relative or javascript: value here would be reflected into an
// href in the operator's browser.
func (s *Server) SetPublicGrafanaURL(raw string) {
	v := strings.TrimRight(strings.TrimSpace(raw), "/")
	if v == "" {
		return
	}
	if !strings.HasPrefix(v, "https://") && !strings.HasPrefix(v, "http://") {
		s.log.Warn("ignoring NEXUS_PUBLIC_GRAFANA_URL: not an absolute http(s) URL",
			"value", v)
		return
	}
	s.publicGrafanaURL = v
}

// observabilityUI answers GET /api/ui/observability.
//
// Authenticated on purpose. The value is not a credential, but it does name an
// internal hostname, and an unauthenticated endpoint that echoes back a piece
// of the operator's infrastructure topology is free reconnaissance. The client
// already degrades to "no link" on any non-200, and the only caller renders
// inside an authenticated shell, so requiring a session costs nothing.
func (s *Server) observabilityUI(w http.ResponseWriter, _ *http.Request, _ core.User) {
	out := uiObservability{
		OTLP: uiObservabilityOTLP{
			Enabled:  s.otlpEnabled && s.otlpEndpoint != "",
			Endpoint: s.otlpEndpoint,
		},
		Prometheus: uiObservabilityPrometheus{
			Enabled: s.metricsAddr != "",
			Listen:  s.metricsAddr,
			Path:    "/metrics",
		},
		Metabase: uiObservabilityFlag{Configured: s.metabaseConfigured},
		Traces:   uiObservabilityTraces{Clickhouse: s.reader != nil},
		MCP: uiObservabilityMCP{
			Enabled:    s.mcpStore != nil,
			Clickhouse: s.reader != nil,
		},
		LocalMode: s.localMode,
	}
	if base := s.publicGrafanaURL; base != "" {
		out.Grafana = &uiObservabilityGrafana{
			Base:     base,
			Overview: base + "/d/" + grafanaUIDOverview + "/" + grafanaUIDOverview,
			Spend:    base + "/d/" + grafanaUIDSpend + "/" + grafanaUIDSpend,
			Eval:     base + "/d/" + grafanaUIDEval + "/" + grafanaUIDEval,
		}
	}
	writeJSON(w, http.StatusOK, out)
}
