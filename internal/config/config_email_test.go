package config

import (
	"os"
	"strings"
	"testing"
)

// stubArgs carries the per-test environment-variable override set. Tests
// pollute os.Setenv; a t.Cleanup keeps each test isolated so a parallel
// run can't observe another case's values.
//
// Although load() reads the env each time it runs, the test should
// explicitly unset to the default if it didn't set — otherwise an
// inherited NEXUS_RESEND_API_KEY (from a CI machine) would mask the
// case under test.
type stubArgs struct {
	cleanup []func()
}

func (a *stubArgs) setenv(k, v string) {
	prev, had := os.LookupEnv(k)
	if err := os.Setenv(k, v); err != nil {
		a.cleanup = append(a.cleanup, func() {})
		return
	}
	a.cleanup = append(a.cleanup, func() {
		if had {
			_ = os.Setenv(k, prev)
		} else {
			_ = os.Unsetenv(k)
		}
	})
}

func (a *stubArgs) teardown() {
	for _, c := range a.cleanup {
		c()
	}
}

// TestEmailModeAutoPick_SMTP asserts auto-detection resolves to SMTP
// when SMTPHost is set and ResendAPIKey is not, regardless of the
// operator leaving NEXUS_EMAIL_PROVIDER empty.
func TestEmailModeAutoPick_SMTP(t *testing.T) {
	a := &stubArgs{}
	defer a.teardown()
	a.setenv("NEXUS_EMAIL_PROVIDER", "")
	a.setenv("NEXUS_SMTP_HOST", "mail.example")
	a.setenv("NEXUS_RESEND_API_KEY", "")
	a.setenv("NEXUS_EMAIL_FROM_ADDRESS", "Nexus <noreply@example.com>")
	cfg := load()
	mode, _, _, err := cfg.EmailMode()
	if err != nil {
		t.Fatalf("EmailMode: %v", err)
	}
	if mode != EmailModeSMTP {
		t.Fatalf("mode=%q, want smtp", mode)
	}
}

// TestEmailModeAutoPick_Resend asserts auto-detection resolves to
// Resend when only ResendAPIKey is set.
func TestEmailModeAutoPick_Resend(t *testing.T) {
	a := &stubArgs{}
	defer a.teardown()
	a.setenv("NEXUS_EMAIL_PROVIDER", "")
	a.setenv("NEXUS_SMTP_HOST", "")
	a.setenv("NEXUS_RESEND_API_KEY", "re_test_xxx")
	a.setenv("NEXUS_EMAIL_FROM_ADDRESS", "Nexus <noreply@example.com>")
	cfg := load()
	mode, _, _, err := cfg.EmailMode()
	if err != nil {
		t.Fatalf("EmailMode: %v", err)
	}
	if mode != EmailModeResend {
		t.Fatalf("mode=%q, want resend", mode)
	}
}

// TestEmailModeExplicitMatchesConstant asserts the named providers map
// to the closed set the boot expects.
func TestEmailModeExplicitMatchesConstant(t *testing.T) {
	a := &stubArgs{}
	defer a.teardown()
	a.setenv("NEXUS_EMAIL_PROVIDER", "smtp")
	a.setenv("NEXUS_SMTP_HOST", "m.example")
	a.setenv("NEXUS_EMAIL_FROM_ADDRESS", "a@b.example")
	cfg := load()
	mode, _, _, err := cfg.EmailMode()
	if err != nil {
		t.Fatalf("EmailMode(smtp): %v", err)
	}
	if mode != EmailModeSMTP {
		t.Fatalf("smtp mode expected: got %q", mode)
	}
}

// TestEmailModeUnknownProvider asserts the closed-set error fires for
// anything outside resend|smtp|noop so the boot fails fast.
func TestEmailModeUnknownProvider(t *testing.T) {
	a := &stubArgs{}
	defer a.teardown()
	a.setenv("NEXUS_EMAIL_PROVIDER", "ses")
	cfg := load()
	_, _, _, err := cfg.EmailMode()
	if err == nil {
		t.Fatalf("expected error for unknown provider")
	}
	if !strings.Contains(err.Error(), "unknown NEXUS_EMAIL_PROVIDER") {
		t.Fatalf("error formatter should name the env var: %v", err)
	}
}

// TestFromAddressResolution asserts the canonical resolver picks the
// new var first and falls back to the deprecated alias.
func TestFromAddressResolution(t *testing.T) {
	a := &stubArgs{}
	defer a.teardown()
	// Both unset -> "".
	// load() does NOT touch .env; it reads the environment directly. By
// going through load() instead of the public Load(), the tests are not
// contaminated by a developer's .env file — which is set as a hard
// invariant in this package, since a leaked .env would silently win over
// the test inputs and produce a green-light failure mode.
	a.setenv("NEXUS_EMAIL_FROM_ADDRESS", "")
	a.setenv("NEXUS_RESEND_FROM_ADDRESS", "")
	cfg := load()
	if cfg.EmailFromAddress != "" {
		t.Fatalf("FromAddress from-empty: got %q", cfg.EmailFromAddress)
	}
	// Only new var -> new var.
	a.setenv("NEXUS_EMAIL_FROM_ADDRESS", "new@b.example")
	a.setenv("NEXUS_RESEND_FROM_ADDRESS", "")
	cfg = load()
	if cfg.EmailFromAddress != "new@b.example" {
		t.Fatalf("FromAddress: want new, got %q", cfg.EmailFromAddress)
	}
	// Both set -> new wins.
	a.setenv("NEXUS_EMAIL_FROM_ADDRESS", "new@b.example")
	a.setenv("NEXUS_RESEND_FROM_ADDRESS", "old@b.example")
	cfg = load()
	if cfg.EmailFromAddress != "new@b.example" {
		t.Fatalf("FromAddress: want new wins, got %q", cfg.EmailFromAddress)
	}
	// Only old var -> old is the deprecated fallback.
	a.setenv("NEXUS_EMAIL_FROM_ADDRESS", "")
	a.setenv("NEXUS_RESEND_FROM_ADDRESS", "old@b.example")
	cfg = load()
	if cfg.EmailFromAddress != "old@b.example" {
		t.Fatalf("FromAddress: want old fallback, got %q", cfg.EmailFromAddress)
	}
}

// TestEmailModeMissingFromRefuses asserts the closed contract: a
// configured transport without a From address fails EmailMode so the
// boot path refuses quietly-drop invitations.
func TestEmailModeMissingFromRefuses(t *testing.T) {
	a := &stubArgs{}
	defer a.teardown()
	a.setenv("NEXUS_EMAIL_PROVIDER", "smtp")
	a.setenv("NEXUS_SMTP_HOST", "m.example")
	a.setenv("NEXUS_EMAIL_FROM_ADDRESS", "")
	a.setenv("NEXUS_RESEND_FROM_ADDRESS", "")
	cfg := load()
	_, _, _, err := cfg.EmailMode()
	if err == nil {
		t.Fatalf("expected error: From address missing")
	}
	if !strings.Contains(err.Error(), "NEXUS_EMAIL_FROM_ADDRESS") {
		t.Fatalf("error should name the env var: %v", err)
	}
}
