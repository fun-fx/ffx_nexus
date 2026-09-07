package main

import (
	"fmt"
	"log/slog"

	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/localdb"
)

const serveUsage = `Usage: nexus [serve] [--local]
       nexus migrate [flags]
       nexus mailtest --to <address>

  --local   Run a private Postgres alongside the gateway and use it as the
            control plane. Intended for a laptop: it stores state under
            NEXUS_LOCAL_STATE_DIR (default ~/.nexus), relaxes cookie security
            for plain-HTTP localhost, and applies schema migrations at boot.
            Never use it in a cluster.

Configuration is otherwise environment-only; see docs/configuration.md.
`

// parseServeFlags reads the arguments left after subcommand dispatch. The
// server takes one flag, so this is a loop rather than a flag.FlagSet: every
// other setting is an env var, and the standard parser would answer a
// mistyped one with a message about a flag package nothing else uses.
func parseServeFlags(args []string) (local, help bool, err error) {
	for _, a := range args {
		switch a {
		case "--local", "-local":
			local = true
		case "--help", "-h":
			help = true
		default:
			return false, false, fmt.Errorf("unknown flag %q", a)
		}
	}
	return local, help, nil
}

// startLocalMode brings up the child Postgres and rewrites cfg so the rest of
// boot treats it as an ordinary external database.
//
// The overrides are not suggestions the operator can undo with an env var,
// with one exception noted below. Local mode is a single named configuration
// — "one command, working console, on a laptop" — and a half-applied version
// of it produces the failures the mode exists to avoid: a console that cannot
// log in over http, or a schema the binary refuses to serve against.
func startLocalMode(cfg *config.Config, log *slog.Logger) (*localdb.DB, error) {
	if cfg.PostgresURL != "" {
		// Refusing is kinder than silently ignoring one of the two. An
		// operator who set both wants the external database; they can drop
		// --local and get exactly that.
		return nil, fmt.Errorf("local mode and NEXUS_POSTGRES_URL are mutually exclusive: " +
			"drop --local/NEXUS_LOCAL_DB to use the database you configured")
	}

	stateDir, err := localdb.StateDir(cfg.LocalStateDir)
	if err != nil {
		return nil, err
	}
	cfg.LocalStateDir = stateDir

	// Generated once and persisted, because a key that changes between runs
	// leaves every provider credential the user stored in the console
	// permanently undecryptable.
	if cfg.MasterKey == "" {
		key, err := localdb.MasterKey(stateDir)
		if err != nil {
			return nil, err
		}
		cfg.MasterKey = key
	}

	db, err := localdb.Start(localdb.Options{
		StateDir:     stateDir,
		BinariesPath: cfg.LocalDBBinaries,
		Port:         cfg.LocalDBPort,
		Log:          log,
	})
	if err != nil {
		return nil, err
	}

	cfg.ApplyLocalMode(db.URL())

	log.Warn("local mode is on: schema migrations run at boot, cookies are not Secure-only, and state lives on this machine",
		"state_dir", stateDir, "data_dir", db.DataDir())
	log.Info("local mode: control plane is a private Postgres owned by this process",
		"trace_persistence", "off (set NEXUS_CLICKHOUSE_URL to keep history)",
		"rate_limits", "in-memory (single process)")

	return db, nil
}
