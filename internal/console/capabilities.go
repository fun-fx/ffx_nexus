package console

import (
	"net/http"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/gateway"
)

// CapabilitySource is the gateway registry's control-plane surface.
type CapabilitySource interface {
	ProviderCapabilities() []gateway.ProviderCapability
}

func (s *Server) gatewayCapabilities(w http.ResponseWriter, r *http.Request, _ core.User) {
	caps := []gateway.ProviderCapability{}
	if s.capabilities != nil {
		if got := s.capabilities.ProviderCapabilities(); got != nil {
			caps = got
		}
	}
	cfgDiff := []string{}
	var snap *GatewayConfigSnapshot
	if s.gatewayConfigSrc != nil {
		got := s.gatewayConfigSrc.Snapshot()
		snap = &got
		cfgDiff = got.LastDiff
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": caps,
		"last_diff": cfgDiff,
		"config":    snap,
	})
}
