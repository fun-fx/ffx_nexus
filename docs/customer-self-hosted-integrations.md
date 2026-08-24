# Customer Self-Hosted Integrations

This document is the install guide for the integrations a Nexus self-hosted
customer must wire at deploy time. The intent is that an operator without
Nexus-specific context can read it once and produce a working install,
and that "which transport is the right one here" has a defended answer.

## Email transport

The Invite User feature needs to send a one-shot envelope containing a
short URL the invitee uses to set their password. There are exactly two
transports Nexus accepts:

| `NEXUS_EMAIL_PROVIDER` | When to choose it |
| --- | --- |
| `smtp` | The customer's control plane lives on a network that has its own mail relay (Microsoft 365, Google Workspace with allowlisting, Microsoft Exchange on-prem, Postfix, an internal Sendmail/Postfix MTA). This is the default for enterprise self-hosted. |
| `resend` | The customer's email is sent through the [Resend] SaaS. Resend is the only third-party mail SaaS Nexus wraps today; it is not vendor-pinned — the URL is configurable, so a private relay that mimics Resend's `/emails` POST works too. |

[Resend]: https://resend.com/

### Why SMTP is the default

Nexus is a self-hosted product; the operator that runs it controls the
network the pod lives on, and that operator's customers receive mail
addressed *as* that operator. Sending that mail through Cloudflare or
the vendor's own Resend account means the cluster ships content outbound
to a third party whenever an admin invites a teammate, and the
third-party's API key is a secret the user is silently trusting the
vendor with. Two architectural problems follow:

1. **Tenancy confusion.** The envelope's `From:` says "Nexus <noreply@your
   company.example>" — but the actual transport-serving identity is the
   vendor's `noreply@vendor.example`. A spam-classifier that sees the
   vendor's sending domain for mail claiming to be from the customer's
   domain is correctly skeptical; the customer's deliverability suffers.

2. **Secret custody.** The vendor's API key sits in the customer's
   Helm values unless the operator rewrote it. The vendor retains root
   visibility into the sending cluster without the customer's security
   team vetting that visibility.

SMTP, by contrast, points at the relay the customer already runs and
already authenticated with DKIM/SPF/DMARC for the customer's domain. No
new third party, no new secret, no skewed sender identity. Resend remains
an option for the operator that wants it.

### Resolving the From address

`NEXUS_EMAIL_FROM_ADDRESS` is the canonical knob. The example value
`Nexus <noreply@your-company.example>` shows the display-name + addr
shape that mail clients render. The `<address>` portion must be on a
domain whose DNS the operator controls (DKIM/SPF must point at their
relay); presenting an envelope claiming a domain the operator cannot
authenticate is what bounces get caught on.

The previous name `NEXUS_RESEND_FROM_ADDRESS` is honoured as a deprecated
alias so existing Resend-only installs keep working. A boot-time warning
points the operator at the rename before the next minor release.

### Envelope content

Nexus's default envelope is a single paragraph + URL the invitee uses
to set their password. The HTML body uses `html/template` so any value
that lands in the model (inviter email, role, URL) is HTML-escaped — an
inviter with a `<script>` in their display name renders as escaped text
inside the envelope rather than as live `<script>` in the recipient's
mail client.

The body is generated from a typed template file in
`internal/console/invite_email.go`; changing it requires editing the
template literal and running the regression tests in
`internal/console/email_test.go`.

### Failure modes and operator visibility

Every send produces a sealed audit row, keyed on the invite id:

| `core.AuditAction` | When |
| --- | --- |
| `invite.email.started` | The handler detached from the request context with the body. (Reserved for future use; today the send is a goroutine off the request so the audit row is best-effort.) |
| `invite.email.sent` | The transport returned a message id. The id is appended to the row so an operator can correlate "Nexus sent X" → "Resend dashboard row" or "MTA log entry". |
| `invite.email.failed` | The transport rejected the envelope. The Detail includes the relay host name and the error text. |
| `invite.email.template_failed` | The HTML template failed to render (typically a future regression of the template literal). |

A failed send does NOT unwind a successful invite — the invite row is
already committed and the URL is reproducible from the admin console.
The audit row is operator signal, and a high rate of `email.failed` is a
reason to re-run `nexus mailtest` and look at the relay.

## Operator verification: `nexus mailtest`

Every install gets a one-line post-config check:

```bash
nexus mailtest --to ops@your-company.example
```

The subcommand wires the same `console.Mailer` an admin invite would use
and sends a single probe envelope. Exit codes are distinct:

* `0` — the transport acknowledged the envelope with a non-empty id.
* `1` — the configuration refused to construct a Mailer (missing
  env vars, `encryption=none` outside DevMode, etc.) — fix the
  install, do not retry.
* `2` — the transport accepted and then rejected the envelope
  (network reachable but the relay says no). Look at the relay logs
  and the audit row for the specific Send attempt.

The subcommand intentionally does not touch the database: invite
creation is a separate flow, and the test envelope does not consume a
phantom invite row.

## Other integrations

Other operator-configured integrations (OTLP, SSO, Metabase, Grafana,
LLM upstream credentials) follow the same architectural pattern: the
operator chooses the destination, the credentials, and the timeout at
install time, and the console never sees them. Their docs live in the
respective subdirectories.

See also:

- `customer-self-hosted-security.md` — the security properties the
  install preserves end to end, including the egress policy that
  governs every outbound transport call.
- `customer-self-hosted-upgrade-rollback.md` — the upgrade / downgrade
  story against a newer schema, including the migration job's exit codes.
