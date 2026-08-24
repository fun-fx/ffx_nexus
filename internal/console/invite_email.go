package console

import (
	"bytes"
	"fmt"
	"html/template"
)

// inviteTemplate is the only HTML body Nexus sends today. It uses
// html/template (not text/template) so any value interpolated into the
// HTML body is auto-escaped — a custom display-name, a role string, or
// (for the local-dev "preview" type) anything that came in through a
// path that wasn't already URL-validated.
//
// The recipient clicks one button; everything else is navigation
// provenance ("here is the URL we sent", "here is the role"). Two paragraphs
// and a button is intentional: a richer React-Email port lands in a
// follow-up because the current rendering does not block any product
// behaviour.
var inviteTemplate = template.Must(template.New("invite").Parse(`<!doctype html>
<html>
  <body style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Helvetica, Arial, sans-serif; color:#0e0e10; background:#f6f6f9; padding:24px;">
    <div style="max-width:560px; margin:0 auto; background:#ffffff; border:1px solid #e6e6ee; border-radius:12px; padding:28px 32px;">
      <h1 style="margin:0 0 16px; font-size:20px;">You're invited to Nexus</h1>
      <p style="margin:0 0 16px; line-height:1.5;">
        {{.InviterEmail}} has invited you to join their Nexus workspace. Click the button below
        to accept the invite and set your password. This link is personal —
        please don't share it.
      </p>
      <p style="margin:24px 0;">
        <a href="{{.URL}}"
           style="display:inline-block; background:linear-gradient(135deg,#5a4cff,#8a4cff); color:#ffffff; text-decoration:none; font-weight:600; padding:12px 20px; border-radius:8px;">
          Accept invite
        </a>
      </p>
      <p style="margin:24px 0 8px; font-size:12px; color:#6b6b78;">
        Or paste this URL into your browser:
      </p>
      <p style="margin:0 0 24px; font-size:12px; color:#1f1f24; word-break:break-all; background:#f6f6f9; padding:10px 12px; border-radius:6px;">
        {{.URL}}
      </p>
      <hr style="border:none; border-top:1px solid #e6e6ee; margin:24px 0;"/>
      <p style="margin:0; font-size:11px; color:#a0a0ac; line-height:1.5;">
        Role on accept: <strong>{{.Role}}</strong>.<br/>
        If you weren't expecting this email you can safely ignore it — the
        invite will expire automatically.
      </p>
    </div>
  </body>
</html>`))

// inviteModel is the typed bag passed to inviteTemplate. Keeping the
// fields named blocks a future typed-rows feature (an org-admin chooses a
// row of substitution values to highlight in the body) from needing to
// re-derive field names by index. New fields belong here.
type inviteModel struct {
	InviterEmail string
	URL          string
	Role         string
}

// renderInviteHTML executes inviteTemplate against the model.
//
// Errors here indicate the template key is wrong and would have silently
// shipped "" or "<no value>" in the old fmt.Sprintf implementation;
// html/template Parse above guarantees the keys are stable, so failure
// mode is "constant panic at boot" rather than "blank invite in the
// inbox". That is the trade we want — a forbidden value is louder than
// a substituted empty string.
func renderInviteHTML(inviteURL, inviterEmail, role string) (string, error) {
	var buf bytes.Buffer
	if err := inviteTemplate.Execute(&buf, inviteModel{
		InviterEmail: inviterEmail,
		URL:          inviteURL,
		Role:         role,
	}); err != nil {
		return "", fmt.Errorf("invite template: %w", err)
	}
	return buf.String(), nil
}
