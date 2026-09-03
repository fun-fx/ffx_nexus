package main

import (
	"log/slog"
	"testing"

	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/console"
	"github.com/ffxnexus/nexus/internal/evals"
	"github.com/ffxnexus/nexus/internal/observability"
)

// These tests cover the wiring decision D-2c.3 rests on: exactly one branch
// of the trace fan-out retains, and exactly that branch sits behind the
// capture gate.
//
// A mistake here is invisible in production. The gateway serves traffic
// identically whether or not the gate is attached; the only difference is
// whether months of customer prompts accumulate in ClickHouse. So the
// wiring is asserted rather than reviewed.
//
// traceFanout is given a zero-value CHRecorder and a zero-value Worker.
// Nothing here calls Record — these tests are about the shape of the list,
// and the shape is what decides retention.

func fanout(t *testing.T, captureEnabled bool) []observability.Recorder {
	t.Helper()
	cfg := config.Config{CaptureTraceContent: captureEnabled}
	return traceFanout(cfg, console.NewHub(), &observability.CHRecorder{}, nil, nil,
		&evals.Worker{}, slog.New(slog.DiscardHandler))
}

// TestTraceFanout_GatesClickHouseByDefault: with no operator configuration
// at all, the ClickHouse recorder is behind a gate. This is the default-off
// promise; if it regresses, a fresh install silently starts retaining bodies.
func TestTraceFanout_GatesClickHouseByDefault(t *testing.T) {
	var gates int
	for _, r := range fanout(t, false) {
		gate, ok := r.(*observability.CaptureGate)
		if !ok {
			continue
		}
		gates++
		if _, ok := gate.Unwrap().(*observability.CHRecorder); !ok {
			t.Errorf("a CaptureGate wraps %T; the gate belongs on the recorder that retains", gate.Unwrap())
		}
	}
	if gates != 1 {
		t.Fatalf("found %d CaptureGate recorders, want exactly 1 (the ClickHouse branch)", gates)
	}
}

// TestTraceFanout_ClickHouseIsNeverUngated states the same requirement from
// the other side, so that appending chRec directly alongside the gate — the
// likeliest way to reintroduce the leak while leaving the test above green —
// fails too.
func TestTraceFanout_ClickHouseIsNeverUngated(t *testing.T) {
	for _, r := range fanout(t, false) {
		if _, ok := r.(*observability.CHRecorder); ok {
			t.Fatal("the ClickHouse recorder appears in the fan-out unwrapped while capture is off; bodies will be persisted")
		}
	}
}

// TestTraceFanout_EvalWorkerIsNeverGated is the other half of the decision.
// The judge and remote evaluators return without a score when the prompt or
// completion is blank, so gating this branch would trade a silently broken
// eval pipeline for the storage guarantee. Both properties hold at once only
// because MultiRecorder hands each recorder its own copy of the Trace.
func TestTraceFanout_EvalWorkerIsNeverGated(t *testing.T) {
	for _, captureEnabled := range []bool{false, true} {
		var sawWorker bool
		for _, r := range fanout(t, captureEnabled) {
			if _, ok := r.(*evals.Worker); ok {
				sawWorker = true
			}
			gate, ok := r.(*observability.CaptureGate)
			if !ok {
				continue
			}
			if _, ok := gate.Unwrap().(*evals.Worker); ok {
				t.Errorf("capture=%v: the eval worker is behind a CaptureGate; evaluators cannot score a trace with blank bodies", captureEnabled)
			}
		}
		if !sawWorker {
			t.Fatalf("capture=%v: no *evals.Worker in the fan-out, so this test proves nothing", captureEnabled)
		}
	}
}

// TestTraceFanout_OptInRemovesTheGate: an operator who sets
// NEXUS_CAPTURE_TRACE_CONTENT=true gets the ClickHouse recorder directly,
// with nothing left in the path to strip anything.
func TestTraceFanout_OptInRemovesTheGate(t *testing.T) {
	var sawCH bool
	for _, r := range fanout(t, true) {
		if _, ok := r.(*observability.CaptureGate); ok {
			t.Error("capture is enabled but a CaptureGate is still wired in")
		}
		if _, ok := r.(*observability.CHRecorder); ok {
			sawCH = true
		}
	}
	if !sawCH {
		t.Fatal("the ClickHouse recorder is missing from the fan-out entirely")
	}
}

// TestTraceFanout_SkipsNilRecorders guards the interface-nil trap the
// refactor introduced. metricsRec, otlpRec, and evalWorker are all optional;
// if a nil concrete pointer were appended, the interface value would be
// non-nil, MultiRecorder would keep it, and the first request would panic in
// the trace path rather than at boot.
func TestTraceFanout_SkipsNilRecorders(t *testing.T) {
	hub := console.NewHub()
	got := traceFanout(config.Config{}, hub, nil, nil, nil, nil, slog.New(slog.DiscardHandler))

	if len(got) != 1 {
		t.Fatalf("fan-out with no optional recorders has %d entries, want 1 (the hub): %#v", len(got), got)
	}
	if got[0] != observability.Recorder(hub) {
		t.Fatalf("the single recorder is %T, want the console hub", got[0])
	}
}
