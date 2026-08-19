package benchmark

import (
	"context"
	"encoding/json"

	"github.com/ffxnexus/nexus/internal/core"
)

// AuditSink writes drift alerts into the audit_log table. It uses
// Store.Audit so the audit table's own storage (Postgres) is the
// single source of truth for "why was a benchmark row recorded the
// way it was?" — drift alerts and operator actions share the same
// chrono-
//
// The "as system" actor is chosen because the alert is emitted by
// the runner, not a user click. Returning a row from audit_log
// already means the admin can correlate; a system-actor entry reads
// exactly like "background detection" should.
type AuditSink struct {
	Store *core.Store
}

// NewAuditSink is a small helper so the call site reads naturally:
// the constructor is silent on a nil store because the watcher
// accepts any AlertSink; this struct just makes the wiring obvious.
func NewAuditSink(s *core.Store) *AuditSink { return &AuditSink{Store: s} }

// Emit writes the alert as a structured audit row. We JSON-encode
// the alert fields into the detail column so the audit timeline
// reads exactly the alert object. The action column has the bare
// kind so a SQL filter slices the timeline by alert type without
// parsing payloads.
func (a *AuditSink) Emit(ctx context.Context, alert DriftAlert) {
	if a == nil || a.Store == nil {
		return
	}
	raw, err := json.Marshal(alert)
	if err != nil {
		raw = []byte(`{"kind":"` + alert.Kind + `"}`)
	}
	a.Store.Audit(ctx, core.AuditEvent{
		ActorID:  "system",
		OrgID:    "",
		Action:   core.AuditActionBenchmarkScheduleHit,
		TargetID: alert.RunID,
		Detail:   string(raw),
	})
}
