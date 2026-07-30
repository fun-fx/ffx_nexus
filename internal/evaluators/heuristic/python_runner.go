package heuristic

import (
	"bufio"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// evalMetricScript is the Python entry point the subprocess runs.
// Embedded at compile time so a single binary deploy needs nothing
// else in the image (the `python3` interpreter itself is the only
// runtime requirement and is documented in the install guide).
//
//go:embed py/eval_metric.py
var evalMetricScript []byte

// startPythonSubprocess writes the embedded script to a temp file
// and launches `python3 <path>` on it. The returned client has the
// stdin pipe ready for line-delimited JSON requests and the stdout
// reader configured to consume line-delimited JSON replies.
//
// Failure modes, in order of likelihood:
//
//   - python3 not on PATH: error returns from exec.LookPath.
//     dispatch caller will treat as ErrPythonNotWired at the next
//     request boundary.
//   - python3 starts but exits early (e.g. syntax error in the
//     embedded script): the errs goroutine observes a non-nil
//     error and ReadString returns io.EOF; dispatch surfaces that
//     to the operator so a rollback to a known-good image is
//     straightforward.
//   - python3 hangs: callers carry a soft deadline so we never
//     wedge the eval worker (PythonClient.softTimeoutMs is plumbed
//     by EvaluatePython).
func startPythonSubprocess() (*PythonClient, error) {
	python, err := exec.LookPath("python3")
	if err != nil {
		return nil, fmt.Errorf("python3 not on PATH: %w", err)
	}
	// Write the embedded script to a temp file so the subprocess
	// can import relative modules. We name it eval_metric-<pid>.py
	// so two concurrent tests don't overwrite each other.
	tmp, err := os.CreateTemp("", "eval_metric-*.py")
	if err != nil {
		return nil, fmt.Errorf("create temp: %w", err)
	}
	if _, err := tmp.Write(evalMetricScript); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("close temp: %w", err)
	}
	cmd := exec.Command(python, tmp.Name())
	in, err := cmd.StdinPipe()
	if err != nil {
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = in.Close()
		_ = outPipe.Close()
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("start subprocess: %w", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
		_ = os.Remove(tmp.Name())
	}()
	return &PythonClient{
		cmd:  cmd,
		in:   in,
		out:  bufio.NewReader(outPipe),
		errs: make(chan error, 1),
		done: done,
	}, nil
}

// pythonBinaryPath returns the python3 path the next subprocess
// will use. Exposed for tests so they can verify the wiring without
// spawning (they should set PATH to a tempdir with a fake python3).
func pythonBinaryPath() (string, error) {
	return exec.LookPath("python3")
}

// moduleRoot is the absolute path of the heuristic package's
// directory. Tests use it to validate the embedded script path
// resolves to py/eval_metric.py on disk via go:embed.
func moduleRoot() string {
	// heuristic is package "heuristic"; moduleRoot is the directory
	// containing this go file. filepath.Abs guarantees a stable
	// path even when tests are run with `go test ./...`.
	abs, _ := filepath.Abs(".")
	return strings.TrimSuffix(abs, "/_test") + "/"
}
