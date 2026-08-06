package external

import (
	"container/list"
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/observability"
)

// dispatchFn is the single Dispatch entry point the Scheduler
// routes plugin traces through. Production code wires
// d.Dispatch; tests wire a counting noop.
type dispatchFn func(ctx context.Context, t observability.Trace, p *evalplugin.Plugin) error

// Scheduler holds the per-plugin buffer for Send.Trigger values
// other than on_trace, plus the goroutine that drains scheduled
// triggers on the plugin's Collect.Interval. The dispatcher's hot
// path calls Enqueue() for scheduled triggers; the admin REST
// endpoint calls FireManual() when an operator presses the
// "run now" button.
//
// Buffering policy: each plugin has its own bounded list. Enqueue()
// returns ErrBufferFull when the per-plugin cap is reached — the
// MultiEvaluator logs the drop instead of blocking the eval worker,
// because eval is best-effort and the cost of a delayed trace is
// lower than the cost of stalling the gateway.
//
// Concurrency model: one goroutine per registered scheduled plugin
// driven by time.Ticker; the admin REST caller drives FireManual
// directly. All access funnels through Scheduler.mu; both drain paths
// pop the buffer under lock so a vendor that times out cannot cause
// a duplicate send on the next tick.
type Scheduler struct {
	dispatch dispatchFn

	mu        sync.Mutex
	buffers   map[string]*pluginBuffer
	schedules map[string]*scheduledFlush
	// stopped latches on Stop(). Stop releases s.mu before it joins the
	// flush goroutines, so an in-flight sweep tick or a concurrent
	// Enqueue can still acquire the lock after the maps were cleared.
	// Every entry point checks this under s.mu and bails out instead of
	// writing to a nil map, which would panic the process.
	stopped bool

	defaultCap int
	sweepEvery time.Duration
	log        *slog.Logger
	wg         sync.WaitGroup
	cancel     context.CancelFunc
}

// SchedulerConfig tunes the per-plugin buffering policy.
type SchedulerConfig struct {
	// MaxBufferPerPlugin bounds the trace list per plugin. Zero
	// means defaultBufferCap. Drops beyond this return ErrBufferFull
	// so the dispatcher can surface them as audit events.
	MaxBufferPerPlugin int
	// SweepInterval controls how often the scheduler re-reads the
	// registry to add/remove/refresh scheduled goroutines. Set to
	// zero to use the default. The granularity matters only when
	// operators rotate plugins at runtime — at boot, the registry
	// snapshot settles before Start() returns.
	SweepInterval time.Duration
}

// Errors surfaced from the Scheduler so MultiEvaluator can decide
// whether to log-and-continue (overflow) or escalate (nil plugin).
var ErrBufferFull = errors.New("eval plugin scheduler buffer full")

// ErrSchedulerStopped is returned by Enqueue/FireManual/FireScheduled once
// Stop has run. Shutdown is not an error state for the gateway — the eval
// worker logs the drop and moves on — but the caller needs to distinguish
// "buffered" from "discarded because we're draining".
var ErrSchedulerStopped = errors.New("eval plugin scheduler stopped")

// defaultBufferCap bounds the per-plugin queue. Sized for "vendor
// outage pushback" at typical production trace rates — a single
// plugin at 100 traces/s with a 60s flush window is comfortably under
// this cap.
const defaultBufferCap = 4096

// schedulerSweepInterval is how often the background goroutine
// re-reads the registry to add/remove scheduled flushers. Coarse on
// purpose — plugin add/remove happens only at boot or after the
// reconcile loop, both of which are visible at parse time.
const schedulerSweepInterval = 15 * time.Second

// pluginBuffer is a per-plugin bounded queue.
type pluginBuffer struct {
	ll      *list.List
	cap     int
	dropped int64
}

// scheduledFlush tracks a goroutine that drains one plugin's buffer
// on the plugin's Collect.Interval.
type scheduledFlush struct {
	plugin   *evalplugin.Plugin
	interval time.Duration
	cancel   context.CancelFunc
}

// NewScheduler constructs an idle Scheduler. dispatch is the same
// Dispatcher.Dispatch function the multi-evaluator uses; tests pass a
// counting stub.
func NewScheduler(dispatch dispatchFn, cfg SchedulerConfig) *Scheduler {
	cap := cfg.MaxBufferPerPlugin
	if cap <= 0 {
		cap = defaultBufferCap
	}
	sweep := cfg.SweepInterval
	if sweep <= 0 {
		sweep = schedulerSweepInterval
	}
	return &Scheduler{
		dispatch:   dispatch,
		buffers:    make(map[string]*pluginBuffer),
		schedules:  make(map[string]*scheduledFlush),
		defaultCap: cap,
		sweepEvery: sweep,
	}
}

// AttachLogger installs an optional logger for drop/reconcile events.
func (s *Scheduler) AttachLogger(l *slog.Logger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = l
}

// Start walks the registry once to schedule every scheduled-trigger
// plugin, then launches a background goroutine that re-walks every
// SweepInterval so plugin rotation is picked up without a restart.
func (s *Scheduler) Start(ctx context.Context, reg *evalplugin.Registry) {
	s.mu.Lock()
	if s.cancel != nil {
		// already running
		s.mu.Unlock()
		return
	}
	if s.stopped {
		// Stop cleared the buffers; restarting would sweep against nil
		// maps. Callers needing restart semantics build a new instance.
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()
	s.reconcile(reg)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.sweepEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.reconcile(reg)
			}
		}
	}()
}

// reconcile brings the Scheduler up to date with a registry snapshot.
// Plugins whose trigger is `scheduled` get a goroutine per plugin;
// plugins whose trigger is anything else are not touched (manual
// plugins don't need a goroutine — the admin REST drives them).
func (s *Scheduler) reconcile(reg *evalplugin.Registry) {
	enabled := reg.Enabled()
	wanted := make(map[string]evalplugin.Record, len(enabled))
	for _, rec := range enabled {
		if rec.Plugin.Spec.Send.Trigger != evalplugin.TriggerScheduled {
			continue
		}
		wanted[rec.Plugin.Metadata.Name] = rec
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	for name, rec := range wanted {
		ivl := rec.Plugin.Spec.Collect.Interval.Std()
		if ivl <= 0 {
			continue
		}
		if cur, ok := s.schedules[name]; ok {
			if cur.interval == ivl {
				continue
			}
			// interval changed; restart the flush goroutine
			cur.cancel()
		}
		ctx, cancel := context.WithCancel(context.Background())
		buf := s.bufferFor(name)
		sch := &scheduledFlush{plugin: rec.Plugin, interval: ivl, cancel: cancel}
		s.schedules[name] = sch
		s.wg.Add(1)
		go s.flushLoop(ctx, sch, buf)
		s.logf("scheduled eval plugin flush goroutine started", "plugin", name, "interval", ivl.String())
	}
	for name, sch := range s.schedules {
		if _, still := wanted[name]; still {
			continue
		}
		sch.cancel()
		delete(s.schedules, name)
		s.logf("scheduled eval plugin flush goroutine stopped", "plugin", name)
	}
}

// bufferFor returns the per-plugin buffer, creating it lazily when a
// trigger first gets registered. Caller must hold s.mu. Returns nil once
// Stop has cleared the maps — callers must treat that as "scheduler is
// draining" rather than dereferencing it.
func (s *Scheduler) bufferFor(name string) *pluginBuffer {
	if s.stopped {
		return nil
	}
	if buf, ok := s.buffers[name]; ok {
		return buf
	}
	buf := &pluginBuffer{ll: list.New(), cap: s.defaultCap}
	s.buffers[name] = buf
	return buf
}

// flushLoop drains a single plugin's buffer on the plugin's interval.
func (s *Scheduler) flushLoop(ctx context.Context, sch *scheduledFlush, buf *pluginBuffer) {
	defer s.wg.Done()
	ticker := time.NewTicker(sch.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.drain(ctx, sch.plugin, buf)
		}
	}
}

// drain sends every trace currently in the buffer through the
// dispatcher. Pop happens under s.mu, transmit happens after — a
// double-send on vendor timeout is acceptable because the trade was
// "at-least-once trace delivery": losing a trace is observable as a
// dropped score in the vendor dashboard, but sending a trace twice
// is mostly a duplicate identity on the vendor's side.
func (s *Scheduler) drain(ctx context.Context, p *evalplugin.Plugin, buf *pluginBuffer) {
	for {
		s.mu.Lock()
		el := buf.ll.Front()
		if el == nil {
			s.mu.Unlock()
			return
		}
		buf.ll.Remove(el)
		s.mu.Unlock()
		t, ok := el.Value.(observability.Trace)
		if !ok {
			continue
		}
		if err := s.dispatch(ctx, t, p); err != nil {
			s.warnf("scheduled dispatch failed",
				"plugin", p.Metadata.Name,
				"trace_id", t.TraceID,
				"err", err)
		}
	}
}

// Enqueue buffers t for a scheduled plugin. Returns ErrBufferFull
// without blocking when the per-plugin cap is reached. Callers must
// check the plugin's Send.Trigger before calling.
func (s *Scheduler) Enqueue(p *evalplugin.Plugin, t observability.Trace) error {
	if p == nil {
		return errors.New("nil plugin")
	}
	if p.Spec.Send.Trigger != evalplugin.TriggerScheduled {
		return errors.New("scheduler only buffers scheduled-trigger plugins (got " + p.Spec.Send.Trigger + ")")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := s.bufferFor(p.Metadata.Name)
	if buf == nil {
		return ErrSchedulerStopped
	}
	if buf.ll.Len() >= buf.cap {
		buf.dropped++
		return ErrBufferFull
	}
	buf.ll.PushBack(t)
	return nil
}

// FireManual drains a manual-trigger plugin's buffer (if any) and
// returns the count of traces that were dispatched. nil plugin or
// unknown name yields (0, nil) so the REST handler can render "no
// traces collected yet" without failing. The trigger string is
// recorded for audit; it does not alter what gets sent.
//
// FireManual is the manual-firer's admin-REST entry point. It exists
// even though manual plugins have an empty buffer by design: it lets
// admins push a one-shot sample through the vendor when there's been
// no inline traffic (e.g. a fresh install with no live traces).
func (s *Scheduler) FireManual(ctx context.Context, p *evalplugin.Plugin, trigger string) (int, error) {
	if p == nil {
		return 0, nil
	}
	if p.Spec.Send.Trigger != evalplugin.TriggerManual {
		return 0, errors.New("plugin " + p.Metadata.Name + " is not manual-trigger; trigger=" + p.Spec.Send.Trigger)
	}
	s.mu.Lock()
	buf := s.bufferFor(p.Metadata.Name)
	s.mu.Unlock()
	if buf == nil {
		return 0, ErrSchedulerStopped
	}
	count := s.drainAll(ctx, p, buf)
	s.logf("manual eval plugin fire", "plugin", p.Metadata.Name, "count", count, "trigger", trigger)
	return count, nil
}

// FireScheduled drains a scheduled-trigger plugin's buffer
// immediately, skipping the next interval tick. Useful when an
// operator wants to push buffered traces through the vendor right
// away without waiting for `spec.collect.interval` to elapse.
//
// The trigger string only changes the audit log line — it does not
// affect which traces get shipped or in what order. Callers that pass
// a non-scheduled plugin here get ErrNotScheduled so the REST handler
// can return a 4xx with a precise message instead of silently
// rejecting the input.
func (s *Scheduler) FireScheduled(ctx context.Context, p *evalplugin.Plugin, trigger string) (int, error) {
	if p == nil {
		return 0, nil
	}
	if p.Spec.Send.Trigger != evalplugin.TriggerScheduled {
		return 0, ErrNotScheduled
	}
	s.mu.Lock()
	buf := s.bufferFor(p.Metadata.Name)
	s.mu.Unlock()
	if buf == nil {
		return 0, ErrSchedulerStopped
	}
	count := s.drainAll(ctx, p, buf)
	s.logf("scheduled eval plugin fire", "plugin", p.Metadata.Name, "count", count, "trigger", trigger)
	return count, nil
}

// ErrNotScheduled is returned when FireScheduled is invoked with a
// plugin whose Send.Trigger is not `scheduled` (e.g. an `on_trace`
// plugin: the buffer is empty by design and the inline path already
// forwarded every trace).
var ErrNotScheduled = errors.New("plugin trigger is not scheduled")

// drainAll pops the entire buffer until empty, dispatching each
// trace through s.dispatch. Used by both FireManual and FireScheduled
// — the only difference between those two is the trigger-string guard
// and the audit log line.
func (s *Scheduler) drainAll(ctx context.Context, p *evalplugin.Plugin, buf *pluginBuffer) int {
	count := 0
	for {
		s.mu.Lock()
		el := buf.ll.Front()
		if el == nil {
			s.mu.Unlock()
			return count
		}
		buf.ll.Remove(el)
		s.mu.Unlock()
		t, ok := el.Value.(observability.Trace)
		if !ok {
			continue
		}
		if err := s.dispatch(ctx, t, p); err != nil {
			s.warnf("eval plugin dispatch failed",
				"plugin", p.Metadata.Name,
				"trace_id", t.TraceID,
				"err", err)
			continue
		}
		count++
	}
}

// DroppedReports returns how many traces were dropped per plugin since
// the scheduler started. Used by an opt-in admin endpoint so
// operators can see when their buffer cap is too small for the rate.
func (s *Scheduler) DroppedReports() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int64, len(s.buffers))
	for name, buf := range s.buffers {
		if buf.dropped > 0 {
			out[name] = buf.dropped
		}
	}
	return out
}

// Stop cancels scheduled flush goroutines and waits for them to
// exit. After Stop the scheduler cannot be restarted; callers that
// need restart semantics should construct a new instance.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	s.stopped = true
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	for _, sch := range s.schedules {
		sch.cancel()
	}
	s.schedules = nil
	s.buffers = nil
	s.mu.Unlock()
	s.wg.Wait()
}

// logf / warnf are tiny shims so this file doesn't need
// nil-checks at every call site.
func (s *Scheduler) logf(msg string, args ...any) {
	if s.log == nil {
		return
	}
	s.log.Info(msg, args...)
}
func (s *Scheduler) warnf(msg string, args ...any) {
	if s.log == nil {
		return
	}
	s.log.Warn(msg, args...)
}
