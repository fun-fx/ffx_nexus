package main

import (
	"log/slog"

	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/egress"
)

// installEgressGuard configures the process-wide outbound HTTP policy.
//
// A malformed CIDR list is logged and ignored rather than fatal. The reasoning:
// the failure mode of ignoring it is "an in-cluster vendor stays unreachable
// from a tenant-configured plugin", which is visible, recoverable and no worse
// than the default. The failure mode of exiting is a pod that will not boot, and
// the strict default is already the safe end of the policy — refusing to start
// in order to enforce a *widening* of that policy is the wrong trade.
func installEgressGuard(cfg config.Config, log *slog.Logger) {
	allowed, err := egress.ParseTenantAllowedCIDRs(cfg.EgressTenantAllowedCIDRs)
	if err != nil {
		log.Error("NEXUS_EGRESS_TENANT_ALLOWED_CIDRS is malformed; "+
			"tenant-configured destinations keep the default policy and cannot reach "+
			"private addresses",
			"err", err)
		allowed = nil
	}
	egress.SetDefault(egress.New(egress.Policy{TenantAllowedCIDRs: allowed}))

	if len(allowed) > 0 {
		// Worth a warning line, not just info: this is an operator widening the
		// blast radius of a tenant-supplied URL, and it should be findable in the
		// logs when somebody later asks how a plugin reached an internal host.
		ranges := make([]string, 0, len(allowed))
		for _, p := range allowed {
			ranges = append(ranges, p.String())
		}
		log.Warn("tenant-configured egress destinations may reach these private ranges",
			"cidrs", ranges,
			"detail", "an org admin can point an eval profile or plugin at any address "+
				"in these ranges; cloud instance metadata stays blocked regardless")
	}
}
