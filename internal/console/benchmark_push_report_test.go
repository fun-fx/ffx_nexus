package console

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/core"
)

// pushServer is a console with no benchmark runner wired. The
// push-report routes are deliberately independent of the provider:
// they record what the operator did on their own machine, so they must
// keep working on a deployment where the runner is unconfigured.
func pushServer() *Server { return NewServer(nil, nil, nil, slog.Default()) }

func TestReportEnvPushRecordsAndLists(t *testing.T) {
	s := pushServer()

	rec := call(s.reportEnvPush, http.MethodPost, "/api/eval/benchmarks/push-report",
		`{"slug":"acme/gsm8k","ok":true,"completed_at":"2026-08-05T04:00:00Z"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("post: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := decode(t, rec)["recorded"]; got != true {
		t.Fatalf("recorded: want true, got %v", got)
	}

	rec = call(s.listEnvPushReports, http.MethodGet, "/api/eval/benchmarks/push-report", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: want 200, got %d", rec.Code)
	}
	reports, ok := decode(t, rec)["reports"].([]any)
	if !ok || len(reports) != 1 {
		t.Fatalf("want 1 report, got %v", decode(t, rec)["reports"])
	}
	got := reports[0].(map[string]any)
	if got["slug"] != "acme/gsm8k" {
		t.Errorf("slug: got %v", got["slug"])
	}
	if got["ok"] != true {
		t.Errorf("ok: got %v", got["ok"])
	}
	// received_at is the server's own stamp, so it must be present even
	// though the request only supplied completed_at.
	if got["received_at"] == "" || got["received_at"] == nil {
		t.Errorf("received_at should be stamped by the server, got %v", got["received_at"])
	}
}

func TestReportEnvPushAcceptsFailure(t *testing.T) {
	// A failed push is the more useful report of the two: it tells the
	// operator why Validate is still going to 404.
	s := pushServer()
	rec := call(s.reportEnvPush, http.MethodPost, "/api/eval/benchmarks/push-report",
		`{"slug":"acme/gsm8k","ok":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = call(s.listEnvPushReports, http.MethodGet, "/api/eval/benchmarks/push-report", "")
	got := decode(t, rec)["reports"].([]any)[0].(map[string]any)
	if got["ok"] != false {
		t.Errorf("ok: want false, got %v", got["ok"])
	}
}

func TestReportEnvPushRejectsBadInput(t *testing.T) {
	// `ok` has no safe default: guessing either way would make the
	// console claim something the operator never said.
	for name, body := range map[string]string{
		"not JSON":     `not-json`,
		"missing slug": `{"ok":true}`,
		"blank slug":   `{"slug":"   ","ok":true}`,
		"missing ok":   `{"slug":"acme/gsm8k"}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := call(pushServer().reportEnvPush, http.MethodPost,
				"/api/eval/benchmarks/push-report", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestReportEnvPushIgnoresUnparseableTimestamp(t *testing.T) {
	// The timestamp is a convenience from an untrusted clock. A bad one
	// falls back to arrival time; it must not reject a report that is
	// otherwise fine.
	s := pushServer()
	rec := call(s.reportEnvPush, http.MethodPost, "/api/eval/benchmarks/push-report",
		`{"slug":"acme/gsm8k","ok":true,"completed_at":"yesterday"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := decode(t, call(s.listEnvPushReports, http.MethodGet,
		"/api/eval/benchmarks/push-report", ""))["reports"].([]any)[0].(map[string]any)
	if got["completed_at"] == nil || got["completed_at"] == "" {
		t.Errorf("completed_at should fall back to arrival time, got %v", got["completed_at"])
	}
}

func TestReportEnvPushRepushReplaces(t *testing.T) {
	// Keyed by slug: pushing the same environment again is a correction,
	// not a second event, so the operator sees one current answer.
	s := pushServer()
	for _, body := range []string{
		`{"slug":"acme/gsm8k","ok":false}`,
		`{"slug":"acme/gsm8k","ok":true}`,
	} {
		if rec := call(s.reportEnvPush, http.MethodPost,
			"/api/eval/benchmarks/push-report", body); rec.Code != http.StatusOK {
			t.Fatalf("post %s: got %d", body, rec.Code)
		}
	}
	reports := decode(t, call(s.listEnvPushReports, http.MethodGet,
		"/api/eval/benchmarks/push-report", ""))["reports"].([]any)
	if len(reports) != 1 {
		t.Fatalf("want 1 report after re-push, got %d", len(reports))
	}
	if got := reports[0].(map[string]any)["ok"]; got != true {
		t.Errorf("the later report should win, got ok=%v", got)
	}
}

func TestPushReportStoreExpires(t *testing.T) {
	var st pushReportStore
	base := time.Date(2026, 8, 5, 4, 0, 0, 0, time.UTC)
	st.put(PushReport{Slug: "acme/old", OK: true, ReceivedAt: base}, base)

	// Just inside the window the entry is still useful.
	if got := st.list(base.Add(pushReportTTL - time.Minute)); len(got) != 1 {
		t.Fatalf("inside TTL: want 1 report, got %d", len(got))
	}
	// Past it the operator should be re-checking with Validate instead.
	if got := st.list(base.Add(pushReportTTL + time.Minute)); len(got) != 0 {
		t.Fatalf("past TTL: want 0 reports, got %d", len(got))
	}
}

func TestPushReportStoreCapsSize(t *testing.T) {
	// A scripted caller must not be able to grow the map without bound.
	var st pushReportStore
	now := time.Date(2026, 8, 5, 4, 0, 0, 0, time.UTC)
	for i := 0; i < maxPushReports+50; i++ {
		st.put(PushReport{
			Slug: fmt.Sprintf("acme/env-%d", i),
			OK:   true,
			// Stagger arrival so eviction has an unambiguous oldest.
			ReceivedAt: now.Add(time.Duration(i) * time.Second),
		}, now.Add(time.Duration(i)*time.Second))
	}
	got := st.list(now.Add(time.Duration(maxPushReports+50) * time.Second))
	if len(got) > maxPushReports {
		t.Fatalf("store grew past the cap: %d > %d", len(got), maxPushReports)
	}
	// Eviction drops the oldest, so the most recent slug must survive.
	newest := fmt.Sprintf("acme/env-%d", maxPushReports+49)
	if got[0].Slug != newest {
		t.Errorf("newest report should be first, want %s got %s", newest, got[0].Slug)
	}
}

func TestPushReportPathIsNotReadAsRunID(t *testing.T) {
	// "push-report" sits next to "/{id}" in the router, so a GET could
	// plausibly be captured as a run lookup. It has to reach its own
	// handler: a run lookup would 404 (or worse, return a run) instead
	// of the reports list the console asks for.
	s := pushServer()
	s.SetBenchmarks(&fakeRunner{run: core.BenchmarkRun{ID: "should-not-be-used"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/eval/benchmarks/push-report", nil)
	// The store is empty, so a correct route answers with an empty list
	// rather than a run body.
	s.listEnvPushReports(rec, req, admin())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := decode(t, rec)["reports"]; !ok {
		t.Fatalf("want a reports key, got %s", rec.Body.String())
	}
}

func TestPushReportWorksWithoutBenchmarkRunner(t *testing.T) {
	// A deployment with no control-plane database still has an operator
	// who can publish environments from their laptop. The other
	// benchmark routes answer 503 there; these must not, or the report
	// has nowhere to land.
	rec := call(pushServer().reportEnvPush, http.MethodPost,
		"/api/eval/benchmarks/push-report", `{"slug":"acme/gsm8k","ok":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 without a runner, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPushReportsSortedNewestFirst(t *testing.T) {
	var st pushReportStore
	now := time.Date(2026, 8, 5, 4, 0, 0, 0, time.UTC)
	st.put(PushReport{Slug: "acme/first", ReceivedAt: now}, now)
	st.put(PushReport{Slug: "acme/second", ReceivedAt: now.Add(time.Minute)}, now.Add(time.Minute))

	got := st.list(now.Add(2 * time.Minute))
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
	if got[0].Slug != "acme/second" {
		t.Errorf("want newest first, got %s", got[0].Slug)
	}
}
