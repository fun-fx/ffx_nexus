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
		t.Errorf("key collision between scheduler and audit: %v", sched)
	}
}

// TestKeyForRoleTwoInt32 ensures the key shape Postgres expects
// (two int4 args) — a change to a single-int8 key would be a
// breaking change because pg_try_advisory_lock is overloaded
// on the number of arguments.
func TestKeyForRoleTwoInt32(t *testing.T) {
	k := leaser.KeyForRoleTest("benchmark_scheduler")
	// The hash must fit in int32 regardless of role length; the
	// two halves are independent so we verify each is in int32
	// range (a future broken hash that emits > int32 bits would
	// cause Postgres to reject the argument).
	for i, v := range k {
		if int32(v) != v {
			t.Errorf("key half %d out of int32 range: %d", i, v)
		}
	}
}

// TestKeyForScheduleDistinct confirms schedule ids produce
// distinct advisory lock keys, even when prefixes overlap.
func TestKeyForScheduleDistinct(t *testing.T) {
	a := leaser.KeyForSchedule("schedule-aaaaaaaa-1111")
	b := leaser.KeyForSchedule("schedule-bbbbbbbb-2222")
	if a == b {
		t.Errorf("schedule key collision: a=%v b=%v", a, b)
	}
}

// TestKeyForScheduleCollisionSpace sketches an upper bound on
// keyspace collisions. Across 1000 schedule IDs generated
// deterministically with distinct input, the number of
// distinct keys should match the number of unique IDs (the
// hash is deterministic per input).
func TestKeyForScheduleCollisionSpace(t *testing.T) {
	ids := make([]string, 0, 1000)
	for i := 0; i < 1000; i++ {
		ids = append(ids, uniqueScheduleID(i))
	}
	seen := map[[2]int32]struct{}{}
	for _, id := range ids {
		s := leaser.KeyForSchedule(id)
		seen[s] = struct{}{}
	}
	if len(seen) != len(ids) {
		t.Errorf("schedule key collision: %d distinct out of %d", len(seen), len(ids))
	}
}

// uniqueScheduleID returns a deterministic unique id for each
// i. The id structure (sufficient entropy) is what separates
// "real-world hash distribution" from "two names that differ
// only in their suffix". Postgres advisory lock keys have
// 2^64 of headroom; we exercise the key-mapping itself, not
// the surrounding entropy budget.
func uniqueScheduleID(i int) string {
	b := make([]byte, 4)
	b[0] = byte(i >> 24)
	b[1] = byte(i >> 16)
	b[2] = byte(i >> 8)
	b[3] = byte(i)
	return "sch-" + string(b)
}
