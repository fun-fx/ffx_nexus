package gateway

import "testing"

func TestProviderCapabilitiesMatrix(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&stubProvider{name: "openai", models: []string{"gpt-4o"}})
	caps := reg.ProviderCapabilities()
	if len(caps) != 1 {
		t.Fatalf("len = %d", len(caps))
	}
	if !caps[0].Chat || !caps[0].Stream {
		t.Fatalf("chat/stream: %+v", caps[0])
	}
	if caps[0].Health != "registered" {
		t.Fatalf("health = %q", caps[0].Health)
	}
}
