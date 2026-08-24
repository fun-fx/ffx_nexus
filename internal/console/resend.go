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
// # Why not resend-go
//
// We deliberately avoid the third-party SDK so the dependency surface
// stays tight: the only endpoint Nexus exercises is POST /emails, and
// its contract — Authorization: Bearer, JSON body, 200 + {"id"} — is
// small enough that a stdlib http.Client call is the whole integration.
// Keeping SDK churn out of release notes is the bonus.
//
// # Termination behaviour
//
// The HTTP client is an egress.Operator dialer so a self-hosted install
// pointing at an internal relay (NEXUS_RESEND_API_BASE_URL) gets the
// same connect-time IP policy as any other outbound request. The bare
// `https://api.resend.com` default is the operator's chosen SaaS.
//
// # Idempotency
//
// Every send attaches an `Idempotency-Key` header derived from the
// invite id. Resend deduplicates within 24 hours, so the retry path on
// transient failures stays clean.
//
// # Failure modes
//
//   - 5xx upstream → wrapped sentinel; caller decides retry behaviour.
//   - 4xx upstream → bubbles up so the admin sees the real reason.
//   - network / DNS → net.OpError-style wrapped error.
//
// # Concurrency
//
// *http.Client is safe for concurrent use.
type ResendClient struct {
	APIKey   string
	From     string                  // e.g. "Nexus <noreply@yourcompany.example>"
	Endpoint string                  // overridable for test stubs and internal relays
	BaseURL  string                  // base for /emails resolution; defaults to https://api.resend.com
	Timeout  time.Duration

	hc *http.Client
}

// NewResendClient returns a client with sane defaults. baseURL may be
// empty — it falls back to https://api.resend.com, which is the right
// choice when the operator has not told us otherwise.
//
// The caller MUST verify APIKey is non-empty before calling Send; an
// empty key returns ErrConfig immediately rather than posting
// unauthenticated requests to Resend.
func NewResendClient(apiKey, fromAddr, baseURL string, timeout time.Duration) *ResendClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if baseURL == "" {
		baseURL = "https://api.resend.com"
	}
	c := &ResendClient{
		APIKey:   apiKey,
		From:     fromAddr,
		BaseURL:  baseURL,
		Endpoint: baseURL + "/emails",
		Timeout:  timeout,
		// Operator class: api.resend.com or an internal relay the
		// operator points at deliberately for an air-gapped install.
		hc: egress.Client(egress.Operator, timeout),
	}
	return c
}

// ErrConfig is returned when the Resend client is called without
// an API key. The honours "feature disabled" sentinel.
var ErrConfig = errors.New("resend: not configured (set NEXUS_RESEND_API_KEY)")

// resendSendRequest mirrors the POST /emails body documented at
// https://resend.com/docs/api-reference/emails/send-email. Only the
// fields we currently send are modelled.
type resendSendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html"`
}

// resendSendResponse carries Resend's message id; the audit trail
// records it so an operator can correlate "we sent invite X" to the
// Resend dashboard.
type resendSendResponse struct {
	ID string `json:"id"`
}

// resendErrorBody is what Resend returns for non-2xx responses. Their
// docs do not promise a stable shape across all error categories, so
// decoding is opportunistic and we fall back to the raw status text
// when the field is absent.
type resendErrorBody struct {
	StatusCode int    `json:"-"`
	Message    string `json:"message"`
	Name       string `json:"name"`
}

// Send delivers a single transactional email. idempotencyKey is required
// so the retry path on transient failures does not produce duplicates;
// Resend preserves the key verbatim for 24 hours.
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
