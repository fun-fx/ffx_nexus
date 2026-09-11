package console

import (
	"encoding/json"
	"html"
	"net/http"
	"time"

	"github.com/ffxnexus/nexus/internal/core"
)

// InstallReadiness is the customer-facing packaging of live D-2b / eval /
// capture contracts. It does not re-run CNI tests; it reports what this
// process was booted with so an operator can attach evidence to a ticket.
type InstallReadiness struct {
	GeneratedAt               string   `json:"generated_at"`
	BuildTag                  string   `json:"build_tag"`
	EgressMode                string   `json:"egress_mode"`
	CaptureTraceContent       bool     `json:"capture_trace_content"`
	EvalPluginOnly            bool     `json:"eval_plugin_only"`
	PurgeLegacyProfilesOnBoot bool     `json:"purge_legacy_profiles_on_boot"`
	FailClosed                bool     `json:"fail_closed"`
	Notes                     []string `json:"notes"`
	D2b                       struct {
		Packaged           bool   `json:"packaged"`
		ProviderEgressMode string `json:"provider_egress_mode"`
		ChartNetworkPolicy string `json:"chart_network_policy"`
		Note               string `json:"note"`
	} `json:"d2b"`
}

// InstallReadinessSource supplies the live report.
type InstallReadinessSource interface {
	Report() InstallReadiness
}

// ReadinessFunc adapts a function to InstallReadinessSource.
type ReadinessFunc func() InstallReadiness

func (f ReadinessFunc) Report() InstallReadiness { return f() }

func (s *Server) installReadiness(w http.ResponseWriter, r *http.Request, _ core.User) {
	if s.installReady == nil {
		http.Error(w, "readiness report unavailable", http.StatusServiceUnavailable)
		return
	}
	rep := s.installReady.Report()
	if r.URL.Query().Get("format") == "html" {
		body, _ := json.MarshalIndent(rep, "", "  ")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="nexus-readiness.html"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Nexus installation readiness</title>`))
		_, _ = w.Write([]byte(`<h1>Nexus installation readiness</h1><p>Build <code>`))
		_, _ = w.Write([]byte(html.EscapeString(rep.BuildTag)))
		_, _ = w.Write([]byte(`</code> · generated `))
		_, _ = w.Write([]byte(html.EscapeString(rep.GeneratedAt)))
		_, _ = w.Write([]byte(`</p><pre>`))
		_, _ = w.Write([]byte(html.EscapeString(string(body))))
		_, _ = w.Write([]byte(`</pre>`))
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func NewInstallReadiness(buildTag, egressMode string, capture, pluginOnly, purgeLegacy bool) InstallReadiness {
	rep := InstallReadiness{
		GeneratedAt:               time.Now().UTC().Format(time.RFC3339),
		BuildTag:                  buildTag,
		EgressMode:                egressMode,
		CaptureTraceContent:       capture,
		EvalPluginOnly:            pluginOnly,
		PurgeLegacyProfilesOnBoot: purgeLegacy,
		FailClosed:                egressMode == "in_cluster_only" || egressMode == "proxy",
		Notes: []string{
			"This report packages the live process contract; it does not re-run D-2b CNI enforcement tests.",
			"Prompt bodies reach durable storage only when capture_trace_content is true.",
		},
	}
	rep.D2b.Packaged = true
	rep.D2b.ProviderEgressMode = egressMode
	rep.D2b.ChartNetworkPolicy = "deploy/helm/nexus: networkPolicy.providerEgress (fail-closed render)"
	rep.D2b.Note = "Helm in_cluster_only / proxy modes are gated at chart render time. Runtime must match NEXUS_EGRESS_MODE."
	if !rep.FailClosed {
		rep.Notes = append(rep.Notes, "egress_mode is unset: outbound provider dials are not chart-constrained.")
	}
	return rep
}
