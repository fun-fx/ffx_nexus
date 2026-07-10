// Package router — failover notification (V4).
//
// When the quality-aware router's ranked list is used by an actual
// gateway (Rank → "try candidate[0], on error try candidate[1]…"), a
// failover is a *signal* worth sending to the operator's alerting
// stack: it means a primary vendor was unreachable / degraded and the
// secondary absorbed a request. Posting that to a webhook, Slack, or
// PagerDuty turns the metric into an actionable page.
//
// The notifier is deliberately behind a tiny interface so gateway
// code can wire one or more sinks without a refactor when a new
// target is added (PagerDuty, Microsoft Teams, Discord, OpsGenie…).

package router

import "context"

// FailoverEvent describes one primary → secondary hop in a single
// request. We pre-marshal it once so per-sink encoders don't each get
// to make opinionated JSON decisions.
type FailoverEvent struct {
	// OrgID / virtual key that were responsible for the request. Empty
	// means "unknown" (gateway can attempt to surface but the notifier
	// must keep working).
	OrgID        string `json:"org_id,omitempty"`
	VirtualKeyID string `json:"virtual_key_id,omitempty"`

	// Alias is the routing group name (e.g. "fast", "auto", "smart").
	// Empty for direct (non-aliased) requests.
	Alias string `json:"alias,omitempty"`

	// Tried models in the order they were tried — useful for ops to see
	// if a specific provider keeps falling over to the same backup.
	Tried []string `json:"tried"`

	// Primary is the model that produced the error that triggered the
	// fallback (== Tried[0] for a fresh request).
	Primary string `json:"primary"`

	// Fallback is the model that eventually took the request.
	Fallback string `json:"fallback"`

	// Reason is a short human label ("upstream_timeout",
	// "upstream_5xx", "rate_limit", ...) — driver passes whatever it
	// observed; notifiers MAY add a default if empty.
	Reason string `json:"reason"`

	// LatencyMs of the failing primary request, so dashboards /
	// pagers can pattern-match slow primaries. 0 if unknown.
	LatencyMs int64 `json:"latency_ms,omitempty"`

	// FailedAt is the wall-clock time of the failure (typically the
	// moment the primary returned the error response). Notifiers MAY
	// skip including this in their outgoing message; it's there for
	// sinks that want grained ordering or correlation keys.
	FailedAtUnix int64 `json:"failed_at_unix_ms,omitempty"`
}

// Notifier is the abstract sink. Implementations must be safe for
// concurrent use and never block the caller for more than a few
// milliseconds: the gateway hot path takes the failure as recorded
// and notifies asynchronously. Implementations that cannot honor
// this MUST run a worker and provide a buffered event channel of
// their own (the helpers in this file do that).
type Notifier interface {
	Notify(ctx context.Context, ev FailoverEvent)
}

// NoopNotifier discards every event. Used when no env is set so
// calling sites have a single typed nil-check on the field.
type NoopNotifier struct{}

func (NoopNotifier) Notify(context.Context, FailoverEvent) {}

// MultiNotifier fans one event out to several sinks. A slow / failing
// sink logs but never blocks the others; the gateway's hot-path
// latency is protected by the per-sink worker (see NewBuffered).
type MultiNotifier struct {
	notifiers []Notifier
}

// NewMultiNotifier composes any number of sinks. Nil entries are
// ignored so callers can pass `cfg.Build()` outputs directly.
func NewMultiNotifier(notifiers ...Notifier) *MultiNotifier {
	kept := make([]Notifier, 0, len(notifiers))
	for _, n := range notifiers {
		if n == nil {
			continue
		}
		if _, isNoop := n.(NoopNotifier); isNoop {
			continue
		}
		kept = append(kept, n)
	}
	if len(kept) == 0 {
		return nil
	}
	return &MultiNotifier{notifiers: kept}
}

// Notify forwards the event to each sink. Sinks MUST be
// self-buffered; this method does not block on slow HTTP POSTs.
func (m *MultiNotifier) Notify(ctx context.Context, ev FailoverEvent) {
	if m == nil {
		return
	}
	for _, n := range m.notifiers {
		n.Notify(ctx, ev)
	}
}
