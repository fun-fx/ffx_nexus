package mcp

import "testing"

func TestClassifyToolRisk(t *testing.T) {
	cases := map[string]string{
		"read_file":     RiskRead,
		"list_dir":      RiskRead,
		"write_file":    RiskWrite,
		"delete_row":    RiskWrite,
		"http_request":  RiskNetwork,
		"fetch_url":     RiskNetwork,
		"send_email":    RiskDataExport,
		"export_csv":    RiskDataExport,
	}
	for name, want := range cases {
		if got := ClassifyToolRisk(name); got != want {
			t.Errorf("%s: got %s want %s", name, got, want)
		}
	}
}
