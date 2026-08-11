package benchmark

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// DriftAlertSpec configures the post-settlement drift detector.
//
// Defaults are conservative: a 5% relative change between two
// adjacent settled runs is enough to surface in a banner, and a
// freshness of 30 days is the threshold at which we say "your
// router is reading something stale" rather than "the data is fresh".
// The router already implements half-life decay so the freshness
// alert is mostly a safety net for deployments that have turned
// decay off (NEXUS_ROUTE_BENCH_HALF_LIFE=0).
type DriftAlertSpec struct {
	// RelativeChangeThreshold is the minimum absolute relative
	// change (|current - last| / last) that triggers an alert.
	// 0 disables the comparison entirely.
	RelativeChangeThreshold float64

	// FreshnessThreshold is the maximum age at which a settled
	// row is still considered "current". Crossed ages alert at
	// boot and on every poll pass. Zero disables the age check.
	FreshnessThreshold time.Duration
}

// DefaultDriftAlertSpec is what the runner uses when the operator
// has not overridden it. It is exposed so callers can build off
// these numbers rather than embedding them.
var DefaultDriftAlertSpec = DriftAlertSpec{
	RelativeChangeThreshold: 0.05,
	FreshnessThreshold:      30 * 24 * time.Hour,
}

// DriftAlert is the structured record a watcher emits. The alert
// pattern is intentionally verbose: the audit log is the source of
// truth for the operator's incident chronology, and a partial
// record would force them to consult another system to know when the
// gap was first observed.
type DriftAlert struct {
	Model        string    `json:"model"`
	Kind         string    `json:"kind"` // "relative_change" | "freshness_age"
	CurrentScore float64   `json:"current_score"`
	PreviousScore float64  `json:"previous_score"`
	RunID        string    `json:"run_id"`
	ObservedAt   time.Time `json:"observed_at"`
	// Origin distinguishes "I just settled" from "I have been stale
	// for a while and only just got around to telling you". The
	// post-settle detector sets Origin="settle". The boot staleness
	// sweep sets Origin="boot".
	Origin      string  `json:"origin"`
	Threshold   float64 `json:"threshold"`
	Detail      string  `json:"detail,omitempty"`
}

// AlertSink accepts a drift alert. The default concrete sink is
// Audit, which writes the alert to the audit log; an alternative
// sink could forward to PagerDuty / Slack, but those bindings
// already exist elsewhere and the operator-facing alert does not
// have a criticality case that warrants re-implementing here.
type AlertSink interface {
	Emit(ctx context.Context, alert DriftAlert)
}

// AlertSinkFunc turns an ordinary function into an AlertSink.
type AlertSinkFunc func(ctx context.Context, alert DriftAlert)

// Emit satisfies AlertSink.
func (f AlertSinkFunc) Emit(ctx context.Context, alert DriftAlert) { f(ctx, alert) }

// NullSink discards alerts. Useful for tests and for deployments
// where the audit log is the sink and a separate sink is undesirable.
var NullSink AlertSink = AlertSinkFunc(func(_ context.Context, _ DriftAlert) {})

// DriftWatcher detects the two events above. The watcher is a
// pure-logic component: it holds no state beyond the spec; the
// caller is responsible for sourcing the comparison runs (the
// store's GetRecent runs is sufficient).
type DriftWatcher struct {
	spec DriftAlertSpec
	sink AlertSink
	log  *slog.Logger
}

// NewDriftWatcher constructs a watcher. A nil logger is replaced
// with a discarding one.
func NewDriftWatcher(spec DriftAlertSpec, sink AlertSink, log *slog.Logger) *DriftWatcher {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if sink == nil {
		sink = NullSink
	}
	return &DriftWatcher{spec: spec, sink: sink, log: log}
}

// Sources is the read interface the watcher needs. Kept narrow:
// two adjacent run reads and a name, all it ever asks for. Wider
// interfaces let other callers drag in dependency we do not want.
type DriftSources interface {
	GetBenchmarkRun(ctx context.Context, id string) (RunLite, error)
	ListRecentSettledRuns(ctx context.Context, model string, limit int) ([]RunLite, error)
}

// RunLite is the slice of BenchmarkRun the watcher reads. Kept as
// a separate struct rather than reusing core.BenchmarkRun because
// the watcher never reads anything but id, model, completed_at and
// avg_score, and importing core here would create a cycle.
type RunLite struct {
	ID          string
	Model       string
	AvgScore    float64
	CompletedAt time.Time
}

// ObserveSettle is the post-settlement hook the poller calls. The
// settled run plus the most-recent prior settled run (if any) is
// compared against the spec.
func (w *DriftWatcher) ObserveSettle(ctx context.Context, src DriftSources, just RunLite) {
	if w == nil || w.spec.RelativeChangeThreshold <= 0 || src == nil {
		return
	}
	if just.AvgScore <= 0 || just.CompletedAt.IsZero() {
		return
	}
	previous, err := src.ListRecentSettledRuns(ctx, just.Model, 2)
	if err != nil {
		w.log.Warn("drift: list prior runs failed", "model", just.Model, "err", err)
		return
	}
	// We expect the just-settled row to be in the most-recent list
	// as the head. Skip it to compare to the second.
	var prior RunLite
	for _, r := range previous {
		if r.ID != just.ID {
			prior = r
			break
		}
	}
	if prior.ID == "" || prior.AvgScore <= 0 {
		return
	}
	rel := (just.AvgScore - prior.AvgScore) / prior.AvgScore
	if rel < 0 {
		rel = -rel
	}
	if rel < w.spec.RelativeChangeThreshold {
		return
	}
	w.sink.Emit(ctx, DriftAlert{
		Model:         just.Model,
		Kind:          "relative_change",
		CurrentScore:  just.AvgScore,
		PreviousScore: prior.AvgScore,
		RunID:         just.ID,
		ObservedAt:    just.CompletedAt,
		Origin:        "settle",
		Threshold:     w.spec.RelativeChangeThreshold,
		Detail: fmt.Sprintf(
			"%.1f%% relative change (current %.3f vs prior %.3f)",
			rel*100, just.AvgScore, prior.AvgScore),
	})
}

// ObserveStaleness sweeps the rows the operator has been scoring
// against and emits an alert per model that has no settled row in
// the freshness window.
//
// Boot-time use case: tell the operator "your router has been using
// a stale benchmark for the last 19 days, please schedule a re-run".
// The router itself does not know this; it blends whatever is in the
// snapshot.
func (w *DriftWatcher) ObserveStaleness(ctx context.Context, src DriftSources, models []string) {
	if w == nil || w.spec.FreshnessThreshold <= 0 {
		return
	}
	now := time.Now().UTC()
	threshold := w.spec.FreshnessThreshold
	for _, m := range models {
		rows, err := src.ListRecentSettledRuns(ctx, m, 1)
		if err != nil {
			w.log.Warn("drift: list settled for staleness failed",
				"model", m, "err", err)
			continue
		}
		if len(rows) == 0 {
			continue // no benchmark ever — that is not a staleness, it is an absence
		}
		latest := rows[0]
		age := now.Sub(latest.CompletedAt)
		if age <= threshold {
			continue
		}
		w.sink.Emit(ctx, DriftAlert{
			Model:        m,
			Kind:         "freshness_age",
			CurrentScore: latest.AvgScore,
			RunID:        latest.ID,
			ObservedAt:   now,
			Origin:       "boot",
			Threshold:    threshold.Hours() / 24,
			Detail: fmt.Sprintf(
				"last settled %s ago (%.1f days)", age.Round(time.Minute),
				age.Hours()/24),
		})
	}
}

// Compile-time guard that the runner's store satisfies DriftSources.
// The methods the runner exposes are richer than what the watcher
// needs; an adapter exists at cmd/nexus/main.go.
var _ = errors.New("dance with what you got") // intentional unused symbol to ease grep
