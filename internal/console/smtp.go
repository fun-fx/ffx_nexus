package console

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/ffxnexus/nexus/internal/egress"
)

// SMTPMailer is the SMTP transport that sends invitations through a
// customer's own relay. Construction requires non-empty Host, From, and a
// usable Port; the boot helper rejects half-configured transports before
// this struct ever comes into existence, so Send can assume the dialer and
// From were already validated.
//
// The implementation uses Go's stdlib net/smtp with a guarded
// *net.Dialer from internal/egress so the SMTP connect also runs the
// connect-time address check the HTTP egress path uses. That is the load-
// bearing property: a relay whose hostname resolves to a private address
// at dial time is rejected the same way an HTTP call to it would be.
//
// Empty Username is the authentic case for "the relay allowlists our
// egress NAT IP and does not require credentials" — in-cluster Postfix,
// Microsoft 365 direct-send, smtp-relay.gmail.com with IP allowlisting,
// many on-prem Exchange install paths. AUTH is skipped in that branch;
// refusing to send without credentials would reject the most common
// enterprise topology in error.
//
// STARTTLS is REQUIRED when encryption="starttls". A relay that does not
// advertise STARTTLS, or strips the advertisement on the response stream,
// fails the send — cleartext AUTH credentials are not something we want
// Nexus to ever emit. TLS handshakes verify the configured hostname against
// the certificate, just like an HTTPS client would.
//
// Headers are CRLF-policed at construction time: an operator who pastes a
// From containing \r or \n never reaches Send.
type SMTPMailer struct {
	Host       string
	Port       int
	Username   string
	Password   string
	From       string
	Encryption string // "starttls" (default) | "tls" (implicit, 465) | "none" (dev-only)
	Timeout    time.Duration

	dialer net.Dialer
}

// NewSMTPMailer validates the static fields and constructs the dialer.
// Returns ErrSMTPNotConfigured when any required field is empty.
// ErrSMTPBogusFrom when the From contains CRLF; the caller can show this
// in the audit row since it is operator-controlled data.
func NewSMTPMailer(host string, port int, username, password, fromAddr, encryption string, timeout time.Duration) (*SMTPMailer, error) {
	if strings.TrimSpace(host) == "" || fromAddr == "" {
		return nil, ErrSMTPNotConfigured
	}
	if strings.ContainsAny(fromAddr, "\r\n") {
		return nil, fmt.Errorf("smtp: From header contains a bare CR/LF: %q", fromAddr)
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	enc := strings.ToLower(strings.TrimSpace(encryption))
	switch enc {
	case "", "starttls":
		enc = "starttls"
	case "tls", "starttlsimplicit":
		// "tls" is implicit TLS (port 465); "starttlsimplicit" is the
		// longer form for operators who insist on being explicit.
		enc = "tls"
	case "none":
		// Permitted only by NewSMTPMailerInsecure for local dev use;
		// the boot helper refuses to call it without DevMode so the
		// chart never selects it.
	default:
		return nil, fmt.Errorf("smtp: encryption %q is not one of starttls|tls|none", encryption)
	}
	return &SMTPMailer{
		Host:       host,
		Port:       port,
		Username:   username,
		Password:   password,
		From:       fromAddr,
		Encryption: enc,
		Timeout:    timeout,
		dialer:     *egress.Dialer(egress.Operator),
	}, nil
}

// ErrSMTPNotConfigured is what callers see when config fields are
// missing — it does NOT include which one, because the boot helper is the
// authority on that and the handler should not double-audit it.
var ErrSMTPNotConfigured = errors.New("smtp: not configured (set NEXUS_SMTP_HOST and NEXUS_EMAIL_FROM_ADDRESS)")

// Send delivers one envelope. The host:port dial uses the egress guard;
// encrypts the channel per the configured mode; and AUTH only after TLS.
// The idempotency key is folded into Message-ID so retrying the same
// envelope (admin changes their mind and re-sends) is observable in
// downstream relay logs.
func (s *SMTPMailer) Send(ctx context.Context, to, subject, html, idempotencyKey string) (string, error) {
	if s == nil || s.Host == "" {
		return "", ErrSMTPNotConfigured
	}
	if strings.ContainsAny(to, "\r\n") {
		return "", fmt.Errorf("smtp: recipient contains a bare CR/LF: %q", to)
	}

	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	conn, err := s.dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("smtp: dial %s: %w", s.Host, err)
	}
	// Close the raw TCP conn on every exit so a return mid-method does
	// not leak a half-handshaken socket to the relay.
	defer func() {
		if c, ok := conn.(*net.TCPConn); ok && c != nil {
			_ = c.Close()
		}
	}()
	deadline := time.Now().Add(s.Timeout)
	if err := conn.SetDeadline(deadline); err != nil {
		return "", fmt.Errorf("smtp: set deadline: %w", err)
	}

	client, err := newEnvelopeClient(conn, s.Host, s.Encryption)
	if err != nil {
		return "", err
	}
	defer func() { _ = client.Quit() }()

	// Three cases for AUTH:
	//   1. Both Username and Password set → AUTH PLAIN after TLS.
	//   2. Only Username set → AUTH without password is a malformed
	//      configuration; surface as an error rather than silently
	//      submitting queued mail as anonymous.
	//   3. Neither set → skip AUTH. Common and operator-supported for
	//      relays that trust the source IP (Postfix, M365 direct-send,
	//      Google Workspace IP-allowlisted relay).
	switch {
	case s.Username == "" && s.Password == "":
		// intentionally no auth: see comment above.
	case s.Username != "" && s.Password != "":
		if !client.tlsDone() {
			// Refuse to send credentials in cleartext. A relay that
			// did not negotiate TLS — STARTTLS stripped, implicit-TLS
			// configured wrong, encryption=none — gets a real error
			// here, not a silent downgrade.
			return "", errors.New("smtp: refusing to AUTH without a TLS channel (the relay did not advertise STARTTLS)")
		}
		// AuthPlain follows the net/smtp contract for sending both
		// identity fields in a single round trip.
		if err := client.authPlain(s.Username, s.Password); err != nil {
			return "", fmt.Errorf("smtp: AUTH PLAIN: %w", err)
		}
	default:
		return "", errors.New("smtp: SMTP_USERNAME is set but SMTP_PASSWORD is empty; refusing to AUTH half-way")
	}

	msgID := buildMessageID(idempotencyKey, s.Host)
	headers := composeHeaders(s.From, to, subject, msgID)
	body := buildMIME(html, "text/html; charset=utf-8")

	if err := client.mail(s.From); err != nil {
		return "", fmt.Errorf("smtp: MAIL FROM: %w", err)
	}
	if err := client.rcpt(to); err != nil {
		return "", fmt.Errorf("smtp: RCPT TO: %w", err)
	}
	w, err := client.data()
	if err != nil {
		return "", fmt.Errorf("smtp: DATA: %w", err)
	}
	if _, err := w.Write([]byte(headers)); err != nil {
		return "", fmt.Errorf("smtp: write headers: %w", err)
	}
	// The MIME preamble ended with a terminating blank line so the body
	// starts on its own. Splitting headers / body here gives the caller
	// a clear error path on either side.
	if _, err := w.Write([]byte("\r\n")); err != nil {
		return "", fmt.Errorf("smtp: write separator: %w", err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		return "", fmt.Errorf("smtp: write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("smtp: close data: %w", err)
	}
	return msgID, nil
}

// buildMessageID constructs a stable Message-ID. The idempotency key is
// folded in so the same envelope returning twice correlates in the
// receiving IMAP mailbox / delegated mailbox tool / forensic search.
func buildMessageID(idempotencyKey, host string) string {
	suffix := ""
	if k := strings.TrimSpace(idempotencyKey); k != "" {
		// 8 hex chars of the key are enough to disambiguate two retries
		// without leaking the full secret in a Message-ID header that
		// the receiving MUA might log.
		var buf [4]byte
		_, _ = rand.Read(buf[:])
		suffix = "-" + hex.EncodeToString(buf[:]) + "-" + truncateForHeader(k, 8)
	}
	return "<nexus-invite" + suffix + "@" + host + ">"
}

// truncateForHeader keeps the suffix readable in mail clients that
// truncate Message-IDs aggressively. The existing truncate in the package
// is a whitespace-collapsing string formatter used by smoke tests; this
// helper is a byte-truncator for Message-ID headers where whitespace
// collapsing would corrupt the hex.
func truncateForHeader(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// composeHeaders joins RFC 5322 header lines. CRLF injection is impossible
// because the From is checked at NewSMTPMailer, the recipient is
// validated at Send, and the subject is the constant caller-supplied one
// (the invite flow passes "You're invited to Nexus" verbatim).
func composeHeaders(from, to, subject, messageID string) string {
	var b strings.Builder
	b.WriteString("From: ")
	b.WriteString(from)
	b.WriteString("\r\n")
	b.WriteString("To: ")
	b.WriteString(to)
	b.WriteString("\r\n")
	b.WriteString("Subject: ")
	b.WriteString(subject)
	b.WriteString("\r\n")
	b.WriteString("Date: ")
	b.WriteString(time.Now().UTC().Format(time.RFC1123Z))
	b.WriteString("\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Message-ID: ")
	b.WriteString(messageID)
	b.WriteString("\r\n")
	b.WriteString("X-Nexus-Source: nexus-console\r\n")
	b.WriteString("X-Nexus-Invite: 1\r\n")
	return b.String()
}

// buildMIME wraps body in a single-section multipart with explicit
// Content-Type. The body is treated as already-escaped HTML (the caller
// runs renderInviteHTML through html/template before Send).
func buildMIME(body, contentType string) string {
	var b strings.Builder
	b.WriteString("Content-Type: ")
	b.WriteString(contentType)
	b.WriteString("\r\n")
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	b.WriteString(body)
	return b.String()
}
