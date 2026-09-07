// Package localdb runs a Postgres server as a child process of the gateway.
//
// It exists for one reason: without Postgres the console is a shell. Every
// credential, virtual key, user and invite route answers 503 (see
// console.Server.requireStore), so "start the binary and configure it in the
// web UI" — the whole point of the npx and docker one-liners — does not work
// in zero-dependency mode.
//
// Running a real Postgres rather than adding a second storage backend keeps
// the SQL, the migrations and the store layer identical between a laptop and
// a customer's cluster. The trade is a ~30MB binary download on first run
// (cached afterwards) or a distro package inside the container image.
//
// This is a local-development facility. It binds loopback only, uses fixed
// credentials, and is never what a Kubernetes deployment should use: there
// the pod is stateless and Postgres is external.
package localdb

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

const (
	// pgVersion pins the server major version to the one the migrations in
	// migrations/postgres are exercised against (deploy/docker-compose.yml
	// runs postgres:16). Bumping it invalidates an existing data directory:
	// the runner compares PG_VERSION and re-runs initdb on a mismatch, which
	// silently discards everything the operator configured.
	pgVersion = embeddedpostgres.V16

	dbUser = "nexus"
	dbPass = "nexus"
	dbName = "nexus"

	// defaultPort is deliberately not 5432. A laptop running `nexus --local`
	// must not collide with the Postgres the developer already has, and a
	// collision here surfaces as an opaque pg_ctl failure.
	defaultPort    = 15432
	portProbeRange = 32

	defaultStartTimeout = 90 * time.Second
)

// Options configures a local database. StateDir is the only required field.
type Options struct {
	// StateDir is the parent of the "pg" directory holding the data
	// directory, the binary cache and the server log.
	StateDir string

	// BinariesPath points at a pre-installed Postgres distribution — the
	// directory whose bin/ holds initdb and pg_ctl. Empty means download the
	// binaries and cache them under StateDir, which is what a laptop does;
	// the container image sets it so `docker run` never reaches the network
	// for a database.
	BinariesPath string

	// Port is the preferred TCP port. Zero means defaultPort. The server
	// moves to the next free port if this one is taken.
	Port int

	// StartTimeout bounds initdb plus the first successful connection. The
	// default is generous because the first run on a cold cache also pays for
	// the binary download.
	StartTimeout time.Duration

	Log *slog.Logger
}

// DB is a running local Postgres. Stop it when the process exits; leaving it
// running orphans a postgres process that holds the data directory lock.
type DB struct {
	pg      *embeddedpostgres.EmbeddedPostgres
	url     string
	logPath string
	logFile *os.File
	log     *slog.Logger

	mu      sync.Mutex
	stopped bool
}

// Start initialises the data directory if needed and brings the server up.
// The returned DB is ready to accept connections at URL.
func Start(opts Options) (*DB, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	if opts.StateDir == "" {
		return nil, errors.New("localdb: StateDir is required")
	}

	root := filepath.Join(opts.StateDir, "pg")
	dataDir := filepath.Join(root, "data")
	// Note: dataDir is intentionally not created here. initdb wants to create
	// it, and the runner wipes and re-initialises whatever is there when
	// PG_VERSION does not match.
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("localdb: create %s: %w", root, err)
	}

	binaries := opts.BinariesPath
	if binaries == "" {
		// Extracted binaries live outside the runtime directory because the
		// runner deletes the runtime directory on every start; keeping them
		// here turns the second boot into a no-op instead of a re-extract.
		binaries = filepath.Join(root, "binaries-"+string(pgVersion))
	}

	if err := releaseDataDir(dataDir, binaries, log); err != nil {
		return nil, err
	}

	port, err := freePort(opts.Port)
	if err != nil {
		return nil, err
	}

	// Postgres is chatty and none of it is actionable on a good boot, so it
	// goes to a file rather than interleaving with the gateway's structured
	// log. Every error returned from here names the path.
	logPath := filepath.Join(root, "postgres.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("localdb: open %s: %w", logPath, err)
	}

	timeout := opts.StartTimeout
	if timeout <= 0 {
		timeout = defaultStartTimeout
	}

	cfg := embeddedpostgres.DefaultConfig().
		Version(pgVersion).
		Username(dbUser).
		Password(dbPass).
		Database(dbName).
		Port(uint32(port)).
		CachePath(filepath.Join(root, "cache")).
		BinariesPath(binaries).
		RuntimePath(filepath.Join(root, "run")).
		DataPath(dataDir).
		StartTimeout(timeout).
		Logger(logFile)

	if _, err := os.Stat(filepath.Join(binaries, "bin", "pg_ctl")); errors.Is(err, fs.ErrNotExist) && opts.BinariesPath == "" {
		log.Info("downloading the local Postgres runtime (first run only)",
			"version", string(pgVersion), "cache", filepath.Join(root, "cache"))
	}

	pg := embeddedpostgres.NewDatabase(cfg)
	if err := pg.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("localdb: start postgres on port %d (server log: %s): %w", port, logPath, err)
	}

	db := &DB{
		pg: pg,
		// sslmode=disable is correct rather than lax: the server is a child
		// process reachable only over loopback, and requiring TLS would mean
		// generating and trusting a certificate for a connection that cannot
		// leave the machine.
		url:     fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable", dbUser, dbPass, port, dbName),
		logPath: logPath,
		logFile: logFile,
		log:     log,
	}
	log.Info("local postgres ready", "port", port, "data_dir", dataDir, "server_log", logPath)
	return db, nil
}

// URL is the connection string for the running server.
func (d *DB) URL() string { return d.url }

// DataDir reports where the server keeps its state, for messages that tell an
// operator what to delete for a clean slate.
func (d *DB) DataDir() string { return filepath.Join(filepath.Dir(d.logPath), "data") }

// Stop shuts the server down. It is safe to call more than once, which
// matters because both the deferred cleanup and the signal path want to call
// it.
func (d *DB) Stop() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return nil
	}
	d.stopped = true

	err := d.pg.Stop()
	if d.logFile != nil {
		_ = d.logFile.Close()
	}
	if err != nil {
		return fmt.Errorf("localdb: stop postgres (server log: %s): %w", d.logPath, err)
	}
	d.log.Info("local postgres stopped")
	return nil
}

// releaseDataDir makes the data directory usable again after an unclean exit.
//
// pg_ctl detaches the server, so a gateway killed with SIGKILL — or a
// terminal closed on `npx @ffxnexus/nexus`, which sends SIGHUP — leaves
// postgres running and holding this directory. That is the common case, not
// an exotic one, and Postgres refuses to start a second server against a
// directory whose postmaster.pid names a live process.
//
// Two situations, two answers:
//
//   - The PID is gone. The file is debris; remove it.
//   - The PID is alive. It is our own orphan, because nothing else uses this
//     directory, so shut it down and start fresh. Adopting it instead would
//     mean owning a server we did not configure and cannot log.
//
// If the shutdown does not work we refuse rather than start anyway, because
// two servers on one data directory is how it gets corrupted.
func releaseDataDir(dataDir, binariesPath string, log *slog.Logger) error {
	pidPath := filepath.Join(dataDir, "postmaster.pid")
	raw, err := os.ReadFile(pidPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("localdb: read %s: %w", pidPath, err)
	}

	first, _, _ := strings.Cut(string(raw), "\n")
	pid, convErr := strconv.Atoi(strings.TrimSpace(first))
	if convErr != nil || pid <= 0 || !processAlive(pid) {
		log.Warn("clearing a stale postmaster.pid left by an unclean shutdown", "path", pidPath)
		if err := os.Remove(pidPath); err != nil {
			return fmt.Errorf("localdb: remove %s: %w", pidPath, err)
		}
		return nil
	}

	log.Warn("a postgres from a previous run still holds the local data directory; shutting it down",
		"pid", pid, "data_dir", dataDir)

	pgCtl := filepath.Join(binariesPath, "bin", "pg_ctl")
	if _, statErr := os.Stat(pgCtl); statErr != nil {
		return fmt.Errorf(
			"localdb: postgres (pid %d) still holds %s and %s is missing to stop it — kill the process, or point this run at a different NEXUS_LOCAL_STATE_DIR",
			pid, dataDir, pgCtl)
	}

	// -m fast rolls back open transactions instead of waiting for clients
	// that are never coming back; -w waits for the shutdown to finish so the
	// port and the lock file are free by the time we return.
	cmd := exec.Command(pgCtl, "stop", "-D", dataDir, "-m", "fast", "-w", "-t", "30")
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		return fmt.Errorf(
			"localdb: could not stop the postgres (pid %d) holding %s: %w\n%s",
			pid, dataDir, runErr, strings.TrimSpace(string(out)))
	}
	return nil
}

// processAlive reports whether pid names a running process. A permission
// error counts as alive: the process exists, it just belongs to someone else.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}

// freePort returns the first bindable port at or above preferred.
func freePort(preferred int) (int, error) {
	if preferred <= 0 {
		preferred = defaultPort
	}
	for p := preferred; p < preferred+portProbeRange; p++ {
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			continue
		}
		_ = l.Close()
		return p, nil
	}
	return 0, fmt.Errorf("localdb: no free TCP port between %d and %d for the local database",
		preferred, preferred+portProbeRange-1)
}
