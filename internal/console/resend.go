package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ffxnexus/nexus/internal/egress"
)

// ResendClient is a thin caller around the Resend REST API.
//
// We do not import resend-go to keep the dependency surface
// tight: the only endpoint we exercise today is
// https://api.resend.com/emails (POST), and its contract is small
// enough (Authorization: Bearer, JSON body, 200 + { "id" })
// that a stdlib net/http call is the whole integration.
//
// Idempotency: every send attaches an `Idempotency-Key` header
// derived from the invite id. Resend deduplicates within 24
// hours so the retry path on transient failures is safe.
//
// Failure modes:
//   - 5xx upstream → return a wrapped sentinel; the caller
//     decides whether to retry, surface a 502 to the admin, or
//     quietly fall back to "URL-only delivery".
//   - 4xx upstream (validation, auth) → bubbles up so the admin
//     sees a real reason instead of a silent drop.
//   - network / DNS → wrapped net.OpError style error.
//
// The client is safe for concurrent use — *http.Client is.
type ResendClient struct {
	APIKey   string
	From     string // e.g. "Nexus <noreply@ffx.ai>"
	Endpoint string // overridable so tests can stub localhost
	Timeout  time.Duration

	hc *http.Client
}

// NewResendClient returns a client with sensible defaults. The
// caller MUST verify APIKey is non-empty (Config.ResendEnabled)
// before calling Send — an empty key returns a *ErrConfig error
// immediately rather than posting unauthenticated requests to
// Resend.
func NewResendClient(apiKey, fromAddr string, timeout time.Duration) *ResendClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &ResendClient{
		APIKey:   apiKey,
		From:     fromAddr,
		Endpoint: "https://api.resend.com/emails",
		Timeout:  timeout,
		// Operator class: api.resend.com, or an internal relay the operator
		// points at deliberately for an air-gapped install.
		hc: egress.Client(egress.Operator, timeout),
	}
}

// ErrConfig is returned when the Resend client is called without
// an API key. This is the "feature disabled" sentinel.
var ErrConfig = errors.New("resend: not configured (set NEXUS_RESEND_API_KEY)")

// resendSendRequest mirrors the POST /emails body documented at
// https://resend.com/docs/api-reference/emails/send-email. We
// keep only the fields we currently send: from, to, subject,
// html + idempotency-key.
type resendSendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html"`
}

// resendSendResponse carries the message id Resend returns on a
// successful send. We don't act on it today, but we surface it
// to the audit trail so an operator can correlate "we sent
// invite X" → Resend message id → Resend dashboard / API logs.
type resendSendResponse struct {
	ID string `json:"id"`
}

// resendErrorBody is what Resend renders for non-2xx responses.
// Their docs don't promise a stable shape across all error
// categories, so we decode opportunistically and fall back to
// the raw text when the field is absent.
type resendErrorBody struct {
	StatusCode int    `json:"-"`
	Message    string `json:"message"`
	Name       string `json:"name"`
}

// Send delivers a single transactional email. idempotencyKey is
// required so the retry path on transient failures does not
// produce duplicates. The key is preserved verbatim on the
// Resend side for 24 hours.
func (c *ResendClient) Send(ctx context.Context, to, subject, html, idempotencyKey string) (string, error) {
	if c == nil || c.APIKey == "" {
		return "", ErrConfig
	}
	body, err := json.Marshal(resendSendRequest{
		From:    c.From,
		To:      []string{to},
		Subject: subject,
		HTML:    html,
	})
	if err != nil {
		return "", fmt.Errorf("resend: marshal send body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("resend: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "nexus-console/1.0")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("resend: post /emails: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var ok resendSendResponse
		if err := json.NewDecoder(resp.Body).Decode(&ok); err != nil {
			return "", fmt.Errorf("resend: decode 2xx body: %w", err)
		}
		return ok.ID, nil
	default:
		var e resendErrorBody
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Message == "" {
			e.Message = resp.Status
		}
		return "", fmt.Errorf("resend: %d %s: %s", resp.StatusCode, resp.Status, e.Message)
	}
}

// renderInviteHTML is a deliberately small html/template string.
// React-Email + the resend-go SDK ship richer primitives, but
// Nexus's invite payload is one paragraph + two buttons
// (primary CTA, plain-text fallback). Inlining that keeps a
// dependency in the binary off the table for what is essentially
// a constrained marketing email.
func renderInviteHTML(inviteURL, inviterEmail, role string) string {
	// Inline-template without text/template to avoid a runtime
	// dependency on filesystem templates. Users in this flow
	// expect a "Welcome to Nexus" surface — anything fancier
	// belongs in a React-Email port landed in a follow-up.
	const tmpl = `<!doctype html>
<html>
  <body style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Helvetica, Arial, sans-serif; color:#0e0e10; background:#f6f6f9; padding:24px;">
    <div style="max-width:560px; margin:0 auto; background:#ffffff; border:1px solid #e6e6ee; border-radius:12px; padding:28px 32px;">
      <h1 style="margin:0 0 16px; font-size:20px;">You're invited to Nexus</h1>
      <p style="margin:0 0 16px; line-height:1.5;">
        %s has invited you to join their Nexus workspace. Click the button below
        to accept the invite and set your password. This link is personal —
        please don't share it.
      </p>
      <p style="margin:24px 0;">
        <a href="%s"
           style="display:inline-block; background:linear-gradient(135deg,#5a4cff,#8a4cff); color:#ffffff; text-decoration:none; font-weight:600; padding:12px 20px; border-radius:8px;">
          Accept invite
        </a>
      </p>
      <p style="margin:24px 0 8px; font-size:12px; color:#6b6b78;">
        Or paste this URL into your browser:
      </p>
      <p style="margin:0 0 24px; font-size:12px; color:#1f1f24; word-break:break-all; background:#f6f6f9; padding:10px 12px; border-radius:6px;">
        %s
      </p>
      <hr style="border:none; border-top:1px solid #e6e6ee; margin:24px 0;"/>
      <p style="margin:0; font-size:11px; color:#a0a0ac; line-height:1.5;">
        Role on accept: <strong>%s</strong>.<br/>
        If you weren't expecting this email you can safely ignore it — the
        invite will expire automatically.
      </p>
    </div>
  </body>
</html>`
	// %s on the link does double duty — caller's responsibility
	// to pass a URL that is already validated for the recipient.
	return fmt.Sprintf(tmpl, inviterEmail, inviteURL, inviteURL, role)
}
