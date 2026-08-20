ROLES.md — Phase D-1 lock role name stability
===============================================

Phase D-1 separates the binary into three roles (gateway, worker,
all-in-one). All three share the same scheduler lock role name:

  Role name token: "benchmark_scheduler"
  Source of truth: internal/cron/leader_gate.go

The two-int32 lock keys are derived from this token. If a future
release renames the role, in-flight leases must be drained or
the takeover window opens; this document is the contract for the
upgrader.

Why a stable role name
----------------------

1. Before Phase D-1, the binary was all-in-one and acquired the
   role "benchmark_scheduler". The lease table's PRIMARY KEY is
   `role`. A role-name change is a logical primary-key change.

2. During Phase D-1's rolling rollout, BOTH the all-in-one pod
   and the new worker pod contend for the SAME role. Same key
   => at most one acquires the row at any time => no duplicate
   fires.

3. After the all-in-one pod is removed from the cluster, the
   worker pods keep acquiring the same role. No code change is
   required on the worker side besides the role-aware
   `AcquireSchedule` invocation.

If a future release ever needs to change the role name
---------------------------------------------------------

The new role string MUST be different from the old at the
caller site, but the migration path is:

  a. Drain the cluster: stop new benchmark runs.
  b. Apply migration XXX (rename primary key, e.g. via a new
     migration that re-keys the row).
  c. Roll the worker pod with the new role name.
  d. Confirm leases acquired under the new role.

Any deviation from this order risks losing single-leader
correctness during the rollout window. CI's leaser integration
test (`internal/leaser/integration_test.go`) is the gate.
