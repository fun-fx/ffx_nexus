// Package netpolicy holds the **code-level** mirror of
// docs/network-allowlist.md. The package exports one
// struct per workload role describing the minimum
// egress the workload is ever permitted to make.
//
// The struct list is consumed by:
//
//   - The Helm chart generator (Helm-side
//     `networkPolicy.allowlist.*` block rendering the
//     rules into NetworkPolicy YAML).
//   - The runtime egress guard in
//     internal/urlpolicy/ which intercepts outbound
//     dials and re-checks against this list.
//   - The CI inventory test that fails when the list
//     diverges from docs/network-allowlist.md.
//
// The struct list MUST be a strict subset of
// docs/network-allowlist.md. Drift between the two
// always fails the inventory check.
package netpolicy

// Destination is one row of the egress allowlist.
type Destination struct {
	// Feature is the dependency-contract feature
	// flag that gates this destination (e.g.
	// "tracePersist"). Empty string means always
	// required regardless of feature state.
	Feature string

	// Kind classifies the destination so the
	// runtime guard can take a fast-path per
	// protocol. Values: "tcp" | "udp" | "any".
	Kind string

	// Port is the IANA service port (5432, 53,
	// etc.). Zero when the port is operator-set
	// (e.g. ingress controller port differs per
	// cluster) and rendered from Helm values.
	Port int

	// Proto is "TCP" or "UDP"; ignored when
	// Kind == "any".
	Proto string

	// Allowed indicates whether this row renders
	// to a NetworkPolicy allow rule. Always true
	// for the inventory entries; used by tests
	// that mutate the row.
	Allowed bool
}

// RoleSpec is the egress allowlist for one
// workload. The empty slice means "no allow
// rules outside the default deny".
type RoleSpec struct {
	Name         string
	Component    string // matches app.kubernetes.io/component
	Destinations []Destination
}

// GatewayDestinations mirrors the rows in
// docs/network-allowlist.md where the workload
// column contains "gateway". Rows that depend
// on a feature flag are tagged with Feature.
func GatewayDestinations() []Destination {
	return []Destination{
		// Postgres (always on)
		{Feature: "", Kind: "tcp", Port: 5432, Proto: "TCP", Allowed: true},
		// Redis (rateLimitRedis)
		{Feature: "rateLimitRedis", Kind: "tcp", Port: 6379, Proto: "TCP", Allowed: true},
		// ClickHouse (tracePersist)
		{Feature: "tracePersist", Kind: "tcp", Port: 9000, Proto: "TCP", Allowed: true},
		{Feature: "tracePersist", Kind: "tcp", Port: 8123, Proto: "TCP", Allowed: true},
		// DNS
		{Feature: "", Kind: "udp", Port: 53, Proto: "UDP", Allowed: true},
		// Egress proxy (operator-chosen port)
		{Feature: "", Kind: "tcp", Port: 0, Proto: "TCP", Allowed: true},
	}
}

// WorkerDestinations mirrors the rows where the
// workload column contains "worker".
func WorkerDestinations() []Destination {
	return []Destination{
		{Feature: "", Kind: "tcp", Port: 5432, Proto: "TCP", Allowed: true},
		{Feature: "rateLimitRedis", Kind: "tcp", Port: 6379, Proto: "TCP", Allowed: true},
		{Feature: "tracePersist", Kind: "tcp", Port: 9000, Proto: "TCP", Allowed: true},
		{Feature: "", Kind: "udp", Port: 53, Proto: "UDP", Allowed: true},
		{Feature: "", Kind: "tcp", Port: 0, Proto: "TCP", Allowed: true},
	}
}

// MigrationDestinations mirrors the migration
// Job egress.
func MigrationDestinations() []Destination {
	return []Destination{
		{Feature: "", Kind: "tcp", Port: 5432, Proto: "TCP", Allowed: true},
	}
}

// MonitorDestinations mirrors ServiceMonitor/Prometheus
// egress.
func MonitorDestinations() []Destination {
	return []Destination{
		{Feature: "", Kind: "tcp", Port: 0, Proto: "TCP", Allowed: true}, // Prometheus scrape target
	}
}

// RoleSpecsByComponent returns the role spec list
// keyed by the `app.kubernetes.io/component` label.
func RoleSpecsByComponent() map[string]RoleSpec {
	return map[string]RoleSpec{
		"gateway": {
			Name:         "gateway",
			Component:    "gateway",
			Destinations: GatewayDestinations(),
		},
		"worker": {
			Name:         "worker",
			Component:    "worker",
			Destinations: WorkerDestinations(),
		},
		"migration": {
			Name:         "migration",
			Component:    "migration",
			Destinations: MigrationDestinations(),
		},
		"monitor": {
			Name:         "monitor",
			Component:    "monitor",
			Destinations: MonitorDestinations(),
		},
	}
}
