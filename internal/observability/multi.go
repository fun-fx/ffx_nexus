package observability

import "context"

// MultiRecorder fans a trace out to several recorders (e.g. ClickHouse for
// persistence plus a live WebSocket hub for the dashboard).
type MultiRecorder struct {
	recorders []Recorder
}

// NewMultiRecorder composes recorders. Nil entries are ignored.
func NewMultiRecorder(recorders ...Recorder) *MultiRecorder {
	out := make([]Recorder, 0, len(recorders))
	for _, r := range recorders {
		if r != nil {
			out = append(out, r)
		}
	}
	return &MultiRecorder{recorders: out}
}

// Recorders returns the composed recorders in fan-out order.
//
// This exists so a test can assert which branches of the fan-out sit behind
// a CaptureGate. Whether prompts reach durable storage is decided by the
// shape of this list, and a wiring mistake there is silent at runtime: the
// gateway serves traffic exactly as before and the bodies simply land in
// ClickHouse. Making the list readable lets cmd/nexus check the wiring
// instead of trusting it. The slice is a copy, so a caller cannot reorder
// or replace the live fan-out.
func (m *MultiRecorder) Recorders() []Recorder {
	out := make([]Recorder, len(m.recorders))
	copy(out, m.recorders)
	return out
}

// Record forwards the trace to every recorder.
func (m *MultiRecorder) Record(t Trace) {
	for _, r := range m.recorders {
		r.Record(t)
	}
}

// Close closes every recorder, returning the first error.
func (m *MultiRecorder) Close(ctx context.Context) error {
	var firstErr error
	for _, r := range m.recorders {
		if err := r.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
