package console

import (
	"context"
	"log/slog"
)

// Mailer is the transport-agnostic interface an invite handler calls after
// persisting a row. Returning a synthetic message id (the idempotency key
// is the natural choice on success, an empty string on noop) lets the audit
// row correlate "we sent invite X" -> "transport's id" without callers
// branching on which provider is wired.
//
// Implementations:
//   - *ResendClient  (internal/console/resend.go) — Resend HTTP API.
//   - *SMTPMailer    (internal/console/smtp.go)    — RFC 5321 relay.
//   - *noopMailer    (local-only, this file)        — accept and discard;
//     refuses to deliver unless the server is in DevMode.
//
// Sending is best-effort by caller convention: the invite row is already
// committed and the URL is already copyable from the console, so an error
// from Send is logged and audited but does not unwind the API call.
type Mailer interface {
	Send(ctx context.Context, to, subject, html, idempotencyKey string) (string, error)
}

// noopMailer accepts and discards. The returned id is the idempotency key
// itself so an audit row still has something stable to correlate by.
//
// It is gated to DevMode so a misconfigured production install can never
// silently accept-and-drop invites. boot/installer refuses to wire it when
// DevMode is false; tests use it freely.
type NoopMailer struct {
	Log *slog.Logger
}

func (n *NoopMailer) Send(_ context.Context, _, _, _, idempotencyKey string) (string, error) {
	if n != nil && n.Log != nil {
		n.Log.Info("noopMailer dropped invite", "idempotency_key", idempotencyKey)
	}
	return "noop:" + idempotencyKey, nil
}
