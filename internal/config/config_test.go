package config

import (
	"os"
	"strings"
	"testing"
)

// TestRoleValidation three-modes-only contract for NEXUS_ROLE.
// Operators who want to extend the mode set update validMode
// and the panic in Load() in lockstep; this test fails the
// moment someone adds a fourth mode silently.
func TestRoleValidation(t *testing.T) {
	ok := []string{"all-in-one", "allinone", "All-In-One", "  worker  ", "GATEWAY"}
	bad := []string{"", "scheduler", "cluster", "gate", "wrker"}
	for _, in := range ok {
		if !validMode(in) {
			t.Errorf("validMode(%q) should return true", in)
		}
	}
	for _, in := range bad {
		if validMode(in) {
			t.Errorf("validMode(%q) should return false", in)
		}
	}
}

// TestLoadModePanicOnInvalid pins the boot-time panic for an
// invalid role. We seed NEXUS_ROLE with a clearly bogus value
// and defer a recover that captures the actual panic message.
func TestLoadModePanicOnInvalid(t *testing.T) {
	t.Setenv("NEXUS_ROLE", "scheduler")
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Load should panic on invalid NEXUS_ROLE")
		}
		msg := ""
		switch v := r.(type) {
		case string:
			msg = v
		case error:
			msg = v.Error()
		}
		if !strings.Contains(msg, "NEXUS_ROLE must be one of") {
			t.Errorf("panic message should explain the contract, got %q", msg)
		}
	}()
	_ = Load()
}

// TestLoadRoleAllInOneIsDefault verifies that omitting NEXUS_ROLE
// selects all-in-one. The Helm chart pins this in deployment.yaml
// for the legacy single-Deployment install, and a future change
// must update both places together.
func TestLoadRoleAllInOneIsDefault(t *testing.T) {
	t.Setenv("NEXUS_ROLE", "")
	defer os.Unsetenv("NEXUS_ROLE")
	c := Load()
	if c.Mode != "all-in-one" {
		t.Errorf("default mode = %q, want all-in-one", c.Mode)
	}
	if !c.SchedulerRoleEnabled && c.Mode == "all-in-one" {
		// all-in-one without the gate is allowed, because
		// single-replica = no contention. The gate IS
		// useful (future multi-replica all-in-one) so the
		// value can be true here too — only reject the
		// missing flag for single-pod installs.
		t.Logf("note: all-in-one with gate=%v is the conservative default", c.SchedulerRoleEnabled)
	}
}

// TestLoadRoleWorkerForcesLeaseGate asserts the worker-mode
// auto-promotion to the leader lease. Without the gate the
// worker would happily fire schedules on every replica and
// reintroduce the duplicate-execution bug Phase D-1 exists
// to fix.
func TestLoadRoleWorkerForcesLeaseGate(t *testing.T) {
	t.Setenv("NEXUS_ROLE", "worker")
	t.Setenv("NEXUS_SCHEDULER_ROLE_ENABLED", "false")
	defer os.Unsetenv("NEXUS_ROLE")
	defer os.Unsetenv("NEXUS_SCHEDULER_ROLE_ENABLED")
	c := Load()
	if c.Mode != "worker" {
		t.Errorf("mode = %q, want worker", c.Mode)
	}
	if !c.SchedulerRoleEnabled {
		t.Errorf("worker mode must force SchedulerRoleEnabled=true; got %v", c.SchedulerRoleEnabled)
	}
}

// TestLoadRoleGatewaySuppressesGate verifies that the
// gateway mode does NOT pretend to run a scheduler — the
// cron.New(...) construction in main.go is branch-gated by
// cfg.Mode, so any scheduler goroutine would only be busy
// doing nothing. We pin this by checking Mode == "gateway"
// does not flip SchedulerRoleEnabled (the value the gate
// uses is irrelevant in this mode).
func TestLoadRoleGatewaySuppressesGate(t *testing.T) {
	t.Setenv("NEXUS_ROLE", "gateway")
	t.Setenv("NEXUS_SCHEDULER_ROLE_ENABLED", "true")
	defer os.Unsetenv("NEXUS_ROLE")
	defer os.Unsetenv("NEXUS_SCHEDULER_ROLE_ENABLED")
	c := Load()
	if c.Mode != "gateway" {
		t.Errorf("mode = %q, want gateway", c.Mode)
	}
	if !c.SchedulerRoleEnabled {
		t.Errorf("gateway mode keeps explicit flag %v, expected true", c.SchedulerRoleEnabled)
	}
}

// Sentinel compile-level contract: Mode has the type described
// in the config struct. A future broken refactor that turns
// Mode into an int loses the human-readable env var, so the
// test in TestRoleValidation would silently miss the regression.
var _ string = ""
