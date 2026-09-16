package console

import (
	"net/http"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/gateway"
)

// PricingDriftSource supplies the last published-rate catalog compare.
// Nil means the worker is off and the endpoint reports enabled=false.
type PricingDriftSource interface {
	Snapshot() gateway.PricingCheckSnapshot
}

func (s *Server) SetPricingDrift(src PricingDriftSource) { s.pricingDriftSrc = src }

func (s *Server) getPricingDrift(w http.ResponseWriter, r *http.Request, _ core.User) {
	if s.pricingDriftSrc == nil {
		writeJSON(w, http.StatusOK, gateway.PricingCheckSnapshot{
			Enabled:          false,
			Drifts:           []gateway.PricingDrift{},
			BillingUnchanged: true,
		})
		return
	}
	writeJSON(w, http.StatusOK, s.pricingDriftSrc.Snapshot())
}
