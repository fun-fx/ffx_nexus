package evals

import (
	"context"
	"sync"
	"time"

	"github.com/ffxnexus/nexus/internal/observability"
)

// TraceBatch is a window of traces sent to the Python sidecar in a
// single /evaluate call. PR #136 introduces this as an opt-in
// alternative to per-trace Evaluate calls. The batcher is the only
// component that batches — the worker's evaluator fan-out stays
// per-trace so cheap heuristics still finish fast and a slow sidecar
// cannot starve cheap heuristics.
type TraceBatch struct {
	Traces   []observability.Trace
	Profiles []EvalProfile
}

// BatchConfig tunes the sidecar batcher.
type BatchConfig struct {
	// MaxSize is the upper bound on traces per request. Default 32.
	MaxSize int
	// Window is the maximum time a partial batch waits before
	// flushing. Default 250ms. Smaller => lower latency, bigger => fewer
	// sidecar round trips; we tune conservatively so per-trace median
	// latency doesn't visibly regress for low-volume tenants.
	Window time.Duration
	// Capacity of the in-flight channel; bound so a stuck sidecar
	// backs error early rather than crashing the worker.
	Capacity int
}

// ApplyDefaults normalises a BatchConfig with safe defaults.
func (c *BatchConfig) ApplyDefaults() {
	if c.MaxSize <= 0 {
		c.MaxSize = 32
	}
	if c.Window == 0 {
		c.Window = 250 * time.Millisecond
	}
	if c.Capacity <= 0 {
		c.Capacity = 8
	}
}

// Batcher collects traces and flushes them as TraceBatches.
//
// Profile snapshot is captured at flush time so a runtime-controller
// PATCH that swaps profiles mid-batch is observed on the next flush.
// A mutex-protected push guarantees that the receiver sees a coherent
// slice even when the controller updates `profiles` in parallel.
type Batcher struct {
	cfg BatchConfig
	in  chan observability.Trace
	// profilesSnapshot is the current EvalProfile set applied to the
	// next flush. Replace via SetProfiles. (Controlled by Batcher
	// directly, since the runtime controller already supports swap
	// snapshots cleanly here.)
	profilesSnapshot []EvalProfile
	profilesMu       sync.RWMutex

	flush  chan TraceBatch
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewBatcher constructs (but does not start) a trace batcher.
func NewBatcher(cfg BatchConfig) *Batcher {
	cfg.ApplyDefaults()
	return &Batcher{
		cfg:    cfg,
		in:     make(chan observability.Trace, cfg.Capacity*cfg.MaxSize),
		flush:  make(chan TraceBatch, cfg.Capacity),
		stopCh: make(chan struct{}),
	}
}

// SetProfiles atomically replaces the profile snapshot readers see
// during the next flush. Mirrors Worker.ReplaceProfiles so the
// runtime controller wires both call sites with the same payload.
func (b *Batcher) SetProfiles(p []EvalProfile) {
	cp := make([]EvalProfile, 0, len(p))
	for i := range p {
		cp = append(cp, *p[i].Clone())
	}
	b.profilesMu.Lock()
	b.profilesSnapshot = cp
	b.profilesMu.Unlock()
}

// Start runs a single background goroutine that fills batches by
// either size (MaxSize reached) or time (Window elapsed). The flusher
// pushes to `flush` which the dispatch loop consumes.
func (b *Batcher) Start(onFlush func(context.Context, TraceBatch)) {
	if onFlush == nil {
		return
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.run(onFlush)
	}()
}

// Stop halts the batcher and waits for the in-flight flush to drain.
func (b *Batcher) Stop(ctx context.Context) {
	close(b.stopCh)
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Submit offers a trace. Non-blocking; drops the trace if the buffer
// is full so the eval pipeline never back-pressures requests.
func (b *Batcher) Submit(t observability.Trace) {
	if t.StatusCode != 200 {
		return
	}
	select {
	case b.in <- t:
	default:
		// Drop. The dispatch loop logged the loss in worker.go's
		// existing buffer-full path; this is the same behaviour for
		// a different buffer.
	}
}

func (b *Batcher) run(onFlush func(context.Context, TraceBatch)) {
	buf := make([]observability.Trace, 0, b.cfg.MaxSize)
	timer := time.NewTimer(b.cfg.Window)
	defer timer.Stop()
	for {
		select {
		case <-b.stopCh:
			if len(buf) > 0 {
				b.flushBatch(buf, onFlush)
			}
			return
		case t := <-b.in:
			buf = append(buf, t)
			if len(buf) >= b.cfg.MaxSize {
				b.flushBatch(buf, onFlush)
				buf = buf[:0]
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(b.cfg.Window)
			}
		case <-timer.C:
			if len(buf) > 0 {
				b.flushBatch(buf, onFlush)
				buf = buf[:0]
			}
			timer.Reset(b.cfg.Window)
		}
	}
}

func (b *Batcher) flushBatch(buf []observability.Trace, onFlush func(context.Context, TraceBatch)) {
	b.profilesMu.RLock()
	profiles := append([]EvalProfile(nil), b.profilesSnapshot...)
	b.profilesMu.RUnlock()
	cp := make([]observability.Trace, len(buf))
	copy(cp, buf)
	onFlush(context.Background(), TraceBatch{Traces: cp, Profiles: profiles})
}
