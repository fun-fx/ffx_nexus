package gateway

import "strings"

// price holds per-million-token USD pricing.
type price struct {
	inPerM  float64
	outPerM float64
}

// pricingTable is a best-effort static price list (USD per 1M tokens).
//
// Entries cover the model ids that ship in this repo's default catalog
// (`internal/gateway/providers/openai_compat.go` + `cmd/nexus/main.go`).
// The lookup logic in CostUSD() resolves versioned upstream ids
// (`gpt-4o-2024-08-06`) and provider-prefixed ids (`openai/gpt-4o`)
// back to a family key in this table, so a stable alias keeps the
// gate working after PR #111's dynamic model sync added bare-version
// ids to the registry.
//
// Providers introduced after this file's last refresh may produce
// `CostUSD = 0` until a price entry is added. New entries should
// preserve alphabetical order in their block.
var pricingTable = map[string]price{
	// OpenAI
	"gpt-4o":       {2.50, 10.00},
	"gpt-4o-mini":  {0.15, 0.60},
	"gpt-4.1":      {2.00, 8.00},
	"gpt-4.1-mini": {0.40, 1.60},
	"gpt-4.1-nano": {0.10, 0.40},
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

	// Groq (as of 2026-07 — confirm against groq.com/pricing on a quarterly cadence).
	"groq/llama-3.3-70b-versatile": {0.59, 0.79},
	"groq/llama-3.3-70b-specdec":   {0.59, 0.79},
	"groq/llama-3.1-8b-instant":    {0.05, 0.08},
	"groq/llama-3.1-70b-versatile": {0.59, 0.79},
	"groq/llama3-8b-8192":          {0.05, 0.08},
	"groq/llama3-70b-8192":         {0.59, 0.79},
	"groq/mixtral-8x7b-32768":      {0.27, 0.27},
	"groq/gemma2-9b-it":            {0.20, 0.20},
	"groq/llama-guard-3-8b":        {0.20, 0.20},

	// Mistral (as of 2026-07 — confirm against mistral.ai/platform/pricing).
	"mistral/mistral-large-latest":  {2.00, 6.00},
	"mistral/mistral-medium-latest": {0.40, 2.00},
	"mistral/mistral-small-latest":  {0.20, 0.60},
	"mistral/mistral-small-2409":    {0.20, 0.60},
	"mistral/codestral-latest":      {0.30, 0.90},
	"mistral/codestral-2405":        {0.30, 0.90},
	"mistral/open-mistral-7b":       {0.25, 0.25},
	"mistral/open-mixtral-8x7b":     {0.27, 0.27},
	"mistral/ministral-8b-latest":   {0.10, 0.10},
	"mistral/ministral-3b-latest":   {0.04, 0.04},
	"mistral/pixtral-12b-2409":      {0.15, 0.15},

	// The Grid is a spot market: the model field carries an "instrument"
	// rather than a fixed supplier. Pricing follows a published tier.
	// Reference: https://thegrid.ai/docs/pricing (last reviewed 2026-07).
	"grid/text-standard":  {0.20, 0.50},
	"grid/text-prime":     {0.50, 1.50},
	"grid/text-max":       {1.50, 4.50},
	"grid/code-standard":  {0.30, 0.90},
	"grid/code-prime":     {0.70, 2.10},
	"grid/code-max":       {2.10, 6.30},
	"grid/agent-standard": {0.40, 1.20},
	"grid/agent-prime":    {1.00, 3.00},
	"grid/agent-max":      {3.00, 9.00},
}

// familyAliases maps an upstream-returned versioned model id to one of the
// family keys in `pricingTable`. Lookup is a longest-prefix match against
// the keys; the first one whose key is a strict prefix of the input wins.
// Keep this map conservative — only family-resolution for ids we already
// expect — so a stray `gpt-5` future model fails closed (returns 0) instead
// of silently being charged at gpt-4o's rate.
var familyAliases = []struct {
	prefix string
	family string
}{
	{"gpt-4o-mini", "gpt-4o-mini"},
	{"gpt-4o", "gpt-4o"},
	// Order matters: `gpt-4.1` is a prefix of both `-mini` and `-nano`,
	// so the narrower ids must be tested first or a nano request gets
	// billed at the full gpt-4.1 rate (20x its real price).
	{"gpt-4.1-mini", "gpt-4.1-mini"},
	{"gpt-4.1-nano", "gpt-4.1-nano"},
	{"gpt-4.1", "gpt-4.1"},
	{"o4-mini", "o4-mini"},
	{"o3", "o3"},

	// Anthropic prices at the tier, not the point release, and has held
	// each tier's rate across 4.x. Matching on the bare tier name keeps
	// a newer Opus/Sonnet/Haiku costed at its family rate instead of
	// silently landing at zero — which matters because The Grid's
	// `claude-opus-latest` market reports no cost of its own and is
	// priced from the supplier id it returns (`anthropic/claude-opus-5`).
	// The 3.x entries stay ahead of the generic ones so the older,
	// differently-priced generation is not swallowed by them.
	{"claude-3-7-sonnet", "claude-3-7-sonnet-latest"},
	{"claude-3-5-haiku", "claude-3-5-haiku-latest"},
	{"claude-opus", "claude-opus-4-1"},
	{"claude-sonnet", "claude-sonnet-4-5"},
	{"claude-haiku", "claude-haiku-4-5"},

	{"gemini-2.5-pro", "gemini-2.5-pro"},
	{"gemini-2.5-flash", "gemini-2.5-flash"},
	{"gemini-2.0-flash", "gemini-2.0-flash"},

	// The Grid spot-market instruments — bare ids returned by The Grid's
	// /v1/models sync come back as `code-prime`, `text-max` etc. without
	// the `grid/` prefix. Resolve them through the family aliases so
	// cost lookup still hits the pricingTable.
	{"text-standard", "grid/text-standard"},
	{"text-prime", "grid/text-prime"},
	{"text-max", "grid/text-max"},
	{"code-standard", "grid/code-standard"},
	{"code-prime", "grid/code-prime"},
	{"code-max", "grid/code-max"},
	{"agent-standard", "grid/agent-standard"},
	{"agent-prime", "grid/agent-prime"},
	{"agent-max", "grid/agent-max"},
}

// CostUSD computes the request cost from token usage. Returns 0 for unknown
// models.
//
// Resolution order (first non-empty match wins):
//  1. Exact lookup against `pricingTable` — handles keys that already
//     exist (e.g. "openai/gpt-4o", "groq/llama-3.3-70b-versatile").
//  2. Strip a leading `<provider>/` prefix and retry (handles bare ids
//     like `gpt-4o` after the upstream /v1/models sync returns bare names).
//  3. Longest-prefix match against `familyAliases` — handles versioned ids
//     like `gpt-4o-2024-08-06` that share a family root with a priced key.
//
// `requestModel` is the customer-facing model id the caller asked for.
// `responseModel` is the upstream-resolved id (filled from upstream
// `model` response field); some providers rewrite the model the user
// sees to a versioned id even though they price at the family level.
// Passing both lets the lookup catch that case.
//
// Callers that previously passed only the request model keep working:
// the function still resolves via path 1 or 2 for the request id and,
// when the response id happens to match a known family, finds it through
// path 3 using the response id.
// ResolveCostUSD returns the authoritative spend for one call.
//
// `upstreamReported` is what the provider itself said the call cost
// (`usage.estimated_cost`). When it is positive we return it verbatim:
// the vendor knows its own price and we do not. This is load-bearing for
// The Grid, whose instruments (`code-prime`, `claude-opus-latest`, ...)
// are spot-market contracts filled from whichever token lot was cheapest
// at that moment — the static table below can only ever approximate it,
// and instruments added after a release would silently cost 0.
//
// Everything else falls back to the local pricing table, which is what
// OpenAI / Anthropic / Gemini / Groq / Mistral need because none of them
// report cost on the wire (they return token counts only).
func ResolveCostUSD(upstreamReported float64, requestModel, responseModel string, inTokens, outTokens int) float64 {
	if upstreamReported > 0 {
		return upstreamReported
	}
	return CostUSD(requestModel, responseModel, inTokens, outTokens)
}

func CostUSD(requestModel, responseModel string, inTokens, outTokens int) float64 {
	// Try the request id first (most common case for pricing accuracy).
	if p, ok := matchPrice(requestModel); ok {
		return apply(p, inTokens, outTokens)
	}
	// Fall back to the upstream-resolved id when it differs.
	if responseModel != "" && responseModel != requestModel {
		if p, ok := matchPrice(responseModel); ok {
			return apply(p, inTokens, outTokens)
		}
	}
	return 0
}

// matchPrice runs the resolution chain described on CostUSD() against a
// single model id. Exposed for the table-driven test.
func matchPrice(model string) (price, bool) {
	if model == "" {
		return price{}, false
	}
	if p, ok := pricingTable[model]; ok {
		return p, true
	}
	// Strip a single "<provider>/" prefix and retry (handles cases where
	// the registry carried `openai/gpt-4o` but the upstream deserialised
	// the model field to bare `gpt-4o`).
	if _, rest, found := strings.Cut(model, "/"); found {
		if p, ok := pricingTable[rest]; ok {
			return p, true
		}
		// Longest-prefix alias match against the stripped id (handles
		// `groq/gpt-oss-120b` after the prefix fallback already ran).
		if p, ok := matchAlias(rest); ok {
			return p, true
		}
	}
	// Longest-prefix alias match against the raw id (handles bare
	// versioned ids that the upstream returned unprefixed).
	if p, ok := matchAlias(model); ok {
		return p, true
	}
	return price{}, false
}

// matchAlias walks `familyAliases` in declaration order and returns the
// price family entry whose key is a strict prefix of the input id.
// The map is ordered so more specific aliases come first to avoid a
// shorter prefix stealing the match.
func matchAlias(model string) (price, bool) {
	for _, a := range familyAliases {
		if a.prefix == model {
			// Exact alias is treated as a hit too — saves a redundant
			// slice walk for ids like `gpt-4o` that are already in the
			// family table.
			if p, ok := pricingTable[a.family]; ok {
				return p, true
			}
			continue
		}
		if strings.HasPrefix(model, a.prefix) {
			if p, ok := pricingTable[a.family]; ok {
				return p, true
			}
		}
	}
	return price{}, false
}

func apply(p price, inTokens, outTokens int) float64 {
	return (float64(inTokens)/1e6)*p.inPerM + (float64(outTokens)/1e6)*p.outPerM
}
