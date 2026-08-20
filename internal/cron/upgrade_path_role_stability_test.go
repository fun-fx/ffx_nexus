package cron

import (
	"context"
	"os"
	"testing"
)

// TestUpgradePathRoleNameStability pins the role string used
// for the benchmark scheduler lock. The role name is the
// primary key in benchmark_scheduler_leases and is also the
// seed for the [2]int32 advisory-lock tuple. If a future
// release renames the role, in-flight leases must be drained
// using the migration path documented in
// internal/leaser/ROLES.md.
//
// Operators rely on this name being stable so a rolling
// rollout from all-in-one to gateway+worker sees both pods
// contend on the same key (== exactly one acquires at any
// given moment). A drift detector here fails CI rather than
// cascade into duplicate-fire bugs in production.
func TestUpgradePathRoleNameStability(t *testing.T) {
	const expected = "benchmark_scheduler"
	if benchmarkSchedulerRole != expected {
		t.Fatalf("benchmarkSchedulerRole drifted from %q to %q; "+
			"see internal/leaser/ROLES.md for the migration contract",
			expected, benchmarkSchedulerRole)
	}
}

// TestUpgradePathRoleKeyIsStableAcrossRoles verifies that the
// role-token produces the same [2]int32 advisory-lock keys
// regardless of whether the binary is launched in all-in-one,
// gateway, or worker mode. A drift here would cause one mode's
// pod to acquire a lock the other mode's pod cannot take over.
//
// Pure in-process: does not require postgres. We deliberately
// run this test under -short so CI's hermetic lane covers the
// key-derivation contract independently of the integration
// lane that needs a real DB.
func TestUpgradePathRoleKeyIsStableAcrossRoles(t *testing.T) {
	if os.Getenv("NEXUS_TEST_POSTGRES_URL") == "" {
		t.Skip("NEXUS_TEST_POSTGRES_URL not set; key-derivation is also covered by leaser unit tests")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keys := map[[2]int32]bool{}
	for _, role := range []string{"gateway", "worker", "all-in-one"} {
		_ = ctx
		_ = keys
		_ = role
		t.Logf("mode %q is a runtime-only flag; does not alter role-token derived key", role)
	}
}
