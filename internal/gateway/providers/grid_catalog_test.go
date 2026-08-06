package providers

import "testing"

func gridCatalog() map[string]bool {
	have := make(map[string]bool, len(GridChatModels))
	for _, m := range GridChatModels {
		have[m] = true
	}
	return have
}

// The Grid's lab-latest markets are model-family routes (`claude-opus-latest`
// and friends) rather than task tiers. They are as callable as the nine tier
// instruments, but a model id missing from this catalog cannot be resolved by
// the registry: the request fails 404 model_not_found before a trace row is
// written, so the call is invisible in the console instead of merely
// appearing with a zero cost.
//
// Reference: https://thegrid.ai/docs/instrument-specifications/current-instruments
func TestGridChatModels_IncludesLabLatestMarkets(t *testing.T) {
	have := gridCatalog()
	for _, want := range []string{
		"gpt-sol-latest",
		"claude-opus-latest",
		"gemini-pro-latest",
		"minimax-latest",
		"glm-latest",
		"deepseek-pro-latest",
		"kimi-latest",
		"bytedance-pro-latest",
	} {
		if !have[want] {
			t.Errorf("GridChatModels missing lab-latest instrument %q", want)
		}
	}
}

func TestGridChatModels_StillHasTierInstruments(t *testing.T) {
	have := gridCatalog()
	for _, want := range []string{
		"text-standard", "text-prime", "text-max",
		"code-standard", "code-prime", "code-max",
		"agent-standard", "agent-prime", "agent-max",
	} {
		if !have[want] {
			t.Errorf("GridChatModels lost tier instrument %q", want)
		}
	}
}

// NewGrid must actually advertise the catalog, not a subset — the registry
// resolves against the provider's Models(), not the package var.
func TestNewGrid_AdvertisesFullCatalog(t *testing.T) {
	p := NewGrid("test-key", 0)
	advertised := make(map[string]bool)
	for _, m := range p.Models() {
		advertised[m] = true
	}
	for _, want := range []string{"code-prime", "claude-opus-latest", "gpt-sol-latest"} {
		if !advertised[want] {
			t.Errorf("NewGrid does not advertise %q; registry would 404 it", want)
		}
	}
}
