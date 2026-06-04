package gateway

import "strings"

// price holds per-million-token USD pricing.
type price struct {
	inPerM  float64
	outPerM float64
}

// pricingTable is a best-effort static price list (USD per 1M tokens). Unknown
// models fall back to zero cost. Override via config later if needed.
var pricingTable = map[string]price{
	// OpenAI
	"gpt-4o":       {2.50, 10.00},
	"gpt-4o-mini":  {0.15, 0.60},
	"gpt-4.1":      {2.00, 8.00},
	"gpt-4.1-mini": {0.40, 1.60},
	"o3":           {2.00, 8.00},
	"o4-mini":      {1.10, 4.40},
	// Anthropic
	"claude-opus-4-1":          {15.00, 75.00},
	"claude-sonnet-4-5":        {3.00, 15.00},
	"claude-haiku-4-5":         {1.00, 5.00},
	"claude-3-7-sonnet-latest": {3.00, 15.00},
	"claude-3-5-haiku-latest":  {0.80, 4.00},
	// Gemini
	"gemini-2.5-pro":   {1.25, 10.00},
	"gemini-2.5-flash": {0.30, 2.50},
	"gemini-2.0-flash": {0.10, 0.40},
}

// CostUSD computes the request cost from token usage. Returns 0 for unknown models.
func CostUSD(model string, inTokens, outTokens int) float64 {
	p, ok := pricingTable[model]
	if !ok {
		// Try stripping a provider prefix like "openai/gpt-4o".
		if _, rest, found := strings.Cut(model, "/"); found {
			p, ok = pricingTable[rest]
		}
	}
	if !ok {
		return 0
	}
	return (float64(inTokens)/1e6)*p.inPerM + (float64(outTokens)/1e6)*p.outPerM
}
