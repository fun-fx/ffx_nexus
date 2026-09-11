package mcp

import "strings"

const (
	RiskRead       = "read"
	RiskWrite      = "write"
	RiskNetwork    = "network"
	RiskDataExport = "data_export"
)

// ClassifyToolRisk assigns a coarse risk grade from the tool name.
// This is display + allowlist helper only — it does not auto-execute tools.
func ClassifyToolRisk(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.ReplaceAll(n, "-", "_")
	switch {
	case containsAny(n, "email", "send_mail", "export", "download", "upload", "share", "forward"):
		return RiskDataExport
	case containsAny(n, "http", "fetch", "request", "webhook", "browse", "url", "web_"):
		return RiskNetwork
	case containsAny(n, "write", "create", "update", "delete", "put", "patch", "remove", "insert", "drop"):
		return RiskWrite
	default:
		return RiskRead
	}
}

func containsAny(s string, parts ...string) bool {
	for _, p := range parts {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
