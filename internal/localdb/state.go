package localdb

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// StateDir resolves and creates the directory local mode keeps its data in:
// the Postgres data directory, the binary cache, and the master key.
//
// An empty argument means ~/.nexus. The container image passes /app/data so a
// single `-v` mount preserves everything across `docker run` invocations.
func StateDir(configured string) (string, error) {
	dir := strings.TrimSpace(configured)
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("localdb: resolve home directory for the state dir (set NEXUS_LOCAL_STATE_DIR): %w", err)
		}
		dir = filepath.Join(home, ".nexus")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("localdb: resolve %s: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", fmt.Errorf("localdb: create state dir %s: %w", abs, err)
	}
	return abs, nil
}

// MasterKey returns the credential-encryption key for local mode, generating
// one on first run and persisting it in the state directory.
//
// Generating it is the only way the one-liner can encrypt provider keys at
// all: with NEXUS_MASTER_KEY unset the gateway starts with encryption
// disabled and refuses to store credentials, which is exactly the step the
// quickstart asks the user to perform. Persisting it is what makes the second
// `npx` run able to read what the first one wrote — a fresh key would leave
// every stored credential undecryptable.
func MasterKey(stateDir string) (string, error) {
	path := filepath.Join(stateDir, "master.key")

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if key := strings.TrimSpace(string(raw)); key != "" {
			return key, nil
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("localdb: read %s: %w", path, err)
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("localdb: generate master key: %w", err)
	}
	key := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("localdb: write %s: %w", path, err)
	}
	return key, nil
}
