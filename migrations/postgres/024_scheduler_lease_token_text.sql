-- 024 — promote benchmark_scheduler_leases.lock_token from BIGINT to TEXT.
--
-- The Phase D-1 leaser stores a 128-bit hex token (random uint64 packed
-- with FNV-64a owner-id headroom) so a follower cannot produce an
-- indistinguishable token. BIGINT signed 64-bit fits 63 useful bits and
-- would obscure the private-owner partition; TEXT carries the token as
-- a stable opaque identifier the audit layer can surface without
-- re-parsing Postgres-specific types.
--
-- This migration is the canonical seam: an applied migration must never
-- be edited on disk (migrate ledger pins SHA256). The change that
-- motivated this migration touched the leaser package to write a
-- randomised token; adding a 024 keeps the ledger linear.

ALTER TABLE benchmark_scheduler_leases
    ALTER COLUMN lock_token TYPE TEXT USING lock_token::TEXT;
