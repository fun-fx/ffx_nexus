package gateway

import "strings"

// price holds per-million-token USD pricing for the simple
// input/output split that production providers expose on most models.
//
// Most call math uses exactly this struct via applyBasic. Models with
// cache discounts or reasoning premiums graduate to detailedPrice so
// the cost composer can bill cached input separately from regular
// input and reasoning output from text output. The catalogue carries
// the basic rate for both paths; the composer is responsible for
// looking up the detailed version when a model needs it.
type price struct {
	inPerM  float64
	outPerM float64
}

// detailedPrice extends price with the per-component rates that some
// model catalogues use. All fields are USD per 1M tokens for the named
// component; zero values fall back to the basic in/out rate so authors
// of new entries only set the fields that actually differ.
//
//   - cachedInPerM  — prompt tokens that hit a vendor cache. Set to a
//                     small fraction of inPerM when the vendor offers
//                     a deep discount (e.g. claude prompt cache ~10% of
//                     base input). 0 means "bill at inPerM".
//   - reasoningOutPerM — completion tokens counted as internal reasoning
//                     by the model. Typically 2x–5x outPerM for o-series
//                     and claude extended thinking. 0 means "bill at
//                     outPerM".
//
// Detailed prices are intentionally only declared when the vendor
// exposes the matching disaggregated token detail in usage; otherwise
// the composer has no signal to apply them.
type detailedPrice struct {
	price
	cachedInPerM     float64
	reasoningOutPerM float64
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

// detailedPricingTable holds the small subset of models for which a
// component-level breakdown is published and the upstream exposes the
// matching usage.*_tokens_details block. Only entries listed in this
// map reach the disaggregation path; everyone else stays on the basic
// rate even when details are present, so an unexpected field from a
// new vendor cannot multiply the cost silently.
//
// Numbers reflect public pricing pages reviewed in 2026-07:
//   - Anthropic prompt cache: ~10% of base input for 5m, ~20% for 1h
//     (https://docs.claude.com/en/docs/build-with-claude/prompt-caching).
//   - o3 / o4 reasoning tokens: charged at the model's output rate
//     already, so reasoningOutPerM == outPerM. Listed for completeness.
//   - The Grid does not publish reasoning caches; left at base rate.
var detailedPricingTable = map[string]detailedPrice{
	// Anthropic prompt cache: cache hits at ~10% of input from the 5m
	// window. The 1h window is ~20% but The Grid surfaced caches we
	// observed are 5m, and bucket-level fidelity isn't worth a third
	// field today.
	"claude-opus-4-1":          {price: price{15.00, 75.00}, cachedInPerM: 1.50},
	"claude-sonnet-4-5":        {price: price{3.00, 15.00}, cachedInPerM: 0.30},
	"claude-haiku-4-5":         {price: price{1.00, 5.00}, cachedInPerM: 0.10},
	"claude-3-7-sonnet-latest": {price: price{3.00, 15.00}, cachedInPerM: 0.30},
	"claude-3-5-haiku-latest":  {price: price{0.80, 4.00}, cachedInPerM: 0.08},

	// OpenAI o-series: reasoning tokens are charged the same as
	// completion output. The entries are not strictly necessary today
	// (no extra multiplier) but pinning them causes the composer to
	// prefer the disaggregated reasoning_tokens count when it is
	// present and so produces a number visibly closer to the vendor's
	// own accounting for unsettled streaming cases.
	"o3":      {price: price{2.00, 8.00}, reasoningOutPerM: 8.00},
	"o4-mini": {price: price{1.10, 4.40}, reasoningOutPerM: 4.40},
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
// Pass usage = nil for calls that need a basic price-table estimate from
// just prompt + completion counts. Pass the actual upstream `Usage`
// when the vendor included a `prompt_tokens_details` or
// `completion_tokens_details` block; the composer will pick the
// detailed rate if the model is in `detailedPricingTable` AND the
// matching detail carries non-zero component counts.
//
// When upstream is silent AND the model has a published detailed rate,
// we still prefer detail-aware math — a Claude call with 100 cached
// prompt tokens is genuinely cheaper than 100 fresh ones, and treating
// them as identical would be a 10x overstatement of billable spend.
//
// Everything else falls back to the local pricing table, which is what
// OpenAI / Anthropic / Gemini / Groq / Mistral need because none of them
// report cost on the wire (they return token counts only).
func ResolveCostUSD(upstreamReported float64, requestModel, responseModel string, usage *Usage) float64 {
	if upstreamReported > 0 {
		return upstreamReported
	}
	if usage == nil {
		return CostUSD(requestModel, responseModel, 0, 0)
	}
	detail := false
	if usage.PromptTokenDetails != nil && usage.PromptTokenDetails.HasDetail() {
		detail = true
	}
	if usage.CompletionTokenDetails != nil && usage.CompletionTokenDetails.HasDetail() {
		detail = true
	}
	if detail && lookupDetailedFamily(requestModel, responseModel) != "" {
		return costForModel(requestModel, responseModel, usage.PromptTokens, usage.CompletionTokens, true, usage)
	}
	return CostUSD(requestModel, responseModel, usage.PromptTokens, usage.CompletionTokens)
}

// CostUSD is a thin wrapper that ignores usage disaggregation and
// returns the basic rate-table result. Retained so the streaming code
// path that only has PromptTokens/CompletionTokens totals (the chunk
// that arrives last *should* have all the details but the basic path
// is the documented minimum) still works.
func CostUSD(requestModel, responseModel string, inTokens, outTokens int) float64 {
	return costForModel(requestModel, responseModel, inTokens, outTokens, false, nil)
}

// CostUSAGECost is the disaggregation-aware version of CostUSD. Use
// this whenever the caller holds a *Usage that may carry
// prompt_tokens_details / completion_tokens_details.
func CostUSAGECost(requestModel, responseModel string, usage *Usage) float64 {
	if usage == nil {
		return CostUSD(requestModel, responseModel, 0, 0)
	}
	detail := false
	if usage.PromptTokenDetails != nil && usage.PromptTokenDetails.HasDetail() {
		detail = true
	}
	if usage.CompletionTokenDetails != nil && usage.CompletionTokenDetails.HasDetail() {
		detail = true
	}
	if detail && lookupDetailedFamily(requestModel, responseModel) != "" {
		return costForModel(requestModel, responseModel, usage.PromptTokens, usage.CompletionTokens, true, usage)
	}
	return costForModel(requestModel, responseModel, usage.PromptTokens, usage.CompletionTokens, false, nil)
}

// costForModel runs the resolution chain (model id -> family key -> price)
// and then applies either the basic or the detail-aware formula.
//
// In and out tokens at zero keep math idempotent, so the invoice-only
// case (where the trace knows a model but no usage yet) returns 0 instead
// of NaN. The cost composer is expected to be the only place that
// inspects `useDetail`; everywhere else the choice is made by CostUSD.
func costForModel(requestModel, responseModel string, inTokens, outTokens int, useDetail bool, usage *Usage) float64 {
	if useDetail {
		if dp, ok := matchDetailed(requestModel); ok {
			return applyDetailed(dp, inTokens, outTokens, usage)
		}
		if responseModel != "" && responseModel != requestModel {
			if dp, ok := matchDetailed(responseModel); ok {
				return applyDetailed(dp, inTokens, outTokens, usage)
			}
		}
	}
	if p, ok := matchPrice(requestModel); ok {
		return applyBasic(p, inTokens, outTokens)
	}
	if responseModel != "" && responseModel != requestModel {
		if p, ok := matchPrice(responseModel); ok {
			return applyBasic(p, inTokens, outTokens)
		}
	}
	return 0
}

// lookupDetailedFamily returns the pricing-table family key for the
// inputs — same resolution order as matchPrice — or "" if neither
// model has a detailed entry. Used to decide whether to bypass the
// detail path (we should not let a detail-aware *Usage on a basic-rate
// model trigger detail math, because the fields are not documented
// for that model and would be applying hypothetical rates).
func lookupDetailedFamily(requestModel, responseModel string) string {
	if _, ok := matchDetailed(requestModel); ok {
		return requestModel
	}
	if responseModel != "" && responseModel != requestModel {
		if _, ok := matchDetailed(responseModel); ok {
			return responseModel
		}
	}
	return ""
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

// matchDetailed resolves a model to a detailedPrice entry, mirroring
// matchPrice's three-step chain (exact table, prefix-stripped table,
// longest-prefix alias). It does not share the familyAliases table;
// if the alias map changes the basic price for a model, the detail
// table is consulted with the *family* key it resolves to, not the
// raw alias, so the two stay in sync without renaming every detail
// entry on an alias rename.
func matchDetailed(model string) (detailedPrice, bool) {
	if model == "" {
		return detailedPrice{}, false
	}
	if dp, ok := detailedPricingTable[model]; ok {
		return dp, true
	}
	if _, rest, found := strings.Cut(model, "/"); found {
		if dp, ok := detailedPricingTable[rest]; ok {
			return dp, true
		}
		if p, ok := matchAliasDetail(rest); ok {
			return p, true
		}
	}
	if p, ok := matchAliasDetail(model); ok {
		return p, true
	}
	return detailedPrice{}, false
}

// matchAliasDetail walks `familyAliases` in declaration order and returns
// the detailedPrice whose family key is registered in
// `detailedPricingTable`. Zero value falls back to the basic rate,
// which lets a basic-and-detailed pair live alongside each other.
func matchAliasDetail(model string) (detailedPrice, bool) {
	for _, a := range familyAliases {
		if a.prefix == model || strings.HasPrefix(model, a.prefix) {
			if dp, ok := detailedPricingTable[a.family]; ok {
				return dp, true
			}
		}
	}
	return detailedPrice{}, false
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

func applyBasic(p price, inTokens, outTokens int) float64 {
	return (float64(inTokens)/1e6)*p.inPerM + (float64(outTokens)/1e6)*p.outPerM
}

// applyDetailed breaks the headline input count into "uncached" +
// "cached" and the headline output count into "regular" + "reasoning"
// using `usage` (when provided), summing each at its component rate.
//
//   inTokens == uncached + cached (the upstream reports the cache
//     share inside prompt_tokens_details.cached_tokens). When the
//     cache field is absent, the whole prompt lands at the base rate.
//
//   outTokens == regular_output + reasoning (completion_tokens_details
//     splits the same way). `regular_output` falls back to total -
//     reasoning when only one of the two is reported.
//
// The function never goes negative: any difference is rolled back
// into the base component so the result is bounded below by applyBasic.
func applyDetailed(dp detailedPrice, inTokens, outTokens int, usage *Usage) float64 {
	cachedInRate := dp.cachedInPerM
	if cachedInRate == 0 {
		cachedInRate = dp.price.inPerM // cache hint missing → no discount
	}
	reasoningOutRate := dp.reasoningOutPerM
	if reasoningOutRate == 0 {
		reasoningOutRate = dp.price.outPerM // model has no extra premium
	}

	cachedIn := 0
	regularOut := outTokens
	reasoningOut := 0
	if usage != nil {
		if usage.PromptTokenDetails != nil && usage.PromptTokenDetails.CachedTokens > 0 {
			cachedIn = usage.PromptTokenDetails.CachedTokens
			if cachedIn > inTokens {
				cachedIn = inTokens // upstream never reports more caches than the total
			}
		}
		if usage.CompletionTokenDetails != nil && usage.CompletionTokenDetails.ReasoningTokens > 0 {
			reasoningOut = usage.CompletionTokenDetails.ReasoningTokens
			if reasoningOut > outTokens {
				reasoningOut = outTokens
			}
			regularOut = outTokens - reasoningOut
		}
	}
	uncached := inTokens - cachedIn
	if uncached < 0 {
		uncached = 0
	}
	if regularOut < 0 {
		regularOut = outTokens
	}

	return (float64(uncached)/1e6)*dp.price.inPerM +
		(float64(cachedIn)/1e6)*cachedInRate +
		(float64(regularOut)/1e6)*dp.price.outPerM +
		(float64(reasoningOut)/1e6)*reasoningOutRate
}
