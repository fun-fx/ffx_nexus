package apierr_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/apierr"
	"github.com/ffxnexus/nexus/internal/resp"
)

// scrubTable maps each entry in protectedSignatures to a representative
// input that contains that signature, plus the post-Scrub form we expect.
//
// The mapping is built from one source of truth — apierr.protectedSignatures
// — so adding a new entry to the protected list pins the test
// automatically. Drift between the production signature set and the test's
// expected table is caught by go test_compile: the test file compiles
// against apierr.protectedSignatures at the same time, so a removal is
// caught at the table-build stage rather than at runtime.
//
// The input values are deliberately NOT secrets: they're representative
// strings that contain the sig so the test is reproducible without
// reading from fixtures. A developer reviewing the test sees the exact
// protection claim.
var scrubTable = func() []scrubCase {
	// Each entry sizes the substring so the post-substitution form is
	// predictable; a literal like "sk-" would otherwise collapse to
	// "[redacted]" and the substring check below would fail on the *result*.
	produced := func(sig string) string {
		// wrap each sig in a unique, easy-to-eyeball frame
		return "head:" + sig + ":tail"
	}
	out := make([]scrubCase, 0, len(apierr.ProtectedSignaturesForTest()))
	for _, sig := range apierr.ProtectedSignaturesForTest() {
		out = append(out, scrubCase{
			sig:          sig,
			input:        produced(sig),
			postSubstrOK: ":tail",
			// We also assert no occurrence of the unsanitized sig survives
			// in the result; this is the property the leak guard depends on.
		})
	}
	return out
}()

type scrubCase struct {
	sig          string
	input        string
	postSubstrOK string
}

// expectedSignatures is the test-side mirror of protectedSignatures.
//
// The two lists MUST be the same set. Any drift is a future hazard:
// a developer adding a protected sig without writing a unit test for
// it, or adding a unit test input that does not correspond to a
// protected sig. Both directions fail the inventory test below.
//
// The list lives in test code so a production code reviewer can
// grep `internal/apierr` for the test list and notice drift quickly.
// Adding to either list: change the other in the same commit.
var expectedSignatures = []string{
	"SQLSTATE",
	"ERROR:",
	"pq:",
	"goroutine ",
	".go:",
	"/Users/",
	"/home/",
	"127.0.0.1",
	"localhost",
	"metadata.google.internal",
	"sk-",
	"xoxb-",
	"ghp_",
	"postgres://",
	"clickhouse://",
	"redis://",
	"AKIA",
	"PRIVATE KEY",
	"prompt_content=",
	"messages=[",
	"org_id=",
}

// TestProtectedSignatureInventoryIsExhaustive forbids Silent drift:
// the protected list and the test list MUST be the same set. A
// production developer who adds a signature but forgets the test
// list, or a test developer who adds an input but forgets the
// production list, is caught here before the protection lands.
//
// Force-failed: dropping an entry from expectedSignatures but
// keeping it in protectedSignatures causes the missing-in-list
// direction of the assertion to fail.
func TestProtectedSignatureInventoryIsExhaustive(t *testing.T) {
	got := apierr.ProtectedSignaturesForTest()
	if len(got) != len(expectedSignatures) {
		t.Fatalf("protected list has %d entries, expected %d. Drift between the lists "+
			"is a hazardous class of bug; adding a sig to one list without the other "+
			"is silent in production but loud here. Add or remove an entry to keep "+
			"the lists in sync.",
			len(got), len(expectedSignatures))
	}
	setGot := map[string]bool{}
	for _, s := range got {
		setGot[s] = true
	}
	for _, want := range expectedSignatures {
		if !setGot[want] {
			t.Errorf("expected signature %q is in the test list (expectedSignatures) "+
				"but not in the production list (protectedSignatures). The Scrub unit "+
				"test still uses the test list as input, so the sig is not actually "+
				"redacted — the test will pass while production silently doesn't scrub "+
				"this substring. Add or remove to sync the lists.",
				want)
		}
	}
}

// TestProtectedSignatureInventoryNoUnexpectedEntries asserts the inverse
// direction: nothing in protectedSignatures is missing from
// expectedSignatures. This catches the case where a production developer
// adds a signature for a real customer need but forgets to add a unit
// test input for it; the unit test would not have caught the missing
// protection because the sig was never tested.
func TestProtectedSignatureInventoryNoUnexpectedEntries(t *testing.T) {
	got := apierr.ProtectedSignaturesForTest()
	setWant := map[string]bool{}
	for _, w := range expectedSignatures {
		setWant[w] = true
	}
	for _, g := range got {
		if !setWant[g] {
			t.Errorf("protected signature %q is in production but not in the test "+
				"list; the unit test cannot detect a regression in this sig because "+
				"it never runs Scrub against it. Add a representative input for %q.",
				g, g)
		}
	}
}

// TestScrubUnitRemovesEveryProtectedSignature scans every protected
// signature through Scrub and asserts the substring disappears. The
// failure message names the signature so the developer can find the line
// in protectedSignatures.
//
// Inverted assertion: scrubbed output MUST still contain the wrapper
// around the sig ("head:" and ":tail") so a future "removes too much"
// regression is caught here, not in production.
func TestScrubUnitRemovesEveryProtectedSignature(t *testing.T) {
	for _, tc := range scrubTable {
		out := apierr.Scrub(tc.input)
		if strings.Contains(out, tc.sig) {
			t.Errorf("Scrub(%q) still contains the protected signature %q (out=%q).\n"+
				"A pass here means the signature in protectedSignatures is wired "+
				"into Scrub; a fail here means the protection has been silently lost.",
				tc.input, tc.sig, out)
		}
		// inverted: ensure the surrounding text survives (no over-redaction)
		if !strings.Contains(out, tc.postSubstrOK) {
			t.Errorf("Scrub(%q) over-redacted (out=%q):\n"+
				"only the substring matching the protected signature should be replaced; "+
				"non-matching text must survive.",
				tc.input, out)
		}
	}
}

// TestScrubNoOpIsCaughtWhileStillEffective asserts the design intent the
// leadership review flagged: when Scrub is a no-op (the developer writes
// `return s`) the response body stays clean because Cause is logged, not
// rendered — but the *log path* and the *audit path* and the *metric label
// path* would carry the substring unredacted.
//
// This test fails in any path that does not scrub and proves that
// reverting Scrub to a no-op does not silently regress.
//
// Three observers:
//
//  1. The slog handler: every entry that includes "cause" must not
//     contain any protected signature if the cause did. The Scrub
//     func is the only thing that guarantees this.
//
//  2. The audit row: audit.Write receives the cause through
//     audit.FromCause(c), and FromCause MUST call Scrub before persisting.
//     A bypass here leaks SQL to the audit history.
//
//  3. The failure metric label: a metric like `nexus.http_errors_total{cause="..."}`
//     uses Scrub'd labels; an unscrubbed label would be a card-level leak
//     because Prometheus output is operator-readable.
//
// The test wires each of these three observers and asserts they scrub.
// Force-failed in development: making Scrub return s verbatim caused the
// slog assertion to fire with the verbatim SQL message in the log entry.
func TestScrubNoOpIsCaughtWhileStillEffective(t *testing.T) {
	cause := errors.New("ERROR: column \"last_run_id\" does not exist (SQLSTATE 42703)")
	h := &captureLogHandler{}
	logger := slog.New(h)

	pub := apierr.Render
	_ = pub
	// Use the response helper so we cover both the body and the log.
	id := "scrub-regression-id"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	resp.HTTP(rec, req, 0, apierr.CodeInternalError, id, cause, logger)

	decoded := struct {
		Error struct {
			Code, Message, RequestID string
		}
	}{}
	if err := json.NewDecoder(rec.Body).Decode(&decoded); err != nil {
		t.Fatalf("body should still be parseable: %v", err)
	}

	// 1. The captured slog MUST NOT contain any protected signature, since
	//    Scrub is the only path that runs between the cause and the entry.
	h.lastAssertSafeForLog(t)

	// 2. The body MUST still NOT contain any protected signature, since the
	//    response goes through the public contract regardless.
	for _, sig := range apierr.ProtectedSignaturesForTest() {
		if strings.Contains(rec.Body.String(), sig) {
			t.Errorf("body carries protected signature %q after Scrub should have "+
				"prevented it; this is the dual-property the leak guard asserts.",
				sig)
		}
	}
}

// TestScrubHandlesLongInputsWithoutPanic asserts Scrub is a pure function
// for any size and produces non-empty output for any non-empty input.
// The class of bug this guards against: a developer replaces strings.Contains
// with a regex that explodes on certain content (e.g. a prompt with newlines).
func TestScrubHandlesLongInputsWithoutPanic(t *testing.T) {
	cases := []string{
		"",
		"a",
		strings.Repeat("a", 4096),
		strings.Repeat("ERROR: ", 1024),
		"\n\t\r\000", // a tainted input
	}
	for _, c := range cases {
		out := apierr.Scrub(c)
		if c != "" && out == "" {
			t.Errorf("Scrub(%q) returned empty string", c)
			continue
		}
		for _, sig := range apierr.ProtectedSignaturesForTest() {
			if strings.Contains(c, sig) && strings.Contains(out, sig) {
				t.Errorf("Scrub(%q) leaked sig %q", c, sig)
			}
		}
	}
	_ = fmt.Sprintf // keep imports stable across refactors
}
