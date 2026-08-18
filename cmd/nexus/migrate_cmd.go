package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"

	nexus "github.com/ffxnexus/nexus"
	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/migrate"
	"github.com/ffxnexus/nexus/internal/observability"
)

// runMigrateCommand implements `nexus migrate`.
//
// A dedicated command exists so schema changes are a deliberate, observable
// deployment step with its own exit code, rather than a side effect of a pod
// starting up. That separation is what lets the Helm chart gate an upgrade on
// migrations succeeding (a pre-upgrade hook Job that must exit 0) instead of
// discovering a broken schema from user-visible 500s after the rollout is
// already half-done.
//
// Exit codes:
//
//	0  migrations are up to date (either already, or applied now)
//	1  a migration failed, or the database was unreachable
//	2  --check found outstanding migrations (nothing was applied)
//
// Exit code 2 is distinct so CI and pre-flight scripts can ask "would this
// upgrade change the schema?" without treating that as an error.
func runMigrateCommand(args []string) int {
	fs := flag.NewFlagSet("nexus migrate", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: nexus migrate [flags]

Applies outstanding schema migrations to the configured datastores and exits.
Connection details come from the same environment as the server:
NEXUS_POSTGRES_URL and NEXUS_CLICKHOUSE_URL. A datastore whose URL is empty is
skipped, so a deployment that runs without ClickHouse needs no extra flags.

Every applied migration is recorded in the schema_migrations ledger, so this is
safe to run repeatedly and safe to run concurrently: Postgres work is
serialised behind an advisory lock, and every statement is written to be
replay-safe.

There are no down migrations. See docs/customer-self-hosted-upgrade-rollback.md
for how application rollback works against a newer schema.

Flags:
`)
		fs.PrintDefaults()
	}

	var (
		engine   = fs.String("engine", "all", `which datastore to migrate: "all", "postgres" or "clickhouse"`)
		check    = fs.Bool("check", false, "report outstanding migrations and exit 2 if any; changes nothing")
		dryRun   = fs.Bool("dry-run", false, "alias for --check")
		timeout  = fs.Duration("timeout", 5*time.Minute, "overall deadline, including waiting for the advisory lock")
		allowDft = fs.Bool("allow-checksum-drift", false,
			"proceed when an already-applied migration file has been edited. Only after verifying the schema by hand")
		verbose = fs.Bool("verbose", false, "log every migration considered, not just those applied")
	)
	if err := fs.Parse(args); err != nil {
		return 1
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	cfg := config.Load()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	wantPG := *engine == "all" || *engine == string(migrate.EnginePostgres)
	wantCH := *engine == "all" || *engine == string(migrate.EngineClickHouse)
	if !wantPG && !wantCH {
		fmt.Fprintf(os.Stderr, "unknown --engine %q; want all, postgres or clickhouse\n", *engine)
		return 1
	}

	opts := migrate.Options{
		DryRun:             *check || *dryRun,
		AllowChecksumDrift: *allowDft,
		Logger:             log,
	}

	// A migration job that silently does nothing because both URLs are unset is
	// worse than a failure: the rollout proceeds and the schema is untouched.
	if (!wantPG || cfg.PostgresURL == "") && (!wantCH || cfg.ClickHouseURL == "") {
		fmt.Fprintln(os.Stderr,
			"nothing to migrate: neither NEXUS_POSTGRES_URL nor NEXUS_CLICKHOUSE_URL is set. "+
				"If this is intentional, do not schedule the migration job.")
		return 1
	}

	pending := 0

	if wantPG && cfg.PostgresURL != "" {
		n, err := migrateEngine(ctx, log, opts, migrate.EnginePostgres, func() (migrate.Executor, func(), error) {
			pool, err := pgxpool.New(ctx, cfg.PostgresURL)
			if err != nil {
				return nil, nil, err
			}
			if err := pool.Ping(ctx); err != nil {
				pool.Close()
				return nil, nil, err
			}
			return migrate.NewPostgres(pool, nexusBuildTag), pool.Close, nil
		})
		if err != nil {
			log.Error("postgres migration failed", "err", err)
			return 1
		}
		pending += n
	} else if wantPG {
		log.Info("skipping postgres: NEXUS_POSTGRES_URL is empty")
	}

	if wantCH && cfg.ClickHouseURL != "" {
		n, err := migrateEngine(ctx, log, opts, migrate.EngineClickHouse, func() (migrate.Executor, func(), error) {
			conn, err := observability.NewCHConn(ctx, cfg.ClickHouseURL)
			if err != nil {
				return nil, nil, err
			}
			return migrate.NewClickHouse(conn, nexusBuildTag),
				func() { _ = conn.Close() }, nil
		})
		if err != nil {
			log.Error("clickhouse migration failed", "err", err)
			return 1
		}
		pending += n
	} else if wantCH {
		log.Info("skipping clickhouse: NEXUS_CLICKHOUSE_URL is empty")
	}

	if opts.DryRun {
		if pending > 0 {
			log.Info("outstanding migrations found", "count", pending)
			return 2
		}
		log.Info("schema is up to date")
		return 0
	}
	log.Info("migrations complete")
	return 0
}

// migrateEngine connects, runs, and reports. Returns the number of outstanding
// migrations, which is only meaningful for a dry run.
func migrateEngine(
	ctx context.Context,
	log *slog.Logger,
	opts migrate.Options,
	engine migrate.Engine,
	connect func() (migrate.Executor, func(), error),
) (int, error) {
	migs, err := migrate.Load(nexus.Migrations, engine)
	if err != nil {
		return 0, err
	}

	ex, closeFn, err := connect()
	if err != nil {
		return 0, fmt.Errorf("connecting to %s: %w", engine, err)
	}
	defer closeFn()

	res, err := migrate.Run(ctx, ex, migs, opts)
	if err != nil {
		if errors.Is(err, migrate.ErrChecksumMismatch) {
			log.Error("refusing to continue: an applied migration was modified", "engine", engine)
		}
		return 0, err
	}

	if opts.DryRun {
		if len(res.Pending) > 0 {
			log.Info("outstanding", "engine", engine, "migrations", strings.Join(res.Pending, ", "))
		} else {
			log.Info("up to date", "engine", engine, "applied", len(res.Skipped))
		}
		return len(res.Pending), nil
	}
	log.Info("migration summary",
		"engine", engine,
		"applied", len(res.Applied),
		"alreadyApplied", len(res.Skipped),
		"adoptedExistingDatabase", res.Adopted)
	return 0, nil
}

// schemaReadiness reports how many migrations are outstanding for the engines
// that are configured, without applying anything and without taking the
// migration lock. Used by the server to decide whether it may accept traffic.
func schemaReadiness(
	ctx context.Context,
	pool *pgxpool.Pool,
	chConn driver.Conn,
) (pending []string, err error) {
	if pool != nil {
		migs, err := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
		if err != nil {
			return nil, err
		}
		p, err := migrate.Pending(ctx, migrate.NewPostgres(pool, nexusBuildTag), migs)
		if err != nil {
			return nil, err
		}
		pending = append(pending, p...)
	}
	if chConn != nil {
		migs, err := migrate.Load(nexus.Migrations, migrate.EngineClickHouse)
		if err != nil {
			return nil, err
		}
		p, err := migrate.Pending(ctx, migrate.NewClickHouse(chConn, nexusBuildTag), migs)
		if err != nil {
			return nil, err
		}
		pending = append(pending, p...)
	}
	return pending, nil
}
