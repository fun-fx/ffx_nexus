package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"

	nexus "github.com/ffxnexus/nexus"
	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/health"
	"github.com/ffxnexus/nexus/internal/migrate"
)

// Readiness check names, surfaced in the /readyz payload.
const (
	readyPostgresSchema   = "postgres_schema"
	readyClickHouseSchema = "clickhouse_schema"
)

// applySchemaAtBoot decides what a starting server does about the Postgres
// schema.
//
// Default (AutoMigrate false): verify only. If migrations are outstanding, the
// pod stays NotReady with a message naming them. This is the conservative
// choice for a customer install because:
//
//   - Three replicas rolling simultaneously must not all attempt DDL. The
//     advisory lock makes that safe, but "safe" is not the same as "desirable":
//     schema changes during a rollout mean the old and new binaries briefly
//     share a schema neither was tested against.
//   - A migration failure must stop the deployment. A pod that migrates itself
//     has already been told to serve traffic by the time it finds out.
//
// The Helm chart therefore runs `nexus migrate` as a pre-install/pre-upgrade
// hook Job, and Helm aborts the release if that Job fails.
//
// AutoMigrate true is the local-development convenience: `docker compose up`
// on an empty database should just work without a second command.
func applySchemaAtBoot(
	ctx context.Context,
	pool *pgxpool.Pool,
	cfg config.Config,
	ready *health.Gate,
	log *slog.Logger,
) {
	migs, err := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	if err != nil {
		// A broken embedded migration set is a build defect, not a runtime
		// condition. Refuse to serve rather than guess.
		ready.Set(readyPostgresSchema, false, true, "migration set is unreadable: "+err.Error())
		log.Error("cannot load postgres migrations", "err", err)
		return
	}
	ex := migrate.NewPostgres(pool, nexusBuildTag)

	if cfg.AutoMigrate {
		log.Warn("NEXUS_AUTO_MIGRATE is enabled: applying schema migrations during boot",
			"detail", "intended for local development. In production run the "+
				"`nexus migrate` job before the rollout so a failure stops the deploy")
		res, err := migrate.Run(ctx, ex, migs, migrate.Options{Logger: log})
		if err != nil {
			ready.Set(readyPostgresSchema, false, true, "migration failed: "+err.Error())
			log.Error("postgres migration failed at boot", "err", err)
			return
		}
		log.Info("postgres schema current",
			"applied", len(res.Applied), "alreadyApplied", len(res.Skipped), "adopted", res.Adopted)
		ready.Set(readyPostgresSchema, true, true, "")
		return
	}

	pending, err := migrate.Pending(ctx, ex, migs)
	if err != nil {
		ready.Set(readyPostgresSchema, false, true, "cannot read migration ledger: "+err.Error())
		log.Error("cannot determine postgres schema state", "err", err)
		return
	}
	if len(pending) > 0 {
		detail := fmt.Sprintf(
			"%d migration(s) outstanding (%s). Run `nexus migrate` — the chart does this "+
				"in a pre-upgrade hook Job. Set NEXUS_AUTO_MIGRATE=true only for local development.",
			len(pending), strings.Join(pending, ", "))
		ready.Set(readyPostgresSchema, false, true, detail)
		log.Error("postgres schema is behind this binary; refusing to report ready",
			"pending", len(pending), "migrations", strings.Join(pending, ", "))
		return
	}
	ready.Set(readyPostgresSchema, true, true, "")
	log.Info("postgres schema verified current", "migrations", len(migs))
}

// applyClickHouseSchemaAtBoot mirrors applySchemaAtBoot for the analytics store.
//
// The readiness check is registered as NOT required: ClickHouse holds trace
// history and eval scores, both of which are read-side features. Withholding
// gateway traffic because an analytics store is behind would convert an
// observability problem into an outage, which is the opposite of the guarantee
// this product makes ("Grafana/Metabase/ClickHouse being down must not stop LLM
// requests"). The condition is still reported so an operator can see it.
func applyClickHouseSchemaAtBoot(
	ctx context.Context,
	conn driver.Conn,
	cfg config.Config,
	ready *health.Gate,
	log *slog.Logger,
) {
	migs, err := migrate.Load(nexus.Migrations, migrate.EngineClickHouse)
	if err != nil {
		ready.Set(readyClickHouseSchema, false, false, "migration set is unreadable: "+err.Error())
		log.Error("cannot load clickhouse migrations", "err", err)
		return
	}
	ex := migrate.NewClickHouse(conn, nexusBuildTag)

	if cfg.AutoMigrate {
		res, err := migrate.Run(ctx, ex, migs, migrate.Options{Logger: log})
		if err != nil {
			ready.Set(readyClickHouseSchema, false, false, "migration failed: "+err.Error())
			log.Error("clickhouse migration failed at boot", "err", err)
			return
		}
		log.Info("clickhouse schema current",
			"applied", len(res.Applied), "alreadyApplied", len(res.Skipped), "adopted", res.Adopted)
		ready.Set(readyClickHouseSchema, true, false, "")
		return
	}

	pending, err := migrate.Pending(ctx, ex, migs)
	if err != nil {
		ready.Set(readyClickHouseSchema, false, false, "cannot read migration ledger: "+err.Error())
		log.Error("cannot determine clickhouse schema state", "err", err)
		return
	}
	if len(pending) > 0 {
		detail := fmt.Sprintf(
			"%d migration(s) outstanding (%s). Trace and benchmark history may be "+
				"incomplete until `nexus migrate` runs. LLM request handling is unaffected.",
			len(pending), strings.Join(pending, ", "))
		ready.Set(readyClickHouseSchema, false, false, detail)
		log.Warn("clickhouse schema is behind this binary; analytics features degraded",
			"pending", len(pending), "migrations", strings.Join(pending, ", "))
		return
	}
	ready.Set(readyClickHouseSchema, true, false, "")
	log.Info("clickhouse schema verified current", "migrations", len(migs))
}
