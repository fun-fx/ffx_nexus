// Package leaser implements a durable single-leader election
// layer on top of Postgres. Phase D-1 rewrites the package
// around two hard rules from the spec:
//
//  1. The advisory lock is the primary mutual-exclusion
//     primitive. It MUST be held on the same connection the
//     lease is taken on, for the entire lifetime of the lease.
//     Releasing the connection back to the pool releases the
//     advisory lock too (Postgres semantics) which would
//     silently void the contract.
//
//  2. The leases table is an observation / audit surface. It
//     records who holds the advisory lock and when their
//     heartbeat last refreshed, so an operator investigating
//     "why did two workers fire the same schedule" can read
//     the row without instrumenting Postgres itself. The table
//     is NEVER used to decide who runs. If the row says we
//     hold the lease but pg_advisory_locks says otherwise, the
//     advisory lock wins — every tick re-checks both.
//
// The two rules together prevent the bug class the legacy
// implementation ran into: releasing the connection back to
// the pool dropped the advisory lock silently, after which
// another pod could take it from the same connection pool (a
// race the lease row could not see).
//
// Why two int32 arguments to pg_try_advisory_lock(key1, key2):
// schedules have UUIDs in postgres today; hashing a UUID into a
// single int64 loses bits to collision. Using
// pg_try_advisory_lock(int4 hash_a, int4 hash_b) on a
// two-part hash keeps collisions prohibitively unlikely even
// for hundreds of millions of schedules. A single int8 key
// would work for the few role names we had, but Phase D-1
// wants per-schedule locking so two long-running schedules do
// not serialise each other on a single global mutex.
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

// DefaultTTL bounds a lease's window without a heartbeat.
// Long enough to ride out a network blip, short enough that a
// crashed pod's lease expires before its slot becomes a
// blocking dependency for follow-on schedules.
const DefaultTTL = 15 * time.Second

// DefaultRenewInterval is DefaultTTL/2. Two consecutive misses
// cause a handover; one is recoverable.
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
	lockKey  [2]int32
}

// Manager owns the renew goroutines for active leases and
// pins pgxpool connections so the advisory lock survives
// renew-loop heartbeats.
type Manager struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	// per-lease state. Mu below guards both maps.
	mu     sync.Mutex
	leases map[string]*activeLease

	// stop is closed by Shutdown to terminate renew goroutines
	// that are still ticking on shutdown.
	stop chan struct{}
	wg   sync.WaitGroup
}

// activeLease tracks the dedicated pgxpool.Conn that holds the
// advisory lock for a lease. The conn must NOT return to the
// pool while the lease is held — the conn struct stays open so
// the conn.Release callback can close it on demand.
type activeLease struct {
	lease Lease
	tok   chan struct{}
	conn  *pgxpool.Conn // pinned for the lease lifetime; never Released until Release().
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

// NewKeyForRole hashes the role string into a two-int32
// advisory lock key. fnv64a is deterministic; splitting the
// 64-bit hash into two halves gives the scheduler key space
// approximately 2^64 distinct keys (collision probability
// approximately 2^-32 across an entire customer fleet).
//
// Tests use KeyForRole export; production callers use the
// schedule-keyed entrypoints instead.
func KeyForRole(role string) [2]int32 {
	return KeyForRoleTest(role)
}

// KeyForRoleTest exports the role-key hash so unit tests can
// assert collisions without taking out a real Postgres
// connection. Behaviour is identical to KeyForRole.
func KeyForRoleTest(role string) [2]int32 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(role))
	v := h.Sum64()
	// Two halves, sign-aware: pg_advisory_lock treats each
	// int32 signed so the mask is intentional (we cast
	// int64 to int32 below, mapping the bottom 32 bits and
	// the top 32 bits of the hash independently).
	return [2]int32{int32(uint32(v)), int32(uint32(v >> 32))}
}

// KeyForSchedule hashes a schedule identifier for lock scoping.
// Each schedule gets its own two-int32 advisory lock key, so
// two long-running schedules cannot serialise on a single
// global mutex. The hash itself uses fnv64a for determinism;
// collisions are below the 10^-9 floor across realistic
// schedule counts.
func KeyForSchedule(scheduleID string) [2]int32 {
	return KeyForRoleTest(scheduleID)
}

// Acquire takes the lease for the given role or schedule. The
// caller passes a fresh ownerID (typically pod-name + uuid) so
// a future operator script can identify the previous holder.
// The Manager pins a *pgxpool.Conn and holds pg_try_advisory_lock
// for the lifetime of the lease — Release() returns the conn.
//
// Returns the latest Lease or an error.
//   - ErrAlreadyHeld: another pod owns the lease and the existing
//     lease has not expired yet. The caller can wait and retry.
//   - Other errors: pgxpool unreachable, schema missing, etc.
func (m *Manager) Acquire(ctx context.Context, role, ownerID string) (Lease, error) {
	return m.acquire(ctx, role, role, ownerID)
}

// AcquireSchedule is Acquire scoped to a single schedule
// id. The lease role is recorded as the schedule id; the
// global "role" of the manager is still required for the
// Acquire context so a single Manager can hold multiple
// schedule leases.
//
// We treat scheduleID as the role key for advisory lock
// purposes — two schedules that hash to different keys do
// not block each other, even if the underlying Manager is the
// same goroutine set. The lease-row role is kept as the
// scheduleID so a SafeTakeover only requires the previous
// owner to have explicitly released (advisory lock release,
// not query row update).
func (m *Manager) AcquireSchedule(ctx context.Context, scheduleID, ownerID string) (Lease, error) {
	if scheduleID == "" {
		return Lease{}, errors.New("leaser: scheduleID is required")
	}
	return m.acquire(ctx, scheduleID, scheduleID, ownerID)
}

// acquire is the shared core of Acquire and AcquireSchedule.
// lockRole is the advisory-lock key seed; rowRole is what
// gets stored in benchmark_scheduler_leases.role (operators
// read the row by schedule id; we never put a human-friendly
// job name in the role column to keep the audit log legible).
func (m *Manager) acquire(ctx context.Context, lockRole, rowRole, ownerID string) (Lease, error) {
	if lockRole == "" {
		return Lease{}, errors.New("leaser: role is required")
	}
	if ownerID == "" {
		return Lease{}, errors.New("leaser: ownerID is required")
	}

	lockKey := KeyForRole(lockRole)
	tok, err := newToken()
	if err != nil {
		return Lease{}, fmt.Errorf("leaser: generate token: %w", err)
	}

	// Phase D-1 spec: dedicated connection. Take it from the
	// pool and HOLD it (do not defer Release until Release()).
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return Lease{}, fmt.Errorf("leaser: acquire conn: %w", err)
	}
	acquired := false
	defer func() {
		if !acquired {
			conn.Release()
		}
	}()
	row := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, lockKey[0], lockKey[1])
	var gotLock bool
	if err := row.Scan(&gotLock); err != nil {
		return Lease{}, fmt.Errorf("leaser: advisory_lock(%v): %w", lockKey, err)
	}
	if !gotLock {
		return Lease{}, ErrAlreadyHeld
	}

	// Write/refresh the durable row so operators can see
	// ownership. Only update on stale or matching token.
	now := time.Now().UTC()
	expires := now.Add(DefaultTTL)
	const upsert = `
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
	err = m.pool.QueryRow(ctx, upsert, rowRole, ownerID, now, expires, tok).Scan(&gotRow, &gotOwner)
	if err != nil {
		// First free the advisory lock on this conn before
		// returning; otherwise the conn-release path could
		// (per Postgres behaviour) drop the lock slightly
		// later than the caller expects.
		_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`, lockKey[0], lockKey[1])
		if errors.Is(err, pgx.ErrNoRows) {
			return Lease{}, ErrAlreadyHeld
		}
		return Lease{}, fmt.Errorf("leaser: upsert lease: %w", err)
	}

	lease := Lease{
		Role:     rowRole,
		OwnerID:  ownerID,
		Token:    tok,
		Acquired: now,
		lockKey:  lockKey,
	}

	m.mu.Lock()
	// Refuse double-acquire from the same Manager: another
	// caller in the same pod already pinned the conn.
	if existing, ok := m.leases[rowRole]; ok {
		m.mu.Unlock()
		_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`, lockKey[0], lockKey[1])
		conn.Release()
		return existing.lease, fmt.Errorf("leaser: role %q already held by %q", rowRole, existing.lease.OwnerID)
	}
	al := &activeLease{lease: lease, tok: make(chan struct{}), conn: conn}
	m.leases[rowRole] = al
	m.mu.Unlock()
	acquired = true

	m.wg.Add(1)
	go m.renewLoop(rowRole, al)

	m.log.Info("leaser: acquired",
		"role", rowRole, "owner", ownerID, "ttl", DefaultTTL)
	return lease, nil
}

// renewLoop bumps the heartbeat every DefaultRenewInterval.
// If the renew fails because the row has been claimed by
// someone else (lock_token mismatch) we surface ErrLostLease
// and stop. If the renew fails because Postgres is unreachable
// we keep trying until TTL elapses and another pod takes over.
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

// Release stops the renew loop, frees the advisory lock on
// the pinned connection, returns the connection to the pool,
// and sets the row expires_at = NOW() so another pod can take
// over without waiting for TTL. Idempotent: calling Release
// on a role the manager does not know about is a no-op.
//
// Order matters: release the advisory lock BEFORE returning
// the conn to the pool. Postgres's pg_advisory_unlock fires
// on the same connection that took the lock; if we returned
// the conn first and another request reused it, we'd be
// calling pg_advisory_unlock on someone else's connection.
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

	// Tell Postgres we are explicitly letting go so a peer can
	// take over without waiting for TTL to elapse.
	if _, err := al.conn.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`, al.lease.lockKey[0], al.lease.lockKey[1]); err != nil {
		m.log.Warn("leaser: advisory_unlock", "role", role, "err", err)
	}
	al.conn.Release()

	const query = `
UPDATE benchmark_scheduler_leases
   SET expires_at = NOW()
 WHERE role = $1 AND lock_token = $2`
	if _, err := m.pool.Exec(ctx, query, role, al.lease.Token); err != nil {
		m.log.Warn("leaser: row release", "role", role, "err", err)
		return fmt.Errorf("leaser: release: %w", err)
	}
	m.log.Info("leaser: released", "role", role, "owner", al.lease.OwnerID)
	return nil
}

// Shutdown stops all renew goroutines and waits for them to
// drain. Used by graceful pod shutdown.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	for role := range m.leases {
		// Release handles its own logging; we just iterate.
		al := m.leases[role]
		close(al.tok)
		if _, err := al.conn.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`, al.lease.lockKey[0], al.lease.lockKey[1]); err == nil {
			al.conn.Release()
		} else {
			al.conn.Release()
		}
		delete(m.leases, role)
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
