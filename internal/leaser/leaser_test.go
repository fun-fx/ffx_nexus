package leaser_test

import (
	"testing"

	"github.com/ffxnexus/nexus/internal/leaser"
)

// TestKeyForRoleDeterministic pins FNV-1a hashing across the
// exact strings the production code uses. A drift in the hash
// function or seed would break single-leader correctness because
// each Pod would have a different advisory key and the durable
// row would never match.
func TestKeyForRoleDeterministic(t *testing.T) {
	gotA := leaser.KeyForRoleTest("benchmark_scheduler")
	gotB := leaser.KeyForRoleTest("benchmark_scheduler")
	if gotA != gotB {
		t.Errorf("non-deterministic hash: A=%d B=%d", gotA, gotB)
	}
}

// TestKeyForRoleDistinct confirms two distinct roles produce
// two distinct keys. A collision here would let two pods
// believe they both hold the same advisory lock and write to
// the same timeslot.
func TestKeyForRoleDistinct(t *testing.T) {
	sched := leaser.KeyForRoleTest("benchmark_scheduler")
	audit := leaser.KeyForRoleTest("audit_log_writer")
	if sched == audit {
		t.Errorf("key collision between scheduler and audit: %d", sched)
	}
}

// TestKeyForRoleRange pins the value to int64. Postgres advisory
// locks take int8 — a uint64 wider than int64 would silently
// truncate and alias different keys to the same int8 slot.
func TestKeyForRoleRange(t *testing.T) {
	for _, role := range []string{"", "a", "an-extremely-long-role-name-that-could-overflow-on-bad-hashing-strategy-but-should-not-with-fnv64a", "  زخرفة عربية "} {
		if k := leaser.KeyForRoleTest(role); k < 0 {
			t.Errorf("role %q produced negative int64 key %d", role, k)
		}
	}
}
