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
