// Package migrate applies ordered, recorded, single-writer schema migrations.
//
// # Why this package exists
//
// Migrations used to be a hardcoded []string of paths inside main(), executed
// on every boot of every replica, with each failure logged and then ignored.
// That produced four distinct classes of production defect, all of which had
// actually shipped:
//
//  1. Drift between the files on disk and the list. 009-011 were added to the
//     repository but never to the list, so eval_plugins never existed and the
//     runtime silently fell back to an in-memory plugin store - every rolling
//     update uninstalled every console-installed plugin. The same thing
//     happened again later with 014_invite_tokens.sql (invites 500'd on every
//     fresh install) and with the ClickHouse benchmark_runs migration.
//  2. Wrong order. 013 was listed before 012, so 013's
//     `ALTER TABLE benchmark_runs ADD COLUMN schedule_id` ran before 012
//     created that table. The ALTER failed, the error was logged and dropped,
//     and 012 then created the table without the column - meaning
//     benchmark_runs.schedule_id does not exist in any deployment to date.
//  3. Non-fatal failures. A migration error left the pod Ready and serving
//     traffic against a schema that did not match the binary.
//  4. Concurrent DDL. With three replicas all migrating at boot, identical
//     DDL raced.
//
// # Design
//
// Migrations are DISCOVERED from the embedded filesystem rather than listed,
// which makes class 1 structurally impossible: a file in migrations/<engine>/
// is a migration, full stop. They are sorted by numeric ordinal, and a
// duplicate ordinal is a hard error, which makes class 2 impossible. Every
// applied migration is recorded in a `schema_migrations` ledger with its
// checksum, so re-running is a no-op and an edit to an already-applied file is
// detected instead of silently diverging. Postgres work is serialised with a
// session-level advisory lock and each migration runs in its own transaction
// together with its ledger row, so class 4 cannot corrupt state and a failure
// leaves nothing half-recorded.
//
// # Baseline / adoption of existing databases
//
// Deployments that predate the ledger have tables but no `schema_migrations`.
// We do NOT mark those migrations as applied-by-assumption, and we do not
// reinitialise anything. Every migration in this repository is written with
// `IF NOT EXISTS` guards (verified by TestMigrationsAreIdempotent), so the
// safe and honest action is to simply run them all and record the outcome:
// statements whose object already exists are no-ops, and statements that were
// never applied because of defects 1 and 2 above are finally applied. Adopting
// an existing database therefore also REPAIRS the schema drift those defects
// caused, which is why adoption is the default rather than a flag.
//
// # Rollback
//
// There are deliberately no down migrations. Automatic reversal of DDL is how
// production data gets deleted during an incident. Schema changes are additive
// (expand/contract), so version N of the binary runs against the N+1 schema,
// and rolling the application back is safe on its own. See
// docs/customer-self-hosted-upgrade-rollback.md.
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Engine identifies a target datastore. The value is also the directory name
// under migrations/ that holds that engine's SQL.
type Engine string

const (
	EnginePostgres   Engine = "postgres"
	EngineClickHouse Engine = "clickhouse"
)

// LedgerTable is the table name used on every engine.
const LedgerTable = "schema_migrations"

// Migration is one SQL file to apply exactly once.
type Migration struct {
	// ID is the stable ledger key, e.g. "postgres/014_invite_tokens.sql".
	// It includes the engine so one code path can reason about both.
	ID string
	// Engine is the datastore this migration targets.
	Engine Engine
	// Ordinal is the leading number in the filename. Used for ordering and
	// for duplicate detection.
	Ordinal int
	// Name is the bare filename.
	Name string
	// SQL is the file contents, applied verbatim.
	SQL string
	// Checksum is the SHA-256 of SQL, hex encoded. Recorded in the ledger so
	// an edit to an already-applied migration is detected rather than
	// silently producing two different schemas from the same version string.
	Checksum string
}

// LedgerEntry is one recorded row from the schema_migrations table.
type LedgerEntry struct {
	ID        string
	Checksum  string
	AppliedAt time.Time
	Success   bool
}

// Executor is the engine-specific half of the algorithm. Implementations live
// in postgres.go and clickhouse.go.
type Executor interface {
	// Engine reports which engine this executor targets.
	Engine() Engine
	// EnsureLedger creates the schema_migrations table if absent. Must be
	// idempotent and safe to call concurrently.
	EnsureLedger(ctx context.Context) error
	// Applied returns the ledger keyed by migration ID. Only successful rows
	// are included; a failed attempt must be reported as absent so it is
	// retried.
	Applied(ctx context.Context) (map[string]LedgerEntry, error)
	// Apply runs one migration and records it. Where the engine supports
	// transactions, the SQL and the ledger row must commit atomically.
	Apply(ctx context.Context, m Migration) error
	// Lock serialises migration across processes. The returned release
	// function must be safe to call exactly once. Engines without a locking
	// primitive return a no-op and rely on idempotent DDL.
	Lock(ctx context.Context) (release func(), err error)
	// SchemaExists reports whether this database already holds Nexus objects
	// from before the ledger existed. Used only to log adoption clearly.
	SchemaExists(ctx context.Context) (bool, error)
}

// Options tunes a Run.
type Options struct {
	// DryRun reports what would be applied and changes nothing.
	DryRun bool
	// AllowChecksumDrift downgrades a checksum mismatch from a hard error to
	// a warning. Only for the case where an operator knowingly edited an
	// applied migration (e.g. a comment fix) and has verified the schema by
	// hand. Never set this to work around a real divergence.
	AllowChecksumDrift bool
	// Logger receives progress. Required.
	Logger *slog.Logger
}

// Result summarises one Run.
type Result struct {
	Engine  Engine
	Applied []string
	Skipped []string
	Pending []string // populated on DryRun
	Adopted bool     // an existing pre-ledger database was brought under management
}

// ErrChecksumMismatch is returned when an already-applied migration file has
// changed on disk. Applying it again would not reproduce the recorded schema,
// and skipping it hides a real divergence, so the only safe move is to stop.
var ErrChecksumMismatch = errors.New("migrate: applied migration has been modified on disk")

// Load discovers every migration for one engine from fsys, expecting files at
// migrations/<engine>/NNN_name.sql.
//
// Discovery (rather than an explicit list) is the point: adding a file to the
// repository is sufficient to have it applied, so the list can never drift out
// of sync with reality again.
func Load(fsys fs.FS, engine Engine) ([]Migration, error) {
	dir := path.Join("migrations", string(engine))
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("migrate: reading %s: %w", dir, err)
	}

	out := make([]Migration, 0, len(entries))
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		ord, err := ordinalOf(e.Name())
		if err != nil {
			return nil, fmt.Errorf("migrate: %s/%s: %w", dir, e.Name(), err)
		}
		// Two files claiming the same ordinal have no defined order between
		// them, which is exactly how the ClickHouse 007 collision hid a
		// migration that was never applied. Refuse to guess.
		if prev, dup := seen[ord]; dup {
			return nil, fmt.Errorf(
				"migrate: duplicate ordinal %03d in %s (%s and %s): "+
					"renumber one of them; ordinals must be unique and gap-free-ish per engine",
				ord, dir, prev, e.Name())
		}
		seen[ord] = e.Name()

		body, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("migrate: reading %s/%s: %w", dir, e.Name(), err)
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{
			ID:       string(engine) + "/" + e.Name(),
			Engine:   engine,
			Ordinal:  ord,
			Name:     e.Name(),
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("migrate: no .sql files found under %s", dir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	return out, nil
}

// ordinalOf extracts the leading NNN from "014_invite_tokens.sql".
func ordinalOf(name string) (int, error) {
	i := strings.IndexByte(name, '_')
	if i <= 0 {
		return 0, errors.New("filename must start with a numeric ordinal followed by '_', e.g. 015_add_thing.sql")
	}
	n, err := strconv.Atoi(name[:i])
	if err != nil {
		return 0, fmt.Errorf("leading %q is not a number: %w", name[:i], err)
	}
	return n, nil
}

// Pending reports which migrations have not yet been applied, without applying
// anything and without taking the migration lock. Used by the readiness gate.
func Pending(ctx context.Context, ex Executor, migs []Migration) ([]string, error) {
	if err := ex.EnsureLedger(ctx); err != nil {
		return nil, err
	}
	applied, err := ex.Applied(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range migs {
		if _, ok := applied[m.ID]; !ok {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// Run applies every outstanding migration in ordinal order.
//
// The lock is taken before reading the ledger so two processes starting
// simultaneously cannot both decide the same migration is outstanding.
func Run(ctx context.Context, ex Executor, migs []Migration, opts Options) (Result, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	res := Result{Engine: ex.Engine()}

	if opts.DryRun {
		// Read-only path: no lock, so a readiness probe never contends with an
		// in-flight migration.
		if err := ex.EnsureLedger(ctx); err != nil {
			return res, fmt.Errorf("migrate[%s]: creating ledger: %w", ex.Engine(), err)
		}
		applied, err := ex.Applied(ctx)
		if err != nil {
			return res, err
		}
		for _, m := range migs {
			if _, ok := applied[m.ID]; ok {
				res.Skipped = append(res.Skipped, m.ID)
			} else {
				res.Pending = append(res.Pending, m.ID)
			}
		}
		return res, nil
	}

	// Take the lock FIRST, before creating the ledger. The advisory lock needs
	// no table of its own, so acquiring it up front means even the ledger's own
	// bootstrap DDL is serialised. This matters because `CREATE TABLE IF NOT
	// EXISTS` is not atomic in Postgres: two sessions can both pass the
	// existence check and then collide in the system catalogue. Executors also
	// tolerate that collision defensively (see isDuplicateObject), because the
	// read-only Pending path has to create the ledger without a lock.
	release, err := ex.Lock(ctx)
	if err != nil {
		return res, fmt.Errorf("migrate[%s]: acquiring migration lock: %w", ex.Engine(), err)
	}
	defer release()

	if err := ex.EnsureLedger(ctx); err != nil {
		return res, fmt.Errorf("migrate[%s]: creating ledger: %w", ex.Engine(), err)
	}

	// Read the ledger while holding the lock: before it, another process may
	// have been mid-apply.
	applied, err := ex.Applied(ctx)
	if err != nil {
		return res, err
	}

	// Adoption notice. A database with Nexus objects but an empty ledger is a
	// pre-ledger deployment; every migration below is idempotent, so running
	// them is both safe and the mechanism by which previously-skipped
	// migrations finally land.
	if len(applied) == 0 {
		existing, err := ex.SchemaExists(ctx)
		if err != nil {
			return res, err
		}
		if existing {
			res.Adopted = true
			log.Warn("adopting an existing database into the migration ledger",
				"engine", ex.Engine(),
				"detail", "all migrations will be replayed; they are idempotent, "+
					"so this is a no-op for objects that already exist and will "+
					"apply anything previously missed")
		}
	}

	for _, m := range migs {
		if prev, ok := applied[m.ID]; ok {
			if prev.Checksum != m.Checksum {
				if !opts.AllowChecksumDrift {
					return res, fmt.Errorf(
						"%w: %s (ledger recorded %s, file is now %s). "+
							"An applied migration must never be edited - add a new one instead. "+
							"If this edit is known-cosmetic and the schema is verified, re-run with --allow-checksum-drift",
						ErrChecksumMismatch, m.ID, short(prev.Checksum), short(m.Checksum))
				}
				log.Warn("checksum drift accepted by operator request",
					"migration", m.ID, "recorded", short(prev.Checksum), "onDisk", short(m.Checksum))
			}
			res.Skipped = append(res.Skipped, m.ID)
			continue
		}

		start := time.Now()
		if err := ex.Apply(ctx, m); err != nil {
			// Fatal on purpose. Continuing would leave the binary serving
			// traffic against a schema it does not match, which is precisely
			// the failure mode this package replaces.
			return res, fmt.Errorf("migrate[%s]: applying %s: %w", ex.Engine(), m.ID, err)
		}
		log.Info("migration applied",
			"engine", ex.Engine(), "migration", m.ID, "took", time.Since(start).Round(time.Millisecond))
		res.Applied = append(res.Applied, m.ID)
	}
	return res, nil
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}
