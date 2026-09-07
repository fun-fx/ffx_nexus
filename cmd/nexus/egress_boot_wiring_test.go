package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/egress"
)

// These tests wire the boot path (installEgressGuard) to the
// three operational modes (proxy / in_cluster_only / direct)
// and assert what the resulting Guard does at request time.
//
// The point is not to retest the egress internals — those live
// in internal/egress and are well-covered there — but to
// guarantee the env-var → Policy mapping in boot survives
// future refactors.
//
// TestMain (egress_testmain_test.go) has already turned the
// process's default dial policy into "allow loopback" so this
// package's tests can talk to a local server without tripping
// the static-IP denial of private ranges.

// TestInstallEgressGuard_ProxyMode: mode=proxy + HTTPS_PROXY
// keeps the static IP policy and is observed by CheckURL.
func TestInstallEgressGuard_ProxyMode(t *testing.T) {
	t.Setenv("NEXUS_EGRESS_MODE", "proxy")
	t.Setenv("HTTPS_PROXY", "http://proxy.x.svc:3128")
	t.Setenv("NEXUS_EGRESS_INTERNAL_HOSTS", "")
	t.Setenv("NEXUS_EGRESS_TENANT_ALLOWED_CIDRS", "")

	egress.TestingAllowLoopback(t)
	installEgressGuard(makeCfg(), quietLog())

	g := egress.Default()
	if err := g.CheckURL(context.Background(),
		"http://api.anthropic.com/v1/messages", egress.Tenant); err != nil {
		t.Fatalf("public IP refused in proxy mode: %v", err)
	}

	if err := g.CheckURL(context.Background(),
		"http://169.254.169.254/", egress.Tenant); err == nil {
		t.Fatal("metadata IP accepted in proxy mode")
	} else if !errors.Is(err, egress.ErrBlockedDestination) {
		t.Fatalf("wrong error: %v", err)
	}
}

// TestInstallEgressGuard_InClusterOnly asserts that mode=in_cluster_only
// with a comma list sets PublicDestinationsBlocked AND mirrors the
// list on AllowedInternalHosts verbatim. Whitespace and empty
// entries are dropped, but order is preserved for the operator's
// view.
//
// We avoid DNS in this test because the egress guard's
// net.DefaultResolver is a runtime-resolved dependency the
// CI image does not control. The FQDN-allow-list semantic is
// exercised exhaustively in internal/egress/mode_proxy_test.go,
// where the resolver is replaced with a stub. This test
// only verifies the env-var → Policy wiring.
func TestInstallEgressGuard_InClusterOnly(t *testing.T) {
	t.Setenv("NEXUS_EGRESS_MODE", "in_cluster_only")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NEXUS_EGRESS_INTERNAL_HOSTS",
		"vllm.models.svc.cluster.local:8000, ,embed.internal.svc.cluster.local, ")
	t.Setenv("NEXUS_EGRESS_TENANT_ALLOWED_CIDRS", "")

	egress.TestingAllowLoopback(t)
	installEgressGuard(makeCfg(), quietLog())

	g := egress.Default()
	p := g.Policy()
	if !p.PublicDestinationsBlocked {
		t.Fatal("in_cluster_only did not set PublicDestinationsBlocked")
	}
	if len(p.AllowedInternalHosts) != 2 {
		t.Fatalf("AllowedInternalHosts has %d entries, expected 2: %v",
			len(p.AllowedInternalHosts), p.AllowedInternalHosts)
	}
	want := []string{
		"vllm.models.svc.cluster.local:8000",
		"embed.internal.svc.cluster.local",
	}
	for i, w := range want {
		if p.AllowedInternalHosts[i] != w {
			t.Fatalf("AllowedInternalHosts[%d]=%q want=%q", i,
				p.AllowedInternalHosts[i], w)
		}
	}
	if p.ProxyURL != "" {
		t.Fatalf("in_cluster_only unexpectedly set ProxyURL=%q", p.ProxyURL)
	}
}

// TestInstallEgressGuard_DirectMode: untagged mode matches
// legacy behaviour. A public IP passes via the package's
// loopback-relaxed policy.
func TestInstallEgressGuard_DirectMode(t *testing.T) {
	t.Setenv("NEXUS_EGRESS_MODE", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NEXUS_EGRESS_INTERNAL_HOSTS", "")
	t.Setenv("NEXUS_EGRESS_TENANT_ALLOWED_CIDRS", "")

	egress.TestingAllowLoopback(t)
	installEgressGuard(makeCfg(), quietLog())

	g := egress.Default()
	if err := g.CheckURL(context.Background(),
		"http://1.1.1.1/", egress.Tenant); err != nil {
		t.Fatalf("public IP refused in direct mode: %v", err)
	}
}

// TestInstallEgressGuard_UnknownMode falls back to direct
// mode and logs an error. The chart's render gate refuses
// this combination; the boot path's job is to fail safe.
func TestInstallEgressGuard_UnknownMode(t *testing.T) {
	t.Setenv("NEXUS_EGRESS_MODE", "not_a_real_mode")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NEXUS_EGRESS_INTERNAL_HOSTS", "")
	t.Setenv("NEXUS_EGRESS_TENANT_ALLOWED_CIDRS", "")

	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	egress.TestingAllowLoopback(t)
	installEgressGuard(makeCfg(), logger)

	g := egress.Default()
	if err := g.CheckURL(context.Background(),
		"http://1.1.1.1/", egress.Tenant); err != nil {
		t.Fatalf("unknown mode fell back to direct but refused: %v", err)
	}
	if !strings.Contains(logBuf.String(), "unrecognised") {
		t.Fatalf("unknown mode not logged: %q", logBuf.String())
	}
}

// TestInstallEgressGuard_ProxyWithoutURL refuses to silently
// enter proxy mode when an operator typo'd the mode or
// otherwise forgot HTTPS_PROXY. It logs at error level, the
// runtime falls back to direct, and the boot does not crash.
func TestInstallEgressGuard_ProxyWithoutURL(t *testing.T) {
	t.Setenv("NEXUS_EGRESS_MODE", "proxy")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NEXUS_EGRESS_INTERNAL_HOSTS", "")
	t.Setenv("NEXUS_EGRESS_TENANT_ALLOWED_CIDRS", "")

	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	egress.TestingAllowLoopback(t)
	installEgressGuard(makeCfg(), logger)

	g := egress.Default()
	if err := g.CheckURL(context.Background(),
		"http://1.1.1.1/", egress.Tenant); err != nil {
		t.Fatalf("proxy mode without URL fell back to direct but refused: %v", err)
	}
	if !strings.Contains(logBuf.String(), "HTTPS_PROXY is empty") {
		t.Fatalf("expected an error log stating HTTPS_PROXY is empty; got %q",
			logBuf.String())
	}
}

// TestInstallEgressGuard_TenantCIDRsAreLoaded: widening via
// NEXUS_EGRESS_TENANT_ALLOWED_CIDRs reaches the Guard.
func TestInstallEgressGuard_TenantCIDRsAreLoaded(t *testing.T) {
	t.Setenv("NEXUS_EGRESS_MODE", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NEXUS_EGRESS_INTERNAL_HOSTS", "")
	t.Setenv("NEXUS_EGRESS_TENANT_ALLOWED_CIDRS", "10.7.0.0/16")

	egress.TestingAllowLoopback(t)
	installEgressGuard(makeCfg(), quietLog())

	g := egress.Default()
	if err := g.CheckURL(context.Background(),
		"http://10.7.5.7/", egress.Tenant); err != nil {
		t.Fatalf("tenant CIDR widening did not reach the Guard: %v", err)
	}
}

// makeCfg builds a config.Config with only the egress fields.
// Other fields stay zero-valued, which is enough for boot,
// which reads only Egress* fields.
func makeCfg() config.Config {
	return config.Config{
		EgressMode:               os.Getenv("NEXUS_EGRESS_MODE"),
		EgressProxyURL:           os.Getenv("HTTPS_PROXY"),
		EgressInternalHosts:      os.Getenv("NEXUS_EGRESS_INTERNAL_HOSTS"),
		EgressTenantAllowedCIDRs: os.Getenv("NEXUS_EGRESS_TENANT_ALLOWED_CIDRS"),
	}
}

// quietLog is a sink that swallows all output; tests that
// exercise the boot path for behaviour assertions do not need
// log lines on stdout/stderr.
func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
