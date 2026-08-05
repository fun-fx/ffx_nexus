package console

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ffxnexus/nexus/internal/core"
)

// Environment-push reports.
//
// Publishing a benchmark environment to the provider is a local-CLI
// operation: `prime env push` reads a dataset and a grader off the
// operator's own disk and uploads them. There is no server-side API for
// it, so Nexus cannot perform the push and cannot observe that it
// happened either.
//
// This endpoint closes that blind spot without taking on the push
// itself. The operator's last CLI step is a curl back to here, which
// lets the console say "you pushed this slug at 13:05" instead of
// showing nothing until someone re-runs Validate.
//
// A report is advisory, never authoritative. Anyone holding an admin
// token can post ok=true for a slug that was never published, so the
// only thing that decides whether a slug is really visible is the
// dry-run in dryRunBenchmark, which asks the vendor directly. The
// console must present these as "reported", not as "verified".
//
// The report deliberately carries no CLI output. `prime` echoes request
// details on failure and the API key can ride along in them, so
// accepting stdout would mean a credential could land in memory and
// then get rendered into an admin page. The operator reads their own
// failure text in their own terminal instead.

// pushReportTTL bounds how long a report stays interesting. A push is
// a setup step measured in minutes, so a day is already generous; past
// that the entry is noise and the operator should trust Validate.
const pushReportTTL = 24 * time.Hour

// maxPushReports caps the map so a scripted loop cannot grow it without
// bound. The number is far above the handful of environments a real
// deployment publishes; hitting it means something is wrong, and
// dropping the oldest entry is the least surprising response.
const maxPushReports = 256

// PushReport is one operator-reported `prime env push` outcome.
type PushReport struct {
	Slug        string    `json:"slug"`
	OK          bool      `json:"ok"`
	CompletedAt time.Time `json:"completed_at"`
	// ReceivedAt is stamped by the server. It is the field the console
	// renders, because CompletedAt comes from the reporter's clock and
	// may be skewed or absent.
	ReceivedAt time.Time `json:"received_at"`
}

// pushReportStore keeps the reports in process memory, keyed by slug so
// a re-push overwrites rather than accumulates.
//
// Nothing here is persisted. The data is a UI convenience with a
// same-day shelf life, and losing it on restart costs the operator one
// click on Validate, so a table and a migration would buy nothing.
// The zero value is ready to use.
type pushReportStore struct {
	mu sync.Mutex
	m  map[string]PushReport
}

// put records a report, evicting expired entries on the way. Pruning on
// write (rather than from a goroutine) keeps the store free of any
// lifecycle to wire up or shut down.
func (p *pushReportStore) put(rep PushReport, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = make(map[string]PushReport)
	}
	p.pruneLocked(now)
	if len(p.m) >= maxPushReports {
		if _, replacing := p.m[rep.Slug]; !replacing {
			p.dropOldestLocked()
		}
	}
	p.m[rep.Slug] = rep
}

// list returns the live reports, newest first.
func (p *pushReportStore) list(now time.Time) []PushReport {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneLocked(now)
	out := make([]PushReport, 0, len(p.m))
	for _, rep := range p.m {
		out = append(out, rep)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ReceivedAt.After(out[j].ReceivedAt)
	})
	return out
}

func (p *pushReportStore) pruneLocked(now time.Time) {
	for slug, rep := range p.m {
		if now.Sub(rep.ReceivedAt) > pushReportTTL {
			delete(p.m, slug)
		}
	}
}

func (p *pushReportStore) dropOldestLocked() {
	var oldestSlug string
	var oldest time.Time
	for slug, rep := range p.m {
		if oldestSlug == "" || rep.ReceivedAt.Before(oldest) {
			oldestSlug, oldest = slug, rep.ReceivedAt
		}
	}
	if oldestSlug != "" {
		delete(p.m, oldestSlug)
	}
}

// reportEnvPush records that the operator ran `prime env push` for a
// slug. It exists so the console can show the push happened; it does
// not and must not change how visibility is decided.
func (s *Server) reportEnvPush(w http.ResponseWriter, r *http.Request, _ core.User) {
	var body struct {
		Slug        string `json:"slug"`
		OK          *bool  `json:"ok"`
		CompletedAt string `json:"completed_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	slug := strings.TrimSpace(body.Slug)
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "slug is required"})
		return
	}
	// `ok` decides whether the console reads this as a success, so a
	// missing field must not silently mean failure.
	if body.OK == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ok is required"})
		return
	}

	now := time.Now().UTC()
	// The reporter's timestamp is optional and untrusted; an unparseable
	// or absent value falls back to arrival time rather than rejecting
	// an otherwise valid report.
	completed := now
	if body.CompletedAt != "" {
		if t, err := time.Parse(time.RFC3339, body.CompletedAt); err == nil {
			completed = t.UTC()
		}
	}

	s.pushReports.put(PushReport{
		Slug:        slug,
		OK:          *body.OK,
		CompletedAt: completed,
		ReceivedAt:  now,
	}, now)

	writeJSON(w, http.StatusOK, map[string]any{"recorded": true})
}

// listEnvPushReports serves the reports the console renders next to the
// environment chips.
func (s *Server) listEnvPushReports(w http.ResponseWriter, r *http.Request, _ core.User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"reports": s.pushReports.list(time.Now().UTC()),
	})
}
