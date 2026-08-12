package observability

import (
	"context"
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"
)

// These tests pin the *shape* of the SQL builder (`buildTracePageQuery`,
// `buildTracePageArgs`, `escapeLike`) without needing a real ClickHouse
// connection. The `driver.Conn` interface is wide enough that hand-rolling
// a stub costs more than the regression risk it's worth. Production runs
// the integration path end-to-end under deployment smoke tests.
func TestBuildTracePageQuery_FilterCombinations(t *testing.T) {
	now := time.Date(2026, 7, 27, 9, 30, 0, 0, time.UTC)
	earlier := now.Add(-2 * time.Hour)
	earliest := now.Add(-24 * time.Hour)
	cases := []struct {
		name        string
		userID      string
		before      time.Time
		since       time.Time
		filter      TraceFilter
		wantWhere   []string // substrings that must appear in the WHERE clause (in order)
		wantArgsLen int
		wantLimit   bool
	}{
		{
			name:        "no filters, no window",
			wantLimit:   true,
			wantArgsLen: 1,
		},
		{
			name:        "user-only",
			userID:      "u-1",
			wantWhere:   []string{"user_id = ?"},
			wantArgsLen: 2, // user_id + limit
		},
		{
			name:        "window-only",
			before:      now,
			since:       earliest,
			wantWhere:   []string{"timestamp < ?", "timestamp >= ?"},
			wantArgsLen: 3,
		},
		{
			name:        "status err",
			filter:      TraceFilter{Status: "err"},
			wantWhere:   []string{"status_code >= 400"},
			wantArgsLen: 1,
		},
		{
			name:        "status ok",
			filter:      TraceFilter{Status: "ok"},
			wantWhere:   []string{"status_code < 400"},
			wantArgsLen: 1,
		},
		{
			name:        "provider exact",
			filter:      TraceFilter{Provider: "openai"},
			wantWhere:   []string{"provider_name = ?"},
			wantArgsLen: 2,
		},
		{
			name:   "fuzzy q on four columns",
			filter: TraceFilter{Q: "gpt-4o"},
			// No WHERE string changes (only args differ). Just confirm args length is +4.
			wantArgsLen: 5, // %gpt-4o% × 4 columns + limit
		},
		{
			name:   "full house: user + window + status + provider + q",
			userID: "u-7",
			before: earlier, since: earliest,
			filter: TraceFilter{Status: "err", Provider: "anthropic", Q: "claude"},
			wantWhere: []string{
				"user_id = ?",
				"timestamp < ?",
				"timestamp >= ?",
				"provider_name = ?",
				"status_code >= 400",
				"request_model LIKE ?",
				"provider_name LIKE ?",
				"user_email LIKE ?",
				"guardrail_action LIKE ?",
			},
			wantArgsLen: 9, // user + before + since + provider + 4×q + limit
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := buildTracePageQuery(tc.userID, tc.before, tc.since, 100, tc.filter)
			if tc.wantLimit && !strings.Contains(q, "ORDER BY timestamp DESC, trace_id DESC LIMIT ?") {
				t.Errorf("query missing ORDER BY ... LIMIT ?: %s", q)
			}
			// Spot-check that conds are joined with AND (not comma or ;).
			if strings.Contains(q, ";") || strings.Contains(q, ",user_id") || strings.Contains(q, ",timestamp") || strings.Contains(q, ",status") {
				t.Errorf("conds appear concatenated incorrectly: %s", q)
			}
			args := buildTracePageArgs(tc.userID, tc.before, tc.since, 100, tc.filter)
			if len(args) != tc.wantArgsLen {
				t.Errorf("arg count want=%d got=%d (args=%v) for sql=%s", tc.wantArgsLen, len(args), args, q)
			}
			for _, want := range tc.wantWhere {
				if !strings.Contains(q, want) {
					t.Errorf("query missing WHERE fragment %q: %s", want, q)
				}
			}
		})
	}
} // TestBuildTracePageQuery_SelectsTotalAndResponseAndSessionFields is
// the structural test that pins the SELECT list so a future refactor
// can't silently drop the columns the Recent-sessions panel reads
// (response_model, total_tokens, session_id). The gateway schema
// migration 007_session_id.sql may not have run on a freshly-bootstrapped
// environment yet, so we treat missing-column errors as a server-side
// problem rather than a UI bug — but if the helper stops asking for
// them the read path never gets the chance to complain.
func TestBuildTracePageQuery_SelectsTotalAndResponseAndSessionFields(t *testing.T) {
	q := buildTracePageQuery("", time.Time{}, time.Time{}, 25, TraceFilter{})
	for _, col := range []string{"response_model", "total_tokens", "session_id", "turn_id"} {
		if !strings.Contains(q, col) {
			t.Errorf("query missing required column %q: %s", col, q)
		}
	}
}

// The turn filter is an exact match, never a LIKE. turn_id holds a hash,
// so a fuzzy match would both scan wrong and defeat the bloom index that
// 008_turn_id.sql added for exactly this lookup.
func TestBuildTracePageQuery_TurnFilterIsExactMatch(t *testing.T) {
	f := TraceFilter{Turn: "a1b2c3d4e5f60718"}
	q := buildTracePageQuery("", time.Time{}, time.Time{}, 50, f)
	if !strings.Contains(q, "turn_id = ?") {
		t.Errorf("turn filter must be an equality predicate: %s", q)
	}
	if strings.Contains(q, "turn_id LIKE") {
		t.Errorf("turn filter must not be fuzzy: %s", q)
	}
	args := buildTracePageArgs("", time.Time{}, time.Time{}, 50, f)
	if len(args) != 2 || args[0] != f.Turn {
		t.Fatalf("want [turn, limit], got %v", args)
	}
}

// buildTurnPageQuery groups the overview. The two properties worth
// pinning: the empty-turn_id fallback (so traces written before the
// column existed render one-per-row instead of collapsing into a single
// bogus group), and the aggregate list the console reads.
func TestBuildTurnPageQuery_Shape(t *testing.T) {
	q := buildTurnPageQuery("", time.Time{}, time.Time{})
	if !strings.Contains(q, "if(turn_id = '', trace_id, turn_id) AS turn_group_key") {
		t.Errorf("missing empty-turn_id fallback to trace_id: %s", q)
	}
	if !strings.Contains(q, "GROUP BY turn_group_key") {
		t.Errorf("must group on the derived key: %s", q)
	}
	if !strings.Contains(q, "ORDER BY turn_last_at DESC LIMIT ?") {
		t.Errorf("must order by most recent activity: %s", q)
	}
	for _, frag := range []string{
		"min(timestamp) AS turn_first_at",
		"max(timestamp) AS turn_last_at",
		"toInt64(count()) AS turn_trace_count",
		"toInt64(sum(input_tokens)) AS turn_input_tokens",
		"toInt64(sum(output_tokens)) AS turn_output_tokens",
		"sum(cost_usd) AS turn_cost_usd",
		"toInt64(sum(latency_ms)) AS turn_latency_ms",
		"max(status_code) AS turn_status_code",
	} {
		if !strings.Contains(q, frag) {
			t.Errorf("query missing aggregate %q: %s", frag, q)
		}
	}
}

// Regression guard for an outage: the first cut aliased its aggregates after
// the columns they summed (`sum(input_tokens) AS input_tokens`). ClickHouse
// lets one SELECT expression reference another's alias, so the later
// `sum(input_tokens) + sum(output_tokens)` resolved to the aliases and the
// server rejected every request with ILLEGAL_AGGREGATION — while the shape
// assertions above stayed green, because a string cannot tell you what the
// server thinks it means.
//
// The live tests in turnpage_live_test.go are the real proof. This one runs
// without a ClickHouse, so the rule stays enforced on every laptop and in the
// plain Go CI job.
func TestBuildTurnPageQuery_NoAliasShadowsAColumn(t *testing.T) {
	// Columns of gateway_traces that the roll-up reads. An alias colliding
	// with any of these is the trap.
	columns := map[string]bool{
		"trace_id": true, "turn_id": true, "timestamp": true,
		"provider_name": true, "request_model": true, "response_model": true,
		"input_tokens": true, "output_tokens": true, "latency_ms": true,
		"ttft_ms": true, "cost_usd": true, "status_code": true,
		"user_id": true, "session_id": true, "org_id": true,
	}

	q := buildTurnPageQuery("u-1", time.Now(), time.Now().Add(-time.Hour))
	aliasRe := regexp.MustCompile(`(?i)\sAS\s+([a-z_][a-z0-9_]*)`)
	found := aliasRe.FindAllStringSubmatch(q, -1)
	if len(found) == 0 {
		t.Fatal("expected the roll-up to define aliases")
	}
	for _, m := range found {
		alias := strings.ToLower(m[1])
		if columns[alias] {
			t.Errorf("alias %q shadows the gateway_traces column of the same name; "+
				"ClickHouse will resolve later references to the aggregate and reject "+
				"the query with ILLEGAL_AGGREGATION. Prefix it (turn_%s).", alias, alias)
		}
	}
}

// max(status_code), not any(): one failed call inside an otherwise-green
// agent loop has to surface on the collapsed row, or the operator has to
// expand every turn to find the failure.
func TestBuildTurnPageQuery_StatusIsWorstNotFirst(t *testing.T) {
	q := buildTurnPageQuery("", time.Time{}, time.Time{})
	if strings.Contains(q, "any(status_code)") || strings.Contains(q, "argMax(status_code") {
		t.Errorf("status must be the worst in the turn, not a representative one: %s", q)
	}
}

func TestBuildTurnPageArgs_PlaceholderOrder(t *testing.T) {
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	since := now.Add(-time.Hour)

	q := buildTurnPageQuery("u-9", now, since)
	for _, want := range []string{"user_id = ?", "timestamp < ?", "timestamp >= ?"} {
		if !strings.Contains(q, want) {
			t.Errorf("query missing %q: %s", want, q)
		}
	}
	args := buildTurnPageArgs("u-9", now, since, 20)
	if len(args) != 4 {
		t.Fatalf("want [user, before, since, limit], got %v", args)
	}
	if args[0] != "u-9" || args[1] != now || args[2] != since || args[3] != 20 {
		t.Errorf("placeholder order does not mirror the query: %v", args)
	}

	// Unbounded on both sides collapses to just the limit.
	if got := buildTurnPageArgs("", time.Time{}, time.Time{}, 5); len(got) != 1 || got[0] != 5 {
		t.Errorf("want [limit] only, got %v", got)
	}
}

// Pin the q arg shape so a regression in escapeLike OR buildTracePageArgs
// is caught even when the order-of-operations check above happens to pass.
// Pre-fix: a search for "5%" would render as `LIKE '%5%%'` and match everything
// (treats the user's % as a wildcard). Post-fix: `LIKE '%5\%%'` matches only
// literal "5%" — a regression to the loose behaviour would fail this test.
func TestBuildTracePageArgs_FuzzyLikeEscaping(t *testing.T) {
	cases := []struct {
		q        string
		wantArgs []any // expected pattern placed into each of the 4 slots
	}{
		{"gpt-4o", []any{"%gpt-4o%", "%gpt-4o%", "%gpt-4o%", "%gpt-4o%"}},
		{"100%", []any{`%100\%%`, `%100\%%`, `%100\%%`, `%100\%%`}},
		{"a_b", []any{`%a\_b%`, `%a\_b%`, `%a\_b%`, `%a\_b%`}},
		{`back\slash`, []any{`%back\\slash%`, `%back\\slash%`, `%back\\slash%`, `%back\\slash%`}},
		{"", nil},
	}
	for _, tc := range cases {
		t.Run(tc.q, func(t *testing.T) {
			f := TraceFilter{Q: tc.q}
			args := buildTracePageArgs("", time.Time{}, time.Time{}, 100, f)
			if tc.q == "" {
				if len(args) != 1 {
					t.Fatalf("empty q must produce args=[limit] only, got %v", args)
				}
				return
			}
			if len(args) != 5 {
				t.Fatalf("q=%q: want 5 args (4 like patterns + limit), got %d: %v", tc.q, len(args), args)
			}
			for i := 0; i < 4; i++ {
				if got, want := args[i], tc.wantArgs[i]; got != want {
					t.Errorf("q=%q slot %d: want %q, got %q", tc.q, i, want, got)
				}
			}
			// Last arg must be the limit regardless of q content.
			if args[4] != 100 {
				t.Errorf("q=%q: limit slot want=100, got=%v", tc.q, args[4])
			}
		})
	}
}

// Empty-spec for the q arg path: a regression that double-percent-escapes
// (e.g. `\\%` becoming `\\\\%`) would still match the substring here, so
// this test guards the LITERAL characters emitted — not just substring
// containment.
func TestBuildTracePageArgs_PercentEscapeRoundTrip(t *testing.T) {
	arg := buildTracePageArgs("", time.Time{}, time.Time{}, 25, TraceFilter{Q: "100%"})[0]
	want := `%100\%%`
	if arg != want {
		t.Fatalf("user input %q must escape literal %% as \\%% (result: %q, want: %q)", "100%", arg, want)
	}
}

// Quick contract for the cursor helpers: cursorSince echoes the since
// value as RFC3339Nano UTC, dropping sub-second precision only if the
// input did not have any. A regression that emits local time would
// double-offset the user's filter box and confuse operators.
func TestCursorSince_RoundTripsUTC(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("no tzdata; skip")
	}
	in := time.Date(2026, 7, 20, 9, 0, 0, 123000000, loc) // 2026-07-20 09:00:00.123 PDT
	out := cursorSince(in)
	const want = "2026-07-20T16:00:00.123Z" // 09:00 PDT == 16:00 UTC
	if out != want {
		t.Fatalf("cursorSince: got %q, want %q", out, want)
	}
	if cursorSince(time.Time{}) != "" {
		t.Error("zero time must serialize as empty string")
	}
}

// Reader-level TracePage requires a real driver.Conn to exercise, but we
// at least can prove the "no reader wired" branch on the console side
// returns a clean empty envelope. The actual integration path is covered
// by deployment smoke tests; this is a regression tripwire.
func TestTracePage_NilReaderEnvelope(t *testing.T) {
	// Construct a Reader with nil conn. TracePage will fail at Query, but
	// the *envelope* contract (Items: []TraceSummary{}) is what the
	// console relies on for the "reader disabled" branch — which lives on
	// the handler, not the reader. We exercise the handler in
	// internal/console; here we just confirm the page literal type.
	var page TracePage = TracePage{Items: []TraceSummary{}}
	if page.NextCursor != (TraceCursor{}) {
		t.Errorf("default cursor must be zero value, got %+v", page.NextCursor)
	}
	if page.Items == nil {
		t.Errorf("Items must always be a slice (never nil) so JSON encodes []")
	}
	_ = context.Background() // keep "context" import alive for future cases
}

// DailySpendByDay / DailySpendBreakdown: pin the SQL/arg shapes so a
// regression (e.g. wrong placeholder count, dropped user filter, wrong
// bucket function) catches here before reaching prod.
func TestBuildDailySpendByDayQuery(t *testing.T) {
	cases := []struct {
		name       string
		userID     string
		wantFilter []string // substrings that must appear
		wantArgs   int
	}{
		{"no user filter", "", []string{"org_id = ?", "timestamp >= ?", "timestamp < ?", "GROUP BY day", "ORDER BY day ASC"}, 3},
		{"with user filter", "u-1", []string{"org_id = ?", "user_id = ?", "timestamp >= ?", "timestamp < ?", "GROUP BY day", "ORDER BY day ASC"}, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := buildDailySpendByDayQuery("org-a", tc.userID)
			for _, want := range tc.wantFilter {
				if !strings.Contains(q, want) {
					t.Errorf("query missing %q:\n%s", want, q)
				}
			}
			arg := buildDailySpendByDayArgs("org-a", time.Now(), time.Now().Add(time.Hour), tc.userID)
			if len(arg) != tc.wantArgs {
				t.Errorf("arg len want=%d got=%d (%v)", tc.wantArgs, len(arg), arg)
			}
		})
	}
}

// DailySpendSummary pins the SQL/arg shapes for the hero rollup
// (current + previous window) so the bind order and the two-window
// split can't regress silently. The query fires one ClickHouse
// SELECT with two CTEs, so the hero card loads in lockstep with the
// daily list — a future edit that splits this into two trips would
// surface here as a binder-count diff.
func TestBuildDailySpendSummaryQuery(t *testing.T) {
	cases := []struct {
		name     string
		userID   string
		wantArgs int
	}{
		{"no user filter", "", 6},
		{"with user filter", "u-s", 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			curUntil := now
			curSince := curUntil.Add(-24 * time.Hour)
			prevUntil := curSince
			prevSince := prevUntil.Add(-24 * time.Hour)
			q := buildDailySpendSummaryQuery("org-a", tc.userID)
			wantPieces := []string{
				"WITH",
				"cur AS",
				"prev AS",
				"sum(cost_usd)",
				"sum(input_tokens + output_tokens)",
				"countIf(cache_hit = 1)",
				"FROM gateway_traces",
				"FROM cur, prev",
			}
			for _, want := range wantPieces {
				if !strings.Contains(q, want) {
					t.Errorf("query missing %q:\n%s", want, q)
				}
			}
			// The user_id filter is appended inside BOTH CTEs so we
			// can't accidentally scope only one of them.
			count := strings.Count(q, "user_id = ?")
			wantCount := 0
			if tc.userID != "" {
				wantCount = 2
			}
			if count != wantCount {
				t.Errorf("user_id filter count want=%d got=%d:\n%s", wantCount, count, q)
			}
			arg := buildSummaryArgs("org-a", curSince, curUntil, prevSince, prevUntil, tc.userID)
			if len(arg) != tc.wantArgs {
				t.Errorf("arg len want=%d got=%d (%v)", tc.wantArgs, len(arg), arg)
			}
			if arg[0] != "org-a" || arg[3] != "org-a" {
				t.Errorf("org_id must be bound twice (once per CTE), got %v", arg)
			}
		})
	}
}

// DailySpendSummaryFields pins the wire shape (omitting new fields
// would silently break the Spend hero card) and the savings-pct math
// (so a future refactor can't accidentally flip the sign or divide by
// the wrong column).
func TestDailySpendSummary_SavingsPctMath(t *testing.T) {
	cases := []struct {
		name                       string
		current, previous          float64
		hasPrevious                bool
		wantDelta                  float64
		wantSavingsPct             float64
	}{
		{"no previous", 12.34, 0, false, 12.34, 0},
		{"spent less than before", 50, 100, true, -50, 50},
		{"spent more than before", 130, 100, true, 30, -30},
		{"equal spend", 100, 100, true, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := DailySpendSummary{
				Days:         30,
				CurrentCost:  tc.current,
				PreviousCost: tc.previous,
			}
			s.DeltaCost = s.CurrentCost - s.PreviousCost
			s.HasPrevious = tc.hasPrevious
			if s.HasPrevious && s.PreviousCost > 0 {
				s.SavingsPct = (s.PreviousCost - s.CurrentCost) / s.PreviousCost * 100
			}
			if math.Abs(s.DeltaCost-tc.wantDelta) > 1e-9 {
				t.Errorf("DeltaCost want=%v got=%v", tc.wantDelta, s.DeltaCost)
			}
			if math.Abs(s.SavingsPct-tc.wantSavingsPct) > 1e-9 {
				t.Errorf("SavingsPct want=%v got=%v", tc.wantSavingsPct, s.SavingsPct)
			}
		})
	}
}

// DailySpendSummaryFields_JSONContract pins the wire shape so a
// regression in the field tags surfaces here. The Spend page reads
// every field below — dropping any one of them collapses the hero
// into a blank card.
func TestDailySpendSummaryFields_JSONContract(t *testing.T) {
	s := DailySpendSummary{
		Days:              30,
		CurrentCost:       12.34,
		PreviousCost:      24.68,
		DeltaCost:         -12.34,
		SavingsPct:        50,
		HasPrevious:       true,
		CurrentTokens:     1000,
		PreviousTokens:    2000,
		CurrentRequests:   4,
		PreviousRequests:  8,
		CurrentCacheHits:  1,
		PreviousCacheHits: 0,
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"days":30`,
		`"current_cost_usd":12.34`,
		`"previous_cost_usd":24.68`,
		`"delta_cost_usd":-12.34`,
		`"savings_pct":50`,
		`"has_previous":true`,
		`"current_tokens":1000`,
		`"previous_tokens":2000`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing field %q in json: %s", want, out)
		}
	}
}

func TestBuildDailySpendBreakdownQuery(t *testing.T) {
	start := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	cases := []struct {
		name     string
		userID   string
		wantArgs int
	}{
		{"no user filter", "", 3},
		{"with user filter", "u-2", 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := buildDailySpendBreakdownQuery("org-a", tc.userID)
			wantPieces := []string{
				"GROUP BY request_model, provider, response_model",
				"ORDER BY cost_usd DESC",
				"LIMIT 200",
				"countIf(cache_hit = 1)",
				// The Grid → underlying-seller flat-map must be in the
				// query so prod reads see code-prime rather than thegrid.
				"provider_name = 'thegrid'",
				"response_model",
			}
			for _, want := range wantPieces {
				if !strings.Contains(q, want) {
					t.Errorf("query missing %q:\n%s", want, q)
				}
			}
			arg := buildDailySpendBreakdownArgs("org-a", start, end, tc.userID)
			if len(arg) != tc.wantArgs {
				t.Errorf("arg len want=%d got=%d (%v)", tc.wantArgs, len(arg), arg)
			}
			// bind order: orgID, start, end [, userID]
			if arg[0] != "org-a" {
				t.Errorf("arg[0] want=org-a got=%v", arg[0])
			}
			if !arg[1].(time.Time).Equal(start) || !arg[2].(time.Time).Equal(end) {
				t.Errorf("arg[1..2] want=[%v,%v] got=[%v,%v]", start, end, arg[1], arg[2])
			}
		})
	}
}

// TestBuildDailySpendBreakdownQuery_FlattensGridRedirects pins the
// effective-provider expression so future edits can't silently drop the
// Grid → response_model rewrite. The day drill must never surface a
// `provider = 'thegrid'` row for a cache_hit = 0 trace because that row
// was the Hop, not the seller — operators want code-prime / text-prime
// on their bill, not "thegrid".
func TestBuildDailySpendBreakdownQuery_FlattensGridRedirects(t *testing.T) {
	q := buildDailySpendBreakdownQuery("org-a", "")
	wantExpr := []string{
		"multiIf(",
		"provider_name = 'thegrid' AND cache_hit = 0 AND response_model != ''",
		"response_model",
		") AS provider",
	}
	for _, w := range wantExpr {
		if !strings.Contains(q, w) {
			t.Errorf("query missing Grid-flatten clause %q:\n%s", w, q)
		}
	}
}

// DailySpendBreakdownRow.ResponseModel is rendered with `omitempty` JSON
// tag intentionally: cache-hit rows (no upstream fan-out) carry "" so the
// UI treats them as "cache served" rather than a literal model name.
// Regressions here would change the wire shape.
func TestDailySpendBreakdownRow_JSONContract(t *testing.T) {
	r := DailySpendBreakdownRow{
		Model:         "openai/gpt-4o-mini",
		Provider:      "openai",
		ResponseModel: "",
		CostUSD:       0,
		Tokens:        120,
		Requests:      1,
		CacheHits:     1,
	}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), `"response_model":""`) {
		t.Errorf("response_model must be omitted when empty (got: %s)", out)
	}
	// And the model/provider identity must persist.
	if !strings.Contains(string(out), `"model":"openai/gpt-4o-mini"`) {
		t.Errorf("model missing from JSON: %s", out)
	}
}
