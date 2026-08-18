package migrate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// advisoryLockKey is the key passed to pg_advisory_lock. Advisory locks live in
// a single global namespace per database, so the value only needs to be stable
// and unlikely to collide with another application's choice. It is a fixed
// literal rather than a hash of a string so that an operator debugging a stuck
// migration can find it directly:
//
//	SELECT * FROM pg_locks WHERE locktype = 'advisory' AND objid = 4242042001;
const advisoryLockKey int64 = 4242042001

// pgExecutor applies migrations to Postgres.
type pgExecutor struct {
	pool *pgxpool.Pool
	// lockConn is the connection holding the advisory lock. Advisory locks are
	// session-scoped, so the lock must be taken and released on ONE connection.
	// Taking it from the pool at large would release it as soon as an unrelated
	// query returned that connection.
	lockConn *pgxpool.Conn
	version  string
}

// NewPostgres returns an Executor for a Postgres pool. version is recorded in
// the ledger so an operator can tell which build applied a given migration.
func NewPostgres(pool *pgxpool.Pool, version string) Executor {
	return &pgExecutor{pool: pool, version: version}
}

func (e *pgExecutor) Engine() Engine { return EnginePostgres }

// EnsureLedger creates the ledger table if it is absent.
//
// `CREATE TABLE IF NOT EXISTS` is NOT race-free in Postgres. The existence
// check and the creation are not atomic, so two sessions running it at the same
// moment can both pass the check and then collide in the system catalogue,
// surfacing as a unique-violation on pg_type_typname_nsp_index (23505) or
// duplicate_table (42P07) rather than the "already exists, carry on" the syntax
// suggests. That is exactly what three replicas starting together do, and it is
// why this is tolerated rather than propagated.
//
// The migrations themselves are protected differently: they run while holding
// the advisory lock, so only one session issues their DDL at a time. Only this
// bootstrap statement can race, because the ledger has to exist before there is
// anything to lock around for the read-only Pending path.
func (e *pgExecutor) EnsureLedger(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS ` + LedgerTable + ` (
    id            TEXT PRIMARY KEY,
    checksum      TEXT        NOT NULL,
    applied_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    duration_ms   BIGINT      NOT NULL DEFAULT 0,
    success       BOOLEAN     NOT NULL DEFAULT TRUE,
    error         TEXT        NOT NULL DEFAULT '',
    nexus_version TEXT        NOT NULL DEFAULT ''
);`
	_, err := e.pool.Exec(ctx, ddl)
	if err == nil || isDuplicateObject(err) {
		return nil
	}
	return err
}

// isDuplicateObject reports whether err means "another session created this
// object concurrently", which for an IF NOT EXISTS statement is success.
func isDuplicateObject(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "23505", // unique_violation, typically on a pg_catalog index
		"42P07", // duplicate_table
		"42710": // duplicate_object
		return true
	}
	return false
}

func (e *pgExecutor) Applied(ctx context.Context) (map[string]LedgerEntry, error) {
	rows, err := e.pool.Query(ctx,
		`SELECT id, checksum, applied_at, success FROM `+LedgerTable+` WHERE success`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]LedgerEntry{}
	for rows.Next() {
		var le LedgerEntry
		if err := rows.Scan(&le.ID, &le.Checksum, &le.AppliedAt, &le.Success); err != nil {
			return nil, err
		}
		out[le.ID] = le
	}
	return out, rows.Err()
}

// Apply runs the migration and records it in ONE transaction. Either the schema
// change and its ledger row both land, or neither does; there is no state where
// a migration ran but was not recorded (which would make the next run skip it
// having only half-applied) or was recorded but did not run.
//
// This is safe for every migration in this repository: none use CREATE INDEX
// CONCURRENTLY or any other statement Postgres forbids inside a transaction
// (asserted by TestPostgresMigrationsAreTransactionSafe).
func (e *pgExecutor) Apply(ctx context.Context, m Migration) error {
	start := time.Now()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once committed

	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		// Record the failure on a separate connection so the operator can see
		// it, since the transaction that would have carried it is being rolled
		// back. Best-effort: a recording failure must not mask the real error.
		e.recordFailure(ctx, m, err)
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO `+LedgerTable+` (id, checksum, duration_ms, success, nexus_version)
VALUES ($1, $2, $3, TRUE, $4)
ON CONFLICT (id) DO UPDATE
   SET checksum = EXCLUDED.checksum,
       applied_at = now(),
       duration_ms = EXCLUDED.duration_ms,
       success = TRUE,
       error = '',
       nexus_version = EXCLUDED.nexus_version`,
		m.ID, m.Checksum, time.Since(start).Milliseconds(), e.version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (e *pgExecutor) recordFailure(ctx context.Context, m Migration, cause error) {
	// Use a fresh short-lived context: the caller's may already be cancelled.
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = e.pool.Exec(c, `
INSERT INTO `+LedgerTable+` (id, checksum, success, error, nexus_version)
VALUES ($1, $2, FALSE, $3, $4)
ON CONFLICT (id) DO UPDATE
   SET checksum = EXCLUDED.checksum,
       applied_at = now(),
       success = FALSE,
       error = EXCLUDED.error,
       nexus_version = EXCLUDED.nexus_version`,
		m.ID, m.Checksum, cause.Error(), e.version)
}

// Lock takes a session-level advisory lock so exactly one process migrates at a
// time. With three gateway replicas rolling at once this is what turns racing
// DDL into an orderly queue.
//
// pg_advisory_lock blocks rather than failing, which is the behaviour we want
// during a rolling upgrade: the second process waits for the first to finish
// and then finds nothing to do. The caller's context bounds the wait.
func (e *pgExecutor) Lock(ctx context.Context) (func(), error) {
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		conn.Release()
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf(
				"timed out waiting for the migration advisory lock (key %d): "+
					"another process is migrating, or a previous run died holding it; "+
					"inspect with SELECT * FROM pg_locks WHERE locktype='advisory' AND objid=%d: %w",
				advisoryLockKey, advisoryLockKey, err)
		}
		return nil, err
	}
	e.lockConn = conn

	return func() {
		// Release on the SAME connection that took the lock, then hand it back.
		// A fresh context: unlocking must still happen when ctx is cancelled,
		// otherwise the lock survives until the connection is reaped.
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(c, `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
		conn.Release()
		e.lockConn = nil
	}, nil
}

// SchemaExists reports whether this database already has Nexus tables from
// before the ledger existed. virtual_keys is the probe because it is created by
// 001_init.sql and has never been renamed, so its presence means "some version
// of Nexus has already initialised this database".
func (e *pgExecutor) SchemaExists(ctx context.Context) (bool, error) {
	var exists bool
	err := e.pool.QueryRow(ctx,
		`SELECT EXISTS (
             SELECT 1 FROM information_schema.tables
             WHERE table_schema = current_schema() AND table_name = 'virtual_keys')`,
	).Scan(&exists)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	return exists, nil
}
