-- 023_scheduler_leases.sql
--
-- Durable scheduler lock for Phase D-1. Each role is identified
-- by either a global role name ("benchmark_scheduler") or a
-- per-schedule id. The leases table is the visible/audit plane;
-- pg_try_advisory_lock(int4, int4, role) is the actual mutual
-- exclusion that holds for the lock lifetime, and is anchored
-- on a pgxpool.Conn that the Manager pins until Release().
--
-- Either layer alone is insufficient: advisory locks vanish
-- with the connection, which means a failing pgxpool connection
-- can silently drop leadership while still selecting due runs.
-- The lease heartbeat lets a new pod take over even if the
-- old leader's connection leaked.
--
-- The TTL is short (15s) so crashed workers lose the lease quickly;
-- the renew interval is half of TTL (7s) so a single missed renew
-- does not cause drift but two consecutive misses do.

CREATE TABLE IF NOT EXISTS benchmark_scheduler_leases (
    role           TEXT        PRIMARY KEY,
    owner_id       TEXT        NOT NULL,
    acquired_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    heartbeat_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at     TIMESTAMPTZ NOT NULL,
    lock_token     TEXT        NOT NULL,
    CONSTRAINT benchmark_scheduler_leases_ttl CHECK (
        expires_at > heartbeat_at AND expires_at <= heartbeat_at + INTERVAL '60 seconds'
    )
);

-- Index used by the take-over query: when picking up an expired
-- lease, the worker does
--   UPDATE ... SET owner_id = $new, ... WHERE role = $r AND expires_at < NOW()
-- RETURNING lock_token
-- The row-level lock on the matched row is the source of single-leader
-- correctness; the index keeps the WHERE cheap.
CREATE INDEX IF NOT EXISTS benchmark_scheduler_leases_expires_idx
    ON benchmark_scheduler_leases (role, expires_at);

COMMENT ON TABLE benchmark_scheduler_leases IS
    'Durable single-leader leases for background workers. Postgres is the only source of truth for ownership; advisory locks are an advisory fast path on top.';
