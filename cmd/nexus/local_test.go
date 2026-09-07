package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestParseServeFlags(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantLocal bool
		wantHelp  bool
		wantErr   bool
	}{
		{name: "no args", args: nil},
		{name: "long form", args: []string{"--local"}, wantLocal: true},
		{name: "short form", args: []string{"-local"}, wantLocal: true},
		{name: "help", args: []string{"--help"}, wantHelp: true},
		// The point of rejecting: a typo must not start a gateway with no
		// control plane and no complaint.
		{name: "typo", args: []string{"--locl"}, wantErr: true},
		{name: "env var passed as flag", args: []string{"--NEXUS_LOCAL_DB=true"}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			local, help, err := parseServeFlags(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseServeFlags(%q) accepted an unknown flag", tc.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseServeFlags(%q): %v", tc.args, err)
			}
			if local != tc.wantLocal || help != tc.wantHelp {
				t.Fatalf("parseServeFlags(%q) = (%v, %v), want (%v, %v)",
					tc.args, local, help, tc.wantLocal, tc.wantHelp)
			}
		})
	}
}

// Setting both means the operator asked for two different control planes.
// Picking one silently is how you end up writing keys into a database you
// forgot exists, so this must refuse — and it must refuse before creating any
// state on disk.
func TestStartLocalModeRefusesConfiguredPostgres(t *testing.T) {
	cfg := config.Config{
		PostgresURL:   "postgres://nexus:nexus@db.internal:5432/nexus",
		LocalStateDir: t.TempDir(),
	}

	db, err := startLocalMode(&cfg, discardLogger())
	if err == nil {
		_ = db.Stop()
		t.Fatal("startLocalMode started a second database alongside NEXUS_POSTGRES_URL")
	}
	if !strings.Contains(err.Error(), "NEXUS_POSTGRES_URL") {
		t.Fatalf("error does not name the conflicting setting: %v", err)
	}
}

// DevMode without the matching cookie change is a console that accepts a
// password and then bounces back to the login form, because the browser never
// returns a Secure cookie over http.
func TestApplyLocalModeRelaxesCookiesForPlainHTTP(t *testing.T) {
	cfg := config.Config{SecureCookies: true}
	cfg.ApplyLocalMode("postgres://nexus:nexus@127.0.0.1:15432/nexus?sslmode=disable")

	if !cfg.LocalDB || !cfg.AutoMigrate || !cfg.DevMode {
		t.Fatalf("ApplyLocalMode left the mode half-applied: %+v",
			struct{ LocalDB, AutoMigrate, DevMode bool }{cfg.LocalDB, cfg.AutoMigrate, cfg.DevMode})
	}
	if cfg.SecureCookies {
		t.Fatal("ApplyLocalMode kept Secure-only cookies on a plain-HTTP console")
	}
	if !cfg.AllowSignup {
		t.Fatal("ApplyLocalMode left no way to create the first account")
	}
}

func TestApplyLocalModeKeepsBootstrapAdminOverSignup(t *testing.T) {
	cfg := config.Config{AdminEmail: "admin@example.com", AdminPassword: "hunter2hunter2"}
	cfg.ApplyLocalMode("postgres://nexus:nexus@127.0.0.1:15432/nexus?sslmode=disable")

	if cfg.AllowSignup {
		t.Fatal("ApplyLocalMode opened public signup even though a bootstrap admin is configured")
	}
}

func TestApplyLocalModeHonoursExplicitSecureCookies(t *testing.T) {
	t.Setenv("NEXUS_SECURE_COOKIES", "true")

	cfg := config.Config{}
	cfg.ApplyLocalMode("postgres://nexus:nexus@127.0.0.1:15432/nexus?sslmode=disable")

	if !cfg.SecureCookies {
		t.Fatal("ApplyLocalMode overrode an explicit NEXUS_SECURE_COOKIES=true")
	}
}
