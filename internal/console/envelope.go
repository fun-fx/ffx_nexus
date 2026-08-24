package console

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"strings"
)

// envelopeClient is a small wrapper around *smtp.Client that adds the
// pieces net/smtp does not surface: TLS handshake tracking (so we can
// refuse AUTH over cleartext), our own hostname-checked TLS config for
// implicit-TLS port 465, and per-call error messages that name the
// destination so auditable SMTP failures are intelligible later.
type envelopeClient struct {
	client     *smtp.Client
	tls        bool
	serverName string
}

// newEnvelopeClient takes an already-dialed TCP connection and negotiates
// the envelope: EHLO first, then either STARTTLS or implicit TLS based on
// the operator's choice. Errors here are returned to Send and surface in
// the audit row with the relay host attached, so an on-call engineer can
// tell "the relay does not support TLS" from "the relay rejected our host-
// name certificate" without packet-capture acrobatics.
//
// Encryption modes:
//
//   - starttls: EHLO, then STARTTLS, then EHLO again. Required by RFC 3207.
//     A relay that does not advertise STARTTLS is refused with an error,
//     not silently downgraded to cleartext.
//
//   - tls: the connection is TLS-wrapped before the SMTP greeting. Used
//     with port 465's implicit-TLS posture; the EHLO/STARTTLS dance is
//     not performed because the channel is already encrypted.
//
//   - none: no TLS layer at all. Refused by the boot helper in customer
//     installs; only constructed via NewSMTPMailerInsecure under DevMode
//     for local development.
func newEnvelopeClient(conn net.Conn, host, mode string) (*envelopeClient, error) {
	switch mode {
	case "starttls":
		client, err := smtp.NewClient(conn, host)
		if err != nil {
			return nil, fmt.Errorf("smtp: client: %w", err)
		}
		if err := client.Hello("nexus.local"); err != nil {
			return nil, fmt.Errorf("smtp: EHLO: %w", err)
		}
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return nil, errors.New("smtp: relay did not advertise STARTTLS; refusing to send credentials")
		}
		cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
		if err := client.StartTLS(cfg); err != nil {
			return nil, fmt.Errorf("smtp: STARTTLS handshake: %w", err)
		}
		// Re-issue EHLO after STARTTLS per RFC 3207 §4.2; some capabilities
		// change once encryption is on, and AUTH/STARTTLS-or-not is one of
		// the common ones.
		if err := client.Hello("nexus.local"); err != nil {
			return nil, fmt.Errorf("smtp: post-STARTTLS EHLO: %w", err)
		}
		return &envelopeClient{client: client, tls: true, serverName: host}, nil

	case "tls":
		cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
		tlsConn := tls.Client(conn, cfg)
		if err := tlsConn.Handshake(); err != nil {
			return nil, fmt.Errorf("smtp: implicit TLS handshake: %w", err)
		}
		client, err := smtp.NewClient(tlsConn, host)
		if err != nil {
			return nil, fmt.Errorf("smtp: client over TLS: %w", err)
		}
		if err := client.Hello("nexus.local"); err != nil {
			return nil, fmt.Errorf("smtp: EHLO over TLS: %w", err)
		}
		return &envelopeClient{client: client, tls: true, serverName: host}, nil

	case "none":
		client, err := smtp.NewClient(conn, host)
		if err != nil {
			return nil, fmt.Errorf("smtp: client: %w", err)
		}
		if err := client.Hello("nexus.local"); err != nil {
			return nil, fmt.Errorf("smtp: EHLO: %w", err)
		}
		return &envelopeClient{client: client, tls: false, serverName: host}, nil

	default:
		return nil, fmt.Errorf("smtp: unknown encryption mode %q", mode)
	}
}

// tlsDone reports whether STARTTLS / implicit TLS has been negotiated. The
// AUTH step refuses to send credentials unless this is true.
func (c *envelopeClient) tlsDone() bool { return c.tls }

// mail runs MAIL FROM. Errors return the upstream's 5xx reply text,
// scoped to the orchestrating Send call by the caller.
func (c *envelopeClient) mail(from string) error {
	if err := c.client.Mail(from); err != nil {
		return err
	}
	return nil
}

// rcpt runs RCPT TO.
func (c *envelopeClient) rcpt(to string) error {
	if err := c.client.Rcpt(to); err != nil {
		return err
	}
	return nil
}

// data opens the DATA block; the caller writes the headers + body to
// the returned io.WriteCloser and closes it to send.
func (c *envelopeClient) data() (io.WriteCloser, error) {
	w, err := c.client.Data()
	if err != nil {
		return nil, err
	}
	return w, nil
}

// Quit sends QUIT and closes the underlying connection.
func (c *envelopeClient) Quit() error {
	if c.client == nil {
		return nil
	}
	return c.client.Quit()
}

// authPlain wraps smtp.Client.Auth with smtp.PlainAuth so callers do not
// reach into the smtp package's lower-level types. The ServerName on
// smtp.Client is unexported; we know our own hostname because Send was
// called with a Mailer that has it, and we know we connected to it, so
// the matching happens implicitly via TLS — the relay's certificate was
// already verified against s.Host during the handshake.
func (c *envelopeClient) authPlain(username, password string) error {
	if c.client == nil {
		return errors.New("smtp: nil client")
	}
	if strings.TrimSpace(username) == "" || password == "" {
		return errors.New("smtp: empty username or password for AUTH PLAIN")
	}
	// The smtp.PlainAuth ServerName argument is the hostname used in
	// the AUTH exchange; plugging the underlying smtp.Client's tracked
	// hostname keeps the relay looking the same to itself as it does
	// in the EHLO it already accepted.
	auth := smtp.PlainAuth("", username, password, c.lastServerName())
	if err := c.client.Auth(auth); err != nil {
		return err
	}
	return nil
}

// lastServerName regenerates the server name smtp.Client tracked from
// NewClient. smtp.NewClient does not expose it post-hoc, so the wrapper
// stores it at construction.
func (c *envelopeClient) lastServerName() string {
	return c.serverName
}
