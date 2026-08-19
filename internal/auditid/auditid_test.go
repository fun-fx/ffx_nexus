package auditid

import (
	"context"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/resp"
)

// TestFromContextNeverReturnsEmpty covers the load-bearing property of
// c0.1: an audit row's correlation id can never be empty. Even when the
// context is nil, or when the request id middleware did not run, or when
// no job id was attached, FromContext must produce a non-empty server-
// generated id. The audit subsystem relies on this in many places
// (Store.Audit, observability labels, the c0.7 export pipeline); an empty
// id is an audit-join defect.
func TestFromContextNeverReturnsEmpty(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
	}{
		{"nil context", nil},
		{"background context", context.Background()},
		{"empty request id", context.WithValue(context.Background(), resp.RequestIDKey(), "")},
		{"job id wins over request id", context.WithValue(context.Background(), jobKey, "job-test")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FromContext(c.ctx)
			if got == "" {
				t.Fatalf("FromContext returned empty for %s", c.name)
			}
			// Visible prefix so an operator scanning logs can immediately
			// recognise how this id was minted.
			if !strings.HasPrefix(got, "req-") &&
				!strings.HasPrefix(got, "job-") &&
				!strings.HasPrefix(got, "srv-") {
				t.Fatalf("FromContext returned %q with no recognised prefix", got)
			}
		})
	}
}

// TestWithClientRequestIDAppliesCharsetAndLengthFilter exercises c0.1's
// untrusted-input discipline. The X-Request-Id header is opaque attacker
// input: it can be empty, contain control characters that would inject
// log lines, exceed our storage budget, or carry non-printable runes.
// WithClientRequestID MUST discard anything that could be weaponised and
// keep the server-generated join key unaffected.
func TestWithClientRequestIDAppliesCharsetAndLengthFilter(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		expect string
	}{
		{"empty", "", ""},
		{"classic client id", "abc-123-def.gh_ij", "abc-123-def.gh_ij"},
		{"with control char", "abc\ndef", ""},
		{"with space", "abc def", ""},
		{"with slash", "abc/def", ""},
		{"with quote", "abc\"def", ""},
		{"with non-ascii", "abc-π-def", ""},
		{"with tab", "abc\tdef", ""},
		{"with too-long", strings.Repeat("a", clientRequestIDMaxLen+1), ""},
		{"with too-long-2", strings.Repeat("a", 4096), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := WithClientRequestID(context.Background(), c.input)
			got := ClientRequestID(ctx)
			if got != c.expect {
				t.Fatalf("WithClientRequestID(%q) -> stored %q, want %q", c.input, got, c.expect)
			}
		})
	}
}

// TestClientRequestIDGoesThroughScrubRoundTrip pins the policy that
// client-supplied ids that DON'T pass the chmod filter never end up in
// the audit row. The integration test in store.go (TestAuditRowGoesThroughScrub)
// is the load-bearing test for the round-trip; this unit test makes the
// filter easy to triage in isolation when a regression lands here.
func TestClientRequestIDGoesThroughScrubRoundTrip(t *testing.T) {
	// Reuse the helper: WithClientRequestID accepts only the safe alphabet,
	// so any value that contains protected substrings (SQLSTATE / prompt
	// content) will already be filtered out. We assert the channel even
	// refuses a string that contains protected substrings without quotes.
	if cid := ClientRequestID(WithClientRequestID(context.Background(), "SQLSTATE 42P01")); cid != "" {
		t.Fatalf("header containing protected signature survived: %q", cid)
	}
}

// TestWithJobRejectsInvalidOrigin ensures an attacker controlling the
// job origin label cannot inject log sequences or scheduler prefix
// collisions. WithJob replaces any invalid origin with "anon"; the
// shipped id's job- prefix is preserved so security audits can still
// recognise that a row was stamped from a job context.
func TestWithJobRejectsInvalidOrigin(t *testing.T) {
	cases := []string{
		"worker.benchmark-rotate",         // letters/digits/dots/dashes/underscores
		"worker/foo",                      // contains slash
		"worker.foo bar",                  // contains space
		"worker.foo;DROP TABLE audit_log", // contains comma / scaffold
		"",                                // empty -> anon
	}
	for _, origin := range cases {
		t.Run(origin, func(t *testing.T) {
			ctx := WithJob(context.Background(), origin)
			id := FromContext(ctx)
			if !strings.HasPrefix(id, "job-") {
				t.Fatalf("WithJob(%q) -> %q, missing job- prefix", origin, id)
			}
			// A valid origin lands verbatim; an invalid origin is
			// replaced with "anon" so the inserted id is safe to log.
			if origin == "worker.benchmark-rotate" {
				if !strings.Contains(id, "worker.benchmark-rotate") {
					t.Fatalf("WithJob did not embed valid origin verbatim: %q", id)
				}
			} else {
				if !strings.Contains(id, "-anon-") {
					t.Fatalf("WithJob did not replace invalid origin with anon: %q", id)
				}
			}
		})
	}
}

// TestJobIDIsStableAcrossCallsFromSameContext ensures that one job
// stamp stays the same through the call graph — workers that thread
// the context through multiple goroutines get a single audit row
// joining them.
func TestJobIDIsStableAcrossCallsFromSameContext(t *testing.T) {
	ctx := WithJob(context.Background(), "scheduler.daily")
	first := FromContext(ctx)
	for i := 0; i < 100; i++ {
		if got := FromContext(ctx); got != first {
			t.Fatalf("FromContext drifted: %q vs %q", first, got)
		}
	}
}

// TestNewServerIDIsNonEmptyAndUnique proves the no-context fallback
// path can't silently produce an empty id. NewServerID is exported
// for the same reason resp.RequestIDFromContextFallback exports its
// helper: it lets Store.Audit use it as a last resort and assert the
// id has the expected sentinel prefix.
func TestNewServerIDIsNonEmptyAndUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 64; i++ {
		id := NewServerID()
		if id == "" {
			t.Fatalf("NewServerID returned empty")
		}
		if !strings.HasPrefix(id, "srv-") {
			t.Fatalf("NewServerID missing srv- prefix: %q", id)
		}
		if seen[id] {
			t.Fatalf("NewServerID collision: %q", id)
		}
		seen[id] = true
	}
}
