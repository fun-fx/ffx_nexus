package heuristic

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ffxnexus/nexus/internal/evals"
	"github.com/ffxnexus/nexus/internal/observability"
)

// PythonClient wraps a long-lived Python subprocess that runs the
// HF Evaluate / LightEval / Ragas metric implementations off-process.
// The subprocess protocol is line-delimited JSON: the Go side writes
// one request per line, the Python side writes one result per line.
//
// Why subprocess and not in-process? Two reasons:
//
//  1. HF Evaluate, LightEval and Ragas all import numpy and pytorch.
//     A Nexus process that ever imports torch inflates its cold-start
//     OOM class from ~80 MiB to >400 MiB and breaks the
//     "ship-by-default simple" promise.
//  2. Ragas and LightEval both currently require Python 3.10+ which
//     pinches the binary builder's available interpreters.
//
// Subprocess has a downside: cold-start latency (~700 ms on first
// run when the venv is cold). For now we treat that as a one-time
// hit and amortise across all subsequent in-pod metric runs. If
// cold start becomes a growth concern the long-term answer is a
// Python sidecar container — same protocol, separate lifecycle.
//
// py/eval_metric.py is the corresponding entry point. It is shipped
// next to this file under the same import path so Go tests can
// exercise the subprocess workflow without writing Python inline.
//
// Protocol:
//
//	request:
//
//	  {"id":"<trace-id>","metric":"hf_evaluate",
//	   "args":{"name":"exact_match"},
//	   "input":"the prediction text","reference":"the reference"}
//
//	reply:
//
//	  {"id":"<trace-id>","value":1.0,"label":"pass","duration_ms":3}
//
//	error:
//
//	  {"id":"<trace-id>","error":"name 'exact_match' is not defined"}
type PythonClient struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	errs chan error
	done chan struct{}

	// softTimeoutMs bounds per-call latency inside the subprocess
	// (ms). Above this, the call returns an error and does not
	// attempt to bounce the subprocess (the next call is fine).
	softTimeoutMs int
}

// pythonScriptPath is the in-tree script path. It is loaded via
// //go:embed in python.go (next to this file) — embedding keeps
// `git clone` of the whole repo self-contained.
const pythonScriptPath = "py/eval_metric.py"

var errPythonNotWired = errors.New("heuristic Python subprocess is not wired up in this build (set PYTHON_SUBPROCESS=1 to enable)")

// EvaluatePython is the dispatcher entry point that routes
// hf_evaluate / lighteval / ragas through the Python subprocess.
// It is a no-op (returns errPythonNotWired) when the subprocess is
// not configured so an in-process build never accidentally pipes
// text through Python.
//
// `args` may include a "name" key that selects the HF / LightEval
// / Ragas metric implementation. Additional args are forwarded
// verbatim so a manifest can override thresholds etc.
func EvaluatePython(ctx context.Context, kind string, args map[string]any, t observability.Trace) ([]evals.Score, error) {
	if !isPythonWired() {
		return nil, errPythonNotWired
	}
	name, hasName := args["name"].(string)
	if !hasName || name == "" {
		return nil, fmt.Errorf("%s: args.name is required", kind)
	}
	client, err := startPythonClient()
	if err != nil {
		return nil, fmt.Errorf("start python subprocess: %w", err)
	}
	defer func() { _ = client.close() }()

	req := map[string]any{
		"id":        t.TraceID,
		"metric":    kind,
		"name":      name,
		"args":      args,
		"input":     resultOutput(t),
		"reference": stringArg(args, "reference", t.EvalReference),
	}
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	if _, err := client.in.Write(append(reqJSON, '\n')); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}
	// Per request timeout. Use a 5s soft cap so a runaway Python
	// loop is surfaced before it eats the eval worker.
	deadline := time.Duration(client.softTimeoutMs) * time.Millisecond
	if deadline == 0 {
		deadline = 5 * time.Second
	}
	tctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	type result struct {
		line string
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		line, err := client.out.ReadString('\n')
		resultCh <- result{line, err}
	}()
	select {
	case <-tctx.Done():
		return nil, fmt.Errorf("%s: timed out after %v", kind, deadline)
	case r := <-resultCh:
		if r.err != nil {
			return nil, fmt.Errorf("%s: read result: %w", kind, r.err)
		}
		var reply struct {
			ID         string  `json:"id"`
			Value      float64 `json:"value"`
			Label      string  `json:"label"`
			DurationMs int64   `json:"duration_ms"`
			Error      string  `json:"error"`
		}
		if err := json.Unmarshal([]byte(r.line), &reply); err != nil {
			return nil, fmt.Errorf("%s: parse reply %q: %w", kind, strings.TrimSpace(r.line), err)
		}
		if reply.ID != t.TraceID {
			return nil, fmt.Errorf("%s: reply id mismatch (got %q want %q)", kind, reply.ID, t.TraceID)
		}
		if reply.Error != "" {
			return nil, fmt.Errorf("%s: %s", kind, reply.Error)
		}
		passed := reply.Label == "pass"
		return []evals.Score{{
			TraceID:   traceID(t),
			Evaluator: "heuristic_" + kind,
			Metric:    name,
			Score:     reply.Value,
			Passed:    passed,
			Rationale: fmt.Sprintf("duration_ms=%d", reply.DurationMs),
		}}, nil
	}
}

// isPythonWired returns true if PYTHON_SUBPROCESS=1 was set at process
// start. By default we never launch the subprocess: this guarantees
// a vanilla Go binary keeps working without Python installed.
func isPythonWired() bool {
	// Avoid pulling os.Getenv into a hot import path; evaluate on
	// each call (cheap) and let the OS string-share the buffer.
	v, _ := lookupEnv("PYTHON_SUBPROCESS")
	return v == "1"
}

// lookupEnv is a thin wrapper around os.LookupEnv so tests can swap
// the function (we don't want to actually hit os.Getenv in tests).
var lookupEnv = realLookupEnv

func realLookupEnv(k string) (string, bool) {
	return os.LookupEnv(k)
}

// startPythonClient spawns the embedded Python entry point. The
// implementation lives in python.go because it touches go:embed
// and the subprocess plumbing.
//
// This wrapper is intentionally fail-fast: a missing python3 is a
// hard error rather than an empty success.
func startPythonClient() (*PythonClient, error) {
	c, err := startPythonSubprocess()
	if err != nil {
		return nil, err
	}
	c.softTimeoutMs = 5000
	return c, nil
}

func (c *PythonClient) close() error {
	if c == nil || c.cmd == nil {
		return nil
	}
	_ = c.in.Close()
	if c.done != nil {
		<-c.done
	}
	return c.cmd.Process.Kill()
}
