// Package evals runs asynchronous, out-of-band quality evaluations on gateway
// traces. It never sits on the request hot path: the gateway hands completed
// traces to a Worker (via the observability.Recorder interface), and evaluation
// runs on background goroutines. Results land in ClickHouse (eval_scores) and
// feed quality-aware routing (Phase 4).
package evals

import (
	"context"
	"strings"
	"time"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/observability"
)

// Score is a single evaluation result for one trace, mirroring the eval_scores
// table schema.
type Score struct {
	TraceID string
	// OrgID is the organization the scored trace belongs to. Reads filter on it,
	// so a score written without one is only visible to the default org.
	//
	// It is carried on the Score rather than resolved at write time because the
	// sink writes in batches, long after the trace was served: looking the org up
	// then would mean a query per row against a user record that may since have
	// moved orgs, and would attribute the score to where the user is now instead
	// of where the traffic was.
	OrgID        string
	UserID       string // owning user (BYOK); empty for org-level/legacy traces
	RequestModel string // model that served the trace; used for PG routing stats
	Timestamp    time.Time
	Evaluator    string  // e.g. "heuristic_pii", "slm_judge"
	Metric       string  // e.g. "pii_leak", "completeness", "quality"
	Score        float64 // normalized 0..1 (higher is better)
	Passed       bool
	Rationale    string
	JudgeModel   string // model used for LLM-as-judge, empty for heuristics
}

// UnattributedOrgID is the org written when a score's true owner cannot be
// established. No tenant's reads match it (see observability.orgScopeClause),
// so such a row is retained and auditable but visible to nobody.
//
// It exists for exactly one path: an external vendor pushes a score for a
// trace_id Nexus cannot resolve, through a plugin installed cluster-wide. The
// alternatives were both worse. Dropping the row loses a paid-for evaluation
// with no record. Defaulting it to core.DefaultOrgID guesses, and in a
// multi-department installation the guess shows one department's vendor scores
// to whichever department happens to hold the default org.
//
// Operators reclaim these rows deliberately — see
// docs/customer-self-hosted-integrations.md — after deciding which org they
// belong to. Installing the plugin per-org rather than cluster-wide avoids the
// situation entirely, because then the plugin itself carries the tenant.
const UnattributedOrgID = "unattributed"

// orgOrDefault normalises an empty org to the default one.
//
// Writing an empty string would put the row outside every org's reads, including
// the default org's, so the score would be silently invisible rather than
// merely mis-attributed. A score nobody can see is worse than one attributed to
// the single-org default, which is where an installation that never configured
// multiple orgs expects its data to be.
//
// Callers that know attribution failed must pass UnattributedOrgID explicitly
// rather than relying on this fallback; the fallback is for the in-process
// worker path, where an empty org means the trace itself predates attribution
// and the default org is its rightful owner.
func orgOrDefault(orgID string) string {
	if strings.TrimSpace(orgID) == "" {
		return core.DefaultOrgID
	}
	return orgID
}

// Evaluator scores a single trace. Implementations must be safe for concurrent
// use and must respect ctx cancellation/timeout.
type Evaluator interface {
	// Name identifies the evaluator (stored as eval_scores.evaluator).
	Name() string
	// Evaluate returns zero or more scores for the trace.
	Evaluate(ctx context.Context, t observability.Trace) ([]Score, error)
}

// Sink persists evaluation scores. Implementations should batch/flush
// internally and must be safe for concurrent use.
type Sink interface {
	WriteScores(ctx context.Context, scores []Score) error
}

// NoopSink discards scores. Used when no score store backend is configured.
type NoopSink struct{}

// WriteScores implements Sink.
func (NoopSink) WriteScores(context.Context, []Score) error { return nil }
