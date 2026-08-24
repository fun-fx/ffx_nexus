package main

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/console"
)

// installEmailTransport resolves cfg into a Mailer for the console
// server.
//
// # Failure semantics
//
//   - A valid provider name with a missing transport (Resend without
//     API key; SMTP without host) is delegated to config.EmailMode,
//     which already fail-fasts at boot. The returned Mailer is what
//     main.go hands to Server.SetMailer.
//
//   - "noop" is the in-process discard transport; it is gated to
//     NEXUS_DEV_MODE=true by the boot helper so a producing cluster
//     cannot produce an accept-and-drop cluster accidentally.
//
//   - An "encrypted off" configuration (SMTP_ENCRYPTION=none) is
//     FORBIDDEN unless DevMode is true. Cleartext AUTH credentials are
//     never something Nexus should ship.
//
// # Returns
//
// The Mailer for unit-testability; production code (cmd/nexus/main.go)
// wires it through Server.SetMailer. A nil Mailer return is impossible
// here — the empty-mode case still hands back a noop so the wire is
// always populated.
func installEmailTransport(cfg config.Config, log *slog.Logger) console.Mailer {
	mode, from, _, err := cfg.EmailMode()
	if err != nil {
		// EmailMode has already validated the from address, the named
		// provider, and the per-provider required fields. Fail-fast
		// from the boot path: a misconfigured install should not start
		// sending invites from a domain the operator cannot authenticate.
		log.Error("email transport refused at boot", "err", err)
		// We return a noop here AND let main.go's wrapping fatal-error
		// detection unwind — the alternatives (panic, fatal stderr) are
		// similar in effect and noisier in tests.
		return &console.NoopMailer{Log: log}
	}
	switch mode {
	case config.EmailModeSMTP:
		m, err := constructSMTP(cfg, from, log)
		if err != nil {
			log.Error("email transport refused at boot: smtp", "err", err)
			return &console.NoopMailer{Log: log}
		}
		return m
	case config.EmailModeResend:
		return constructResend(cfg, from, log)
	case "":
		// Empty mode + non-empty From is impossible here; an empty mode +
		// empty From is the "no transport" case. We still wire a noop so
		// the server has a non-nil Mailer, and warn loudly so admins see
		// it on every boot.
		log.Warn("email transport unconfigured: invite emails will silently drop via noop. " +
			"Set NEXUS_EMAIL_PROVIDER=smtp|resend and NEXUS_EMAIL_FROM_ADDRESS to keep the invite flow working.")
		return &console.NoopMailer{Log: log}
	default:
		log.Error(fmt.Sprintf("email transport refused at boot: mode=%q is unknown", mode))
		return &console.NoopMailer{Log: log}
	}
}

func constructSMTP(cfg config.Config, from string, log *slog.Logger) (console.Mailer, error) {
	if strings.TrimSpace(cfg.SMTPHost) == "" {
		return nil, errors.New("smtp: NEXUS_SMTP_HOST is empty but NEXUS_EMAIL_PROVIDER=smtp")
	}
	if cfg.SMTPPort <= 0 || cfg.SMTPPort > 65535 {
		return nil, fmt.Errorf("smtp: NEXUS_SMTP_PORT=%d is out of range", cfg.SMTPPort)
	}
	enc := strings.ToLower(strings.TrimSpace(cfg.SMTPEncryption))
	if enc == "none" && !cfg.DevMode {
		return nil, errors.New("smtp: NEXUS_SMTP_ENCRYPTION=none is dev-only; " +
			"customer installs must use starttls or tls; refusing to send credentials in cleartext")
	}
	m, err := console.NewSMTPMailer(cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUsername, cfg.SMTPPassword, from, enc, cfg.EmailRequestTimeout)
	if err != nil {
		return nil, fmt.Errorf("smtp: %w", err)
	}
	log.Info("email transport wired: SMTP",
		"host", cfg.SMTPHost,
		"port", cfg.SMTPPort,
		"encryption", enc,
		"from", from,
		"auth", cfg.SMTPUsername != "",
	)
	return m, nil
}

func constructResend(cfg config.Config, from string, log *slog.Logger) console.Mailer {
	if strings.TrimSpace(cfg.ResendAPIKey) == "" {
		// EmailMode() refused to return mode=resend without a key, so
		// this is unreachable in practice. The check stays so a future
		// config edit doesn't silently bypass it.
		log.Error("resend: NEXUS_RESEND_API_KEY is empty but NEXUS_EMAIL_PROVIDER=resend; " +
			"the assembled client will refuse to send every envelope, switch to NEXUS_EMAIL_PROVIDER=smtp " +
			"or set the API key")
	}
	c := console.NewResendClient(cfg.ResendAPIKey, from, cfg.ResendAPIBaseURL, cfg.EmailRequestTimeout)
	log.Info("email transport wired: Resend",
		"from", from,
		"base_url", c.BaseURL,
		"timeout", cfg.EmailRequestTimeout,
	)
	return c
}
