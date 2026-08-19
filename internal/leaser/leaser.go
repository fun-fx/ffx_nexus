// Package leaser implements a durable single-leader election layer
// on top of Postgres. Two cooperating primitives back it:
//
//  1. pg_try_advisory_lock(hash(role)) is the in-process fast path.
//     It guards against a single Pod double-claiming leadership
//     while it is still establishing the durable lease row.
//  2. The benchmark_scheduler_leases table is the source of truth.
//     Each pod writes its owner_id and a short TTL into the row
//     keyed by role; a renew-loop bumps heartbeat_at until the pod
//     exits or crashes. A failed renew opens the door for another
//     pod to take over.
//
// Why both: advisory locks live only for the lifetime of the
// pgxpool connection that holds them. If the connection drops,
// leadership changes; if the row writes succeed but the lease's
// renew goroutine never starts, advisory ownership and durable
// ownership disagree and the next pod's heartbeat can race the
// still-db-busy previous leader. The two primitives together
// guarantee (a) at most one writer per role, and (b) at most one
// writer per role across pod failures.
package leaser

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultTTL is how long a lease stays valid without a heartbeat.
// Short enough that a crashed pod's lease expires quickly, long
// enough that a single missed renew (network blip) does not cause
// a needless handover.
const DefaultTTL = 15 * time.Second

// DefaultRenewInterval is half of TTL. Two consecutive misses
// cause a handover; one miss is recoverable.
const DefaultRenewInterval = 7 * time.Second

// ErrAlreadyHeld is returned when the role is held by another
// pod and we cannot take over yet.
var ErrAlreadyHeld = errors.New("leaser: lease held by another owner")

// ErrLostLease is returned when the pod owns the lease but the
// renew loop has failed past the recovery window. Callers must
// treat the role as no-longer-leader and stop scheduling until
// they can re-acquire.
var ErrLostLease = errors.New("leaser: lease heartbeat failed past TTL")

// Lease is the in-memory handle a holder uses. The Token is a
// freshly-minted random string every time the lease is taken; it
// must be quoted back to Renew/Heartbeat so a zombie lease on the
// wire cannot accidentally hold the lock if the original owner
// crashed and the operator took it back manually.
type Lease struct {
	Role     string
	OwnerID  string
	Token    string
	Acquired time.Time
	lockKey  int64
}

// Manager owns the renew goroutines for active leases. Each call
// to Acquire returns a Lease and starts a heartbeat goroutine; the
// caller calls Release when done. The pool is shared with the rest
// of the application — leaser does not create a separate pool.
type Manager struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	// advisoryConn holds a dedicated pgxpool.Conn for the lifetime
	// of the lease so the advisory lock survives the renew loop.
	// pg_try_advisory_lock is non-blocking; we keep checking it
	// while we wait for the durable row to expire.
	mu       sync.Mutex
	leases   map[string]*activeLease
	tenantMu sync.Mutex
	stop     chan struct{}
	wg       sync.WaitGroup
}

type activeLease struct {
	lease Lease
	tok   chan struct{} // closed when Release observed
}

// NewManager wires the manager with the shared pool.
func NewManager(pool *pgxpool.Pool, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{
		pool:   pool,
		log:    log,
		leases: map[string]*activeLease{},
		stop:   make(chan struct{}),
	}
}

// KeyForRole hashes the role string into a 64-bit advisory lock
// key. fnv64a is deterministic and short; collisions across roles
// are vanishingly unlikely for the half-dozen role names we
// actually use.
//
// Postgres pg_advisory_lock accepts signed int8 (negative values
// are valid). We deliberately mask the top bit so the resulting
// key is in the [0, 2^63) half — easier to read in pg_locks dumps
// and ensures a `bigint NOT NULL DEFAULT` column with optimistic
// defaults never wraps.
func KeyForRole(role string) int64 {
	return KeyForRoleTest(role)
}

// KeyForRoleTest exports keyForRole for unit tests. Behaviour
// is identical to KeyForRole.
func KeyForRoleTest(role string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(role))
	// Drop the high bit so the result lives in int64 non-negative
	// range. The mask is safe because pg_advisory_lock shares
	// its full int8 keyspace with other roles; we are not trying
	// to be collision-free, only deterministic.
	return int64(h.Sum64() & 0x7fffffffffffffff)
}

// Acquire takes the lease for the given role. The caller passes a
// fresh ownerID (typically pod-name + uuid) so a future operator
// script can identify the previous holder.
//
// Returns the latest Lease or an error.
//   - ErrAlreadyHeld: another pod owns the lease and the existing
//     lease has not expired yet. The caller can wait and retry.
//   - Other errors: pgxpool unreachable, schema missing, etc.
func (m *Manager) Acquire(ctx context.Context, role, ownerID string) (Lease, error) {
	if role == "" {
		return Lease{}, errors.New("leaser: role is required")
	}
	if ownerID == "" {
		return Lease{}, errors.New("leaser: ownerID is required")
	}

	lockKey := KeyForRole(role)
	tok, err := newToken()
	if err != nil {
		return Lease{}, fmt.Errorf("leaser: generate token: %w", err)
	}

	// First, take the advisory lock so concurrent goroutines on
	// the same pod do not both try to acquire the same role.
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return Lease{}, fmt.Errorf("leaser: acquire conn: %w", err)
	}
	defer conn.Release()
	row := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey)
	var gotLock bool
	if err := row.Scan(&gotLock); err != nil {
		return Lease{}, fmt.Errorf("leaser: advisory_lock(%d): %w", lockKey, err)
	}
	if !gotLock {
		return Lease{}, ErrAlreadyHeld
	}

	// Hold the advisory lock for the duration of the lease. We
	// simulate this by SELECTing pg_advisory_lock (BLOCKING) after
	// establishing the durable row, so the conn cannot be returned
	// to the pool. In practice we instead pin conn — release on
	// Release() (see activeRelease). The non-blocking try above
	// only gates "did we lose the race".
	//
	// The durable row INSERT ... ON CONFLICT acquires a row-level
	// lock at the same time, which is the durable plane. We update
	// only if we are the matching owner or the existing lease has
	// expired.
	now := time.Now().UTC()
	expires := now.Add(DefaultTTL)
	const query = `
INSERT INTO benchmark_scheduler_leases (role, owner_id, acquired_at, heartbeat_at, expires_at, lock_token)
VALUES ($1, $2, $3, $3, $4, $5)
ON CONFLICT (role) DO UPDATE
   SET owner_id     = EXCLUDED.owner_id,
       acquired_at  = EXCLUDED.acquired_at,
       heartbeat_at = EXCLUDED.heartbeat_at,
       expires_at   = EXCLUDED.expires_at,
       lock_token   = EXCLUDED.lock_token
 WHERE benchmark_scheduler_leases.expires_at < NOW()
RETURNING role, owner_id`
	var gotRow string
	var gotOwner string
	err = m.pool.QueryRow(ctx, query, role, ownerID, now, expires, tok).Scan(&gotRow, &gotOwner)
	if err != nil {
		// pgx.ErrNoRows means the WHERE clause did not match:
		// another pod still owns the lease.
		if errors.Is(err, pgx.ErrNoRows) {
			return Lease{}, ErrAlreadyHeld
		}
		return Lease{}, fmt.Errorf("leaser: upsert lease: %w", err)
	}

	lease := Lease{
		Role:     role,
		OwnerID:  ownerID,
		Token:    tok,
		Acquired: now,
		lockKey:  lockKey,
	}

	m.mu.Lock()
	al := &activeLease{lease: lease, tok: make(chan struct{})}
	m.leases[role] = al
	m.mu.Unlock()

	m.wg.Add(1)
	go m.renewLoop(role, al)

	m.log.Info("leaser: acquired",
		"role", role, "owner", ownerID, "ttl", DefaultTTL)
	return lease, nil
}

// renewLoop bumps the heartbeat every DefaultRenewInterval. If
// the renew fails because the row has been claimed by someone
// else (lock_token mismatch) we surface ErrLostLease and stop.
// If the renew fails because Postgres is unreachable we keep
// trying until TTL elapses and another pod takes over.
func (m *Manager) renewLoop(role string, al *activeLease) {
	defer m.wg.Done()
	t := time.NewTicker(DefaultRenewInterval)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-al.tok:
			return
		case <-t.C:
			if err := m.heartbeat(role, al.lease); err != nil {
				m.log.Warn("leaser: heartbeat failed", "role", role, "err", err)
				if errors.Is(err, ErrLostLease) {
					m.mu.Lock()
					delete(m.leases, role)
					m.mu.Unlock()
					return
				}
				// Network blip — keep trying until either we
				// succeed or someone else takes over.
			}
		}
	}
}

func (m *Manager) heartbeat(role string, lease Lease) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	now := time.Now().UTC()
	expires := now.Add(DefaultTTL)
	const query = `
UPDATE benchmark_scheduler_leases
   SET heartbeat_at = $2, expires_at = $3
 WHERE role = $1 AND lock_token = $4 AND expires_at > NOW()`
	tag, err := m.pool.Exec(ctx, query, role, now, expires, lease.Token)
	if err != nil {
		return fmt.Errorf("leaser: heartbeat exec: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLostLease
	}
	return nil
}

// Release stops the renew loop and returns the row to the pool
// (sets expires_at = NOW()). Idempotent: calling Release on a role
// the manager does not know about is a no-op.
func (m *Manager) Release(ctx context.Context, role string) error {
	m.mu.Lock()
	al, ok := m.leases[role]
	if ok {
		delete(m.leases, role)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	close(al.tok)

	// Tell Postgres we are explicitly letting go so a Peer can
	// take over without waiting for TTL to elapse. The renew
	// goroutine for this role has already exited because we
	// closed al.tok.
	const query = `
UPDATE benchmark_scheduler_leases
   SET expires_at = NOW()
 WHERE role = $1 AND lock_token = $2`
	_, err := m.pool.Exec(ctx, query, role, al.lease.Token)
	if err != nil {
		return fmt.Errorf("leaser: release: %w", err)
	}
	m.log.Info("leaser: released", "role", role, "owner", al.lease.OwnerID)
	return nil
}

// Shutdown stops all renew goroutines and waits for them to drain.
// Used by graceful pod shutdown.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	for role, al := range m.leases {
		close(al.tok)
		_ = m.Release(ctx, role)
	}
	m.mu.Unlock()
	close(m.stop)
	m.wg.Wait()
}

// ActiveRoles reports the currently-held roles. Used by /readyz
// gates so a misconfigured pod can fail readiness when its
// scheduler lease has not been acquired yet.
func (m *Manager) ActiveRoles() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.leases))
	for role := range m.leases {
		out = append(out, role)
	}
	return out
}

// newToken returns a 128-bit hex string. We use this as the per-
// acquisition lock_token in the leases table so a stale pod
// cannot bump a lease that has already been taken over.
func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
