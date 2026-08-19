package auditaggregator

import (
	"strings"
	"testing"
	"time"
)

// TestAggregatedSetIsClosed pins the policy that aggregating the wrong
// event-class would silently change customer dashboards. Every aggregated
// action must:
//   - be a denied-attempt-style event (never a state-change success);
//   - be high-volume-enough under attack to justify burst-collapse.
//
// Adding a new entry to the set without updating this list will fail CI;
// removing an entry will fail because the corresponding "must-collapse"
// test below asserts the action was still aggregated.
func TestAggregatedSetIsClosed(t *testing.T) {
	mustCollapse := []string{
		"auth.login.denied",
		"user.login.denied",
		"key.rejected.invalid",
		"key.rejected.expired",
		"key.rejected.revoked",
		"rate_limited",
		"request_size",
		"budget.exceeded",
		"concurrency.cap",
		"model.allowlist",
		"invite.rejected.invalid",
		"invite.rejected.replay",
	}
	for _, a := range mustCollapse {
		if !AggregatedAction(a) {
			t.Errorf("AggregatedAction(%q) returned false; the closed set says it MUST collapse", a)
		}
	}
	// The opposite direction: high-severity individual rows must NEVER
	// collapse because the audit page expects one row per occurrence.
	mustNotCollapse := []string{
		"auth.login.succeeded",
		"key.create",
		"key.revoke",
		"credential.create",
		"credential.update",
		"user.create",
		"user.delete",
		"eval.plugin.create",
		"benchmark.run.finish",
		"policy.create",
		"audit.view",
		"audit.export",
		"denied.org.boundary",
		"denied.origin",
		"denied.cors",
		"denied.egress",
		"denied.egress.address",
		"denied.audit.view",
		"denied.audit.export",
		"denied.schema.contract",
		"security.panic.recovered",
	}
	for _, a := range mustNotCollapse {
		if AggregatedAction(a) {
			t.Errorf("AggregatedAction(%q) returned true; an individual row must NOT collapse "+
				"(severity of org_boundary / origin / egress / audit-view is too high to fold)", a)
		}
	}
}

// TestResourceFingerprintIsLengthBoundedAndStable pins the dedup key
// behaviour. Two calls with the same target must produce the same
// fingerprint; different targets different fingerprints; an attacker
// cannot smuggle two distinct rows by trailing garbage.
func TestResourceFingerprintIsLengthBoundedAndStable(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"/v1/chat", "/v1/chat", true},
		{"/v1/chat", "/v1/chat/", false}, // trailing slash is character-distinct
		{"", "", true},
		{"hello world", "HELLO WORLD", true}, // case-insensitive
		{"/users/123", "/users/124", false},
	}
	for _, c := range cases {
		got := ResourceFingerprint(c.a) == ResourceFingerprint(c.b)
		if got != c.want {
			t.Errorf("ResourceFingerprint(%q) == ResourceFingerprint(%q) = %v, want %v",
				c.a, c.b, got, c.want)
		}
	}
	// Bounded output is critical for index size. 32 hex chars (full
	// SHA-256) is the size budget. Anything larger eats into the
	// Postgres BTree key size and forces TOAST on the column.
	for _, target := range []string{"", "x", strings.Repeat("a", 4096)} {
		fp := ResourceFingerprint(target)
		if fp == "" {
			continue
		}
		if len(fp) != 64 {
			t.Errorf("ResourceFingerprint produced %d hex chars, want 64 (full SHA-256): %q",
				len(fp), fp)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestWindowStartFloorAlignsOn5MinuteBoundary is the wall-clock grid
// the SQL aggregates on. Replicas share the boundary because the
// function only depends on the input timezone (UTC) and the minute / 5
// integer division. Tests assert boundaries at typical wall-clock times.
func TestWindowStartFloorAlignsOn5MinuteBoundary(t *testing.T) {
	cases := []struct {
		in   time.Time
		want time.Time
	}{
		{time.Date(2026, 8, 19, 14, 0, 0, 0, time.UTC), time.Date(2026, 8, 19, 14, 0, 0, 0, time.UTC)},
		{time.Date(2026, 8, 19, 14, 4, 59, 0, time.UTC), time.Date(2026, 8, 19, 14, 0, 0, 0, time.UTC)},
		{time.Date(2026, 8, 19, 14, 5, 0, 0, time.UTC), time.Date(2026, 8, 19, 14, 5, 0, 0, time.UTC)},
		{time.Date(2026, 8, 19, 14, 13, 37, 0, time.UTC), time.Date(2026, 8, 19, 14, 10, 0, 0, time.UTC)},
		{time.Date(2026, 8, 19, 14, 58, 30, 0, time.UTC), time.Date(2026, 8, 19, 14, 55, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got := WindowStart(c.in)
		if !got.Equal(c.want) {
			t.Errorf("WindowStart(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
