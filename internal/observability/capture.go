package observability

import "context"

// contentFieldCount is the number of Trace fields CaptureGate strips. It
// exists so a test can assert the gate was updated when a new raw-content
// field is added to Trace, rather than discovering the omission when the
// field shows up in a customer's ClickHouse table.
const contentFieldCount = 3

// CaptureGate wraps a Recorder and removes customer content from the Trace
// before the wrapped Recorder sees it.
//
// The reason it is a wrapper and not a check inside the gateway is that the
// two consumers of a Trace want opposite things. In-process evaluators need
// the prompt and completion — internal/evals/judge.go and remote.go return
// without a score when either is empty — while durable storage should keep
// them only if the operator asked. MultiRecorder hands every recorder its
// own copy by value, so those consumers are siblings rather than a chain,
// and wrapping one branch leaves the other untouched. Gating at trace
// construction instead would have disabled evaluation to secure storage,
// which is a feature regression wearing a security label.
//
// Wrap the recorders that retain. Do not wrap the eval worker.
//
// Fail-closed: NewCaptureGate wraps unless capture is explicitly enabled,
// so a caller that forgets to thread the config through gets the private
// default rather than the leaking one.
type CaptureGate struct {
	inner Recorder
}

// NewCaptureGate returns inner unchanged when capture is enabled, and a gate
// that strips content when it is not. Returns nil for a nil inner so it can
// be dropped straight into a recorder list that tolerates nil entries.
func NewCaptureGate(inner Recorder, captureEnabled bool) Recorder {
	if inner == nil {
		return nil
	}
	if captureEnabled {
		return inner
	}
	return &CaptureGate{inner: inner}
}

// Record strips every raw-content field and forwards the metadata.
//
// Trace is passed by value, so blanking fields here cannot affect any other
// recorder in the fan-out. That property is what the gate depends on, and
// compose_capture_test.go asserts it end to end rather than trusting it.
func (g *CaptureGate) Record(t Trace) {
	// InputMessages and OutputMessages are the request and response bodies.
	t.InputMessages = ""
	t.OutputMessages = ""
	// RetrievalContexts is caller-supplied RAG source material — documents
	// the customer chose to send, which makes it content, not telemetry.
	t.RetrievalContexts = ""

	// Two fields are deliberately left alone.
	//
	// EvalReference is the fourth and last content column CHRecorder.insert
	// writes. It is a ground-truth label the caller attaches for scoring
	// rather than conversation content, and the console reads it back to
	// explain a score. Stripping it would break score explanation and
	// retain nothing the customer did not author as an annotation.
	//
	// EvalMetadata is not written by CHRecorder.insert at all, so it does
	// not reach durable storage on this path. It is also the one map field
	// on Trace: clearing it here would be safe, but mutating its contents
	// would reach through the by-value copy into every sibling recorder.
	// A future recorder that persists EvalMetadata must replace the map
	// rather than edit it, and must extend contentFieldCount's guard.
	g.inner.Record(t)
}

// Close closes the wrapped Recorder.
func (g *CaptureGate) Close(ctx context.Context) error { return g.inner.Close(ctx) }

// Unwrap exposes the wrapped Recorder for tests that need to assert what the
// gate is protecting. Not part of the Recorder contract.
func (g *CaptureGate) Unwrap() Recorder { return g.inner }
