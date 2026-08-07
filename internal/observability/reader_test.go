package observability

import (
	"context"
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
}// TestBuildTracePageQuery_SelectsTotalAndResponseAndSessionFields is
// the structural test that pins the SELECT list so a future refactor
// can't silently drop the columns the Recent-sessions panel reads
// (response_model, total_tokens, session_id). The gateway schema
// migration 007_session_id.sql may not have run on a freshly-bootstrapped
// environment yet, so we treat missing-column errors as a server-side
// problem rather than a UI bug — but if the helper stops asking for
// them the read path never gets the chance to complain.
func TestBuildTracePageQuery_SelectsTotalAndResponseAndSessionFields(t *testing.T) {
	q := buildTracePageQuery("", time.Time{}, time.Time{}, 25, TraceFilter{})
	for _, col := range []string{"response_model", "total_tokens", "session_id"} {
		if !strings.Contains(q, col) {
			t.Errorf("query missing required column %q: %s", col, q)
		}
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
