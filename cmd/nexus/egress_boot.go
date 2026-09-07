package main

import (
	"log/slog"
	"strings"

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

	policy := egress.Policy{TenantAllowedCIDRs: allowed}

	switch cfg.EgressMode {
	case "":
		// direct mode. Nothing more to assemble.
	case "proxy":
		if cfg.EgressProxyURL == "" {
			log.Error("NEXUS_EGRESS_MODE=proxy but HTTPS_PROXY is empty; " +
				"provider-side requests will be sent unproxied. The chart " +
				"gate should have refused this combination at install time")
			break
		}
		policy.ProxyURL = cfg.EgressProxyURL
		log.Info("egress proxy mode is on",
			"proxy_url", cfg.EgressProxyURL,
			"detail", "URL is vetted before each request; static IP policy "+
				"still runs in full")
	case "in_cluster_only":
		if cfg.EgressInternalHosts == "" {
			log.Error("NEXUS_EGRESS_MODE=in_cluster_only but " +
				"NEXUS_EGRESS_INTERNAL_HOSTS is empty; tenant requests will " +
				"be refused. The chart gate should have refused this " +
				"combination at install time")
			break
		}
		policy.PublicDestinationsBlocked = true
		policy.AllowedInternalHosts = splitNonEmpty(cfg.EgressInternalHosts, ",")
		log.Warn("egress is in_cluster_only; public destinations are refused",
			"internal_hosts", policy.AllowedInternalHosts,
			"detail", "the chart's networkPolicy.providerEgress.inCluster."+
				"allowedServiceTargets values are mirrored here. Operators "+
				"should keep the two lists in lockstep")
	default:
		log.Error("NEXUS_EGRESS_MODE has an unrecognised value; "+
			"defaulting to direct outbound",
			"value", cfg.EgressMode)
	}

	egress.SetDefault(egress.New(policy))

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

// splitNonEmpty splits s on sep and trims whitespace, dropping
// entries that are empty after the trim.
func splitNonEmpty(s, sep string) []string {
	out := make([]string, 0, 4)
	for _, raw := range strings.Split(s, sep) {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}
