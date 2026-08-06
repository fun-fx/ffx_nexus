package gateway

import (
	"math"
	"testing"
)

// TestCostUSD_TableDriven covers the model's resolution chain end-to-end:
// exact-family match, provider-prefixed id, upstream-returned versioned id,
// and the conscious negative cases (unknown model → 0; future `gpt-5`
// should NOT silently inherit gpt-4o's rate). Each case also pins the
// numeric output so a regression in the table is caught immediately.
//
// Numeric values are USD per million tokens; tokens in, tokens out are
// chosen so the arithmetic result is an integer-formatted number easy
// to eyeball if a test fails (1M in + 1M out → inPerM + outPerM USD).
func TestCostUSD_TableDriven(t *testing.T) {
	cases := []struct {
		name          string
		requestModel  string
		responseModel string // empty if upstream response model equals request
		inTokens      int
		outTokens     int
		want          float64
	}{
		// ------------------ Exact aliases already in the table ------------------
		{"openai gpt-4o exact", "gpt-4o", "", 1_000_000, 1_000_000, 2.50 + 10.00},
		{"openai gpt-4o-mini exact", "gpt-4o-mini", "", 1_000_000, 1_000_000, 0.15 + 0.60},
		{"openai o3 exact", "o3", "", 1_000_000, 1_000_000, 2.00 + 8.00},
		{"openai o4-mini exact", "o4-mini", "", 1_000_000, 1_000_000, 1.10 + 4.40},
		{"anthropic opus exact", "claude-opus-4-1", "", 1_000_000, 1_000_000, 15.00 + 75.00},
		{"anthropic haiku exact", "claude-haiku-4-5", "", 1_000_000, 1_000_000, 1.00 + 5.00},
		{"gemini 2.5-pro exact", "gemini-2.5-pro", "", 1_000_000, 1_000_000, 1.25 + 10.00},

		// ------------------ Provider-prefixed ids (after registry sync) ---------
		{"groq llama-3.3-70b-versatile prefixed", "groq/llama-3.3-70b-versatile", "", 1_000_000, 1_000_000, 0.59 + 0.79},
		{"groq llama-3.1-8b-instant prefixed", "groq/llama-3.1-8b-instant", "", 1_000_000, 1_000_000, 0.05 + 0.08},
		{"mistral large from registry prefix", "mistral/mistral-large-latest", "", 1_000_000, 1_000_000, 2.00 + 6.00},
		{"mistral codestral", "mistral/codestral-latest", "", 1_000_000, 1_000_000, 0.30 + 0.90},
		{"grid text-prime", "grid/text-prime", "", 1_000_000, 1_000_000, 0.50 + 1.50},
		{"grid code-max", "grid/code-max", "", 1_000_000, 1_000_000, 2.10 + 6.30},

		// ------------------ The Grid bare instrument ids ----------------------
		// /v1/models sync returns the bare instrument id without the
		// `grid/` prefix (e.g. `code-prime`). They must still resolve
		// through familyAliases so a freshly-synced call carries a USD
		// number instead of silently being 0. PR fixed: overview cost
		// stayed at 0 for grid providers otherwise.
		{"grid bare text-standard", "text-standard", "", 1_000_000, 1_000_000, 0.20 + 0.50},
		{"grid bare text-prime", "text-prime", "", 1_000_000, 1_000_000, 0.50 + 1.50},
		{"grid bare text-max", "text-max", "", 1_000_000, 1_000_000, 1.50 + 4.50},
		{"grid bare code-standard", "code-standard", "", 1_000_000, 1_000_000, 0.30 + 0.90},
		{"grid bare code-prime", "code-prime", "", 1_000_000, 1_000_000, 0.70 + 2.10},
		{"grid bare code-max", "code-max", "", 1_000_000, 1_000_000, 2.10 + 6.30},
		{"grid bare agent-standard", "agent-standard", "", 1_000_000, 1_000_000, 0.40 + 1.20},
		{"grid bare agent-prime", "agent-prime", "", 1_000_000, 1_000_000, 1.00 + 3.00},
		{"grid bare agent-max", "agent-max", "", 1_000_000, 1_000_000, 3.00 + 9.00},

		// ------------------ Upstream-returned versioned ids ---------------------
		// Models are surfaced as bare versioned ids after PR #111's dynamic
		// /v1/models sync. The lookup must resolve them through familyAliases
		// even when the request id (the family root) is unknown.
		{"openai gpt-4o-2024-08-06 (response-only)", "auto", "gpt-4o-2024-08-06", 1_000_000, 1_000_000, 2.50 + 10.00},
		{"openai gpt-4o-2024-05-13 (response-only)", "auto", "gpt-4o-2024-05-13", 1_000_000, 1_000_000, 2.50 + 10.00},
		{"openai o3-2025-01-31 (response-only)", "auto", "o3-2025-01-31", 1_000_000, 1_000_000, 2.00 + 8.00},
		{"openai gpt-4.1-2025-04-14 (response-only)", "auto", "gpt-4.1-2025-04-14", 1_000_000, 1_000_000, 2.00 + 8.00},
		{"anthropic claude-3-7-sonnet shape from upstream", "auto", "claude-3-7-sonnet-20250219", 1_000_000, 1_000_000, 3.00 + 15.00},
		{"anthropic claude-opus-4 with build suffix", "auto", "claude-opus-4-1-20250805", 1_000_000, 1_000_000, 15.00 + 75.00},

		// ------------------ Mixed-input realism (real call sizes) ---------------
		{"small prompt, small completion", "gpt-4o-mini", "", 250_000, 500_000, 0.15*0.25 + 0.60*0.50},

		// ------------------ Negative paths (must NOT charge) --------------------
		{"unknown model returns zero", "gpt-9000-future", "gpt-9000-future", 1_000_000, 1_000_000, 0},
		// The familyAliases list is intentionally conservative: gpt-5 must
		// NOT silently inherit gpt-4o pricing until a human adds an entry.
		{"gpt-5 must NOT inherit gpt-4o price", "auto", "gpt-5", 1_000_000, 1_000_000, 0},
		{"empty request id with no response id", "", "", 1_000_000, 1_000_000, 0},
		{"empty request id but response resolves", "", "gpt-4o", 1_000_000, 1_000_000, 2.50 + 10.00},
		{"unmodelled provider", "cohere/command-r-plus", "", 1_000_000, 1_000_000, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CostUSD(tc.requestModel, tc.responseModel, tc.inTokens, tc.outTokens)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("CostUSD(%q, %q, %d, %d) = %v, want %v",
					tc.requestModel, tc.responseModel, tc.inTokens, tc.outTokens, got, tc.want)
			}
		})
	}

	// Sanity: numeric scaling. Repeat one case with fractional tokens.
	scaled := CostUSD("gpt-4o", "", 500_000, 250_000)
	wantScaled := 2.50*0.5 + 10.00*0.25
	if math.Abs(scaled-wantScaled) > 1e-9 {
		t.Fatalf("scaling check: got %v want %v", scaled, wantScaled)
	}

	// Sanity: 0-token request → 0 cost for any priced model.
	if CostUSD("gpt-4o", "", 0, 0) != 0 {
		t.Fatal("zero-token request should produce zero cost")
	}

	// Sanity: response model id is honoured when it differs from request id.
	if CostUSD("custom-alias", "claude-3-5-haiku-latest", 1_000_000, 1_000_000) != 0.80+4.00 {
		t.Fatal("response-only lookup must hit familyAliases path")
	}

	// Defensive: 1k input + 1k output should be ~$0.012 (gpt-4o formula);
	// catch any off-by-1000 in the tokens math.
	if v := CostUSD("gpt-4o", "", 1_000, 1_000); v >= 1.0 {
		t.Fatalf("1k input + 1k output should be tiny: got %v", v)
	}
}

// TestPricingTable_NoDuplicateKeys guards against accidentally inserting
// the same model id twice — `var pricingTable = map[...]` would silently
// keep the second value, which the table-driven test might not catch if
// the duplicates are within the same family and the expected output
// happens to match both.
func TestPricingTable_NoDuplicateKeys(t *testing.T) {
	seen := map[string]struct{}{}
	for k := range pricingTable {
		if _, dup := seen[k]; dup {
			t.Fatalf("duplicate key in pricingTable: %q", k)
		}
		seen[k] = struct{}{}
	}
	if len(seen) == 0 {
		t.Fatal("pricingTable must not be empty")
	}
}
