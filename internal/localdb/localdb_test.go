package localdb

import (
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// A second run of the one-liner must decrypt what the first one stored, which
// only holds if the key is written once and read back afterwards. A
// regenerated key is not a visible failure at boot — it surfaces later as
// provider credentials nobody can read.
func TestMasterKeyPersistsAcrossCalls(t *testing.T) {
	dir := t.TempDir()

	first, err := MasterKey(dir)
	if err != nil {
		t.Fatalf("MasterKey: %v", err)
	}
	raw, err := hex.DecodeString(first)
	if err != nil {
		t.Fatalf("generated key is not hex: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("generated key decodes to %d bytes, want 32", len(raw))
	}

	second, err := MasterKey(dir)
	if err != nil {
		t.Fatalf("MasterKey (second call): %v", err)
	}
	if second != first {
		t.Fatalf("key changed between calls: %q then %q", first, second)
	}

	info, err := os.Stat(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatalf("stat master.key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("master.key mode is %o, want 600", perm)
	}
}

func TestMasterKeyRegeneratesWhenFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if err := os.WriteFile(path, []byte("  \n"), 0o600); err != nil {
		t.Fatalf("seed empty key: %v", err)
	}

	key, err := MasterKey(dir)
	if err != nil {
		t.Fatalf("MasterKey: %v", err)
	}
	if strings.TrimSpace(key) == "" {
		t.Fatal("MasterKey returned an empty key for an empty file")
	}
}

func TestStateDirCreatesAndResolves(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "nested", "state")

	got, err := StateDir(target)
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	if got != target {
		t.Fatalf("StateDir returned %q, want %q", got, target)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("StateDir did not create %s: %v", target, err)
	}
}

func TestReleaseDataDirRemovesDeadPID(t *testing.T) {
	dir := t.TempDir()
	// PID 0 is never a live process id in the sense Signal understands, so it
	// stands in for "the writer is gone".
	if err := os.WriteFile(filepath.Join(dir, "postmaster.pid"), []byte("0\n/tmp/data\n"), 0o600); err != nil {
		t.Fatalf("seed pid file: %v", err)
	}

	if err := releaseDataDir(dir, t.TempDir(), quietLogger()); err != nil {
		t.Fatalf("releaseDataDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "postmaster.pid")); !os.IsNotExist(err) {
		t.Fatalf("stale pid file survived: %v", err)
	}
}

// A live holder gets shut down with pg_ctl. When pg_ctl is not there to do
// it, starting anyway would put two servers on one data directory, so the
// only acceptable outcome is an error that names the process.
func TestReleaseDataDirRefusesLivePIDWithoutPgCtl(t *testing.T) {
	dir := t.TempDir()
	self := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(filepath.Join(dir, "postmaster.pid"), []byte(self+"\n"), 0o600); err != nil {
		t.Fatalf("seed pid file: %v", err)
	}

	err := releaseDataDir(dir, filepath.Join(t.TempDir(), "no-binaries"), quietLogger())
	if err == nil {
		t.Fatal("releaseDataDir accepted a data dir held by a live process it could not stop")
	}
	if !strings.Contains(err.Error(), self) {
		t.Fatalf("error does not name the holding pid: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "postmaster.pid")); statErr != nil {
		t.Fatalf("pid file of a live process was removed: %v", statErr)
	}
}

// The orphan case is the common one — closing the terminal on `npx` sends
// SIGHUP, the gateway dies, and the detached postgres keeps running — so the
// next start must invoke pg_ctl stop rather than give up.
func TestReleaseDataDirStopsLiveHolder(t *testing.T) {
	dir := t.TempDir()
	self := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(filepath.Join(dir, "postmaster.pid"), []byte(self+"\n"), 0o600); err != nil {
		t.Fatalf("seed pid file: %v", err)
	}

	binaries := t.TempDir()
	if err := os.MkdirAll(filepath.Join(binaries, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	recordPath := filepath.Join(binaries, "invoked")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + recordPath + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binaries, "bin", "pg_ctl"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write pg_ctl stub: %v", err)
	}

	if err := releaseDataDir(dir, binaries, quietLogger()); err != nil {
		t.Fatalf("releaseDataDir: %v", err)
	}

	args, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("pg_ctl was never invoked: %v", err)
	}
	for _, want := range []string{"stop", dir, "fast"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("pg_ctl invoked as %q, missing %q", strings.TrimSpace(string(args)), want)
		}
	}
}

func TestReleaseDataDirNoFile(t *testing.T) {
	if err := releaseDataDir(t.TempDir(), t.TempDir(), quietLogger()); err != nil {
		t.Fatalf("releaseDataDir on a fresh dir: %v", err)
	}
}

func TestFreePortSkipsBoundPort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	bound := l.Addr().(*net.TCPAddr).Port
	got, err := freePort(bound)
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	if got == bound {
		t.Fatalf("freePort returned the occupied port %d", got)
	}

	probe, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(got)))
	if err != nil {
		t.Fatalf("freePort returned unbindable port %d: %v", got, err)
	}
	_ = probe.Close()
}
