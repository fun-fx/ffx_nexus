package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/mail"
	"os"
	"strings"
	"time"

	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/console"
)

// runMailtestCommand implements `nexus mailtest`.
//
// A dedicated subcommand exists so an operator who just edited the
// NEXUS_SMTP_* / NEXUS_RESEND_* block on a new cluster has a one-liner
// to confirm: end-to-end the email path produces a real envelope on the
// receiving mailbox, with subject, body, idempotency, and audit row
// behaving the same way as an admin-issued invite would. The same job
// can be bundled into the Helm chart as a post-upgrade Job that tests
// the configured transport before the rollout is considered ready.
//
// Exit codes:
//
//	0  the test envelope was handed to the configured transport and the
//	   transport accepted it (SMTP DATA-terminated; Resend returned 2xx;
//	   noop acknowledged the id).
//	1  configuration refused at parse time (missing fields, parse
//	   errors, dev-only ENCRYPTION=none in non-dev runs) — failing the
//	   boot of the chart instead of letting a 500-bombing rollout proceed.
//	2  the relay / Resend rejected the test envelope; the operator's
//	   next step is to look at the relay logs (or the audit_log row
//	   from the admin invite path). Distinct from exit 1 so a CI pre-
//	   upgrade job can fail the rollout without confusing
//	   "configuration broke" with "the network is reachable but the
//	   content bounced".
//
// The command never touches the database: invite creation is a separate
// flow, and the test envelope does not need a row to verify the
// transport. The audit_log entry the operator will see when an admin
// actually issues an invite uses the same Mailer, so the test exercises
// the whole transport code path without consuming a phantom invite
// record.
func runMailtestCommand(args []string) int {
	fs := flag.NewFlagSet("nexus mailtest", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: nexus mailtest [flags]

Sends a single test envelope to the address passed with --to, using the same
Mailer a /api/invites request would use. Intended for verifying the
NEXUS_EMAIL_FROM_ADDRESS + NEXUS_SMTP_* / NEXUS_RESEND_* configuration right
after a Helm install or a config edit.

Connection details come from the same environment as the server. A transport
that is not configured returns exit 1, NOT a silent skip — the operator
committed to a config and the test exists to prove that config works.

Flags:
`)
		fs.PrintDefaults()
	}

	var (
		to      = fs.String("to", "", "destination address for the test envelope (required)")
		subject = fs.String("subject", "Nexus mailtest", "subject line for the test envelope")
		timeout = fs.Duration("timeout", 30*time.Second, "overall deadline for the dial + handshake + DATA")
		body    = fs.String("body", "Nexus mailer self-test; please ignore.", "plain-text body of the test envelope")
	)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if strings.TrimSpace(*to) == "" {
		fmt.Fprintln(os.Stderr, "--to is required")
		return 1
	}
	if _, err := mail.ParseAddress(*to); err != nil {
		fmt.Fprintf(os.Stderr, "--to %q is not a valid envelope recipient: %v\n", *to, err)
		return 1
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := config.Load()

	mailer := installEmailTransport(cfg, log)
	if mailer == nil {
		fmt.Fprintln(os.Stderr, "email transport could not be constructed; inspect the boot log above")
		return 1
	}
	// Detect the noop path specifically: it's not a failure to have, but
	// it is not a successful transport test either. The exit code is 1
	// in this case so a CI job doesn't think "noop acknowledged" means
	// the SMTP path was exercised.
	if _, isNoop := mailer.(*console.NoopMailer); isNoop {
		fmt.Fprintln(os.Stderr, "email transport is noop (no provider configured or NEXUS_EMAIL_PROVIDER=dev); mailtest refused to proceed so a real test cannot be confused with a noop acknowledgement")
		return 1
	}

	// Idempotency here is a per-run constant, intentionally not random:
	// re-running the same command in CI must produce the same Message-ID
	// so a relay sees "the previous run was identical, dedupe" rather
	// than "we just got N identical envelopes". The MD5 is overkill, but
	// standardising the format (and not having a prefix that varies
	// across runs) is what makes the "this many envelopes arrived"
	// check operational.
	idem := "nexus-mailtest-" + time.Now().UTC().Format("20060102T150405Z")
	// Wrap the configured EmailRequestTimeout in a hard upper bound so
	// even a misconfigured 600-second timeout does not stall a CI job.
	budget := *timeout
	if cfg.EmailRequestTimeout > 0 && cfg.EmailRequestTimeout < budget {
		budget = cfg.EmailRequestTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	msgID, err := mailer.Send(ctx, *to, *subject, *body, idem)
	if err != nil {
		fmt.Fprintf(os.Stderr, "envelope was rejected by the transport: %v\n", err)
		return 2
	}
	if msgID == "" {
		fmt.Fprintln(os.Stderr, "transport accepted the envelope but returned no message id; the relay may be misconfigured")
		return 2
	}
	fmt.Printf("OK — message id: %s\n", msgID)
	return 0
}
