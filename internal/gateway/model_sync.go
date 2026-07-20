// Package gateway: background model sync worker. Periodically calls the
// configured ModelFetcher and writes the result into the dynamic provider
// (and its registry index). Runs entirely out-of-band of the hot path
// (registry lookup) so request latency stays O(1).
//
// Behaviour:
//
//   - One goroutine per provider (Map by provider name).
//   - Ticker drives the loop; the first tick fires immediately after
//     startRefresh so callers see the catalog updated without waiting
//     `interval` seconds.
//   - Per-attempt retry uses exponential backoff (1s → 2s → 4s, max 60s)
//     capped at maxRetries (default 3). A failed refresh keeps the
//     previously cached catalog so the gateway never loses the static
//     fallback that the builtin adapter registered.
//   - Cancellation is context-driven: when ctx is cancelled the worker
//     exits without side effects; in-flight http requests respect the
//     ctx and abort.
//
// The worker does not depend on prometheus/client_golang to honour the
// stdlib-only constraint of the gateway. Metrics are exposed via the
// counters at the bottom of this file and surfaced through slog so
// operators get them in the existing log pipeline; later versions can
// pipe them into the existing MetricsRecorder without changing the
// worker API.
package gateway

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// DynamicSyncCounters is a tiny in-memory counter set the worker updates on
// every refresh. The gateway MetricsRecorder (when enabled) reads the
// snapshot via Export() so /metrics on the gateway reflects sync health
// without depending on a third-party prometheus package.
type DynamicSyncCounters struct {
	success uint64 // total successful refreshes
	error   uint64 // total failed refresh attempts (one per retry)
	lastOK  unixNano
	lastErr unixNano
}

// newCountersForTest wires up a counters struct with a real start time so
// tests can assert relative durations without platform-specific shims.
type unixNano int64

func (c *DynamicSyncCounters) Export() (success, error uint64, lastOKUnix, lastErrUnix int64) {
	return atomic.LoadUint64(&c.success),
		atomic.LoadUint64(&c.error),
		atomic.LoadInt64((*int64)(&c.lastOK)),
		atomic.LoadInt64((*int64)(&c.lastErr))
}

// StartDynamicSync launches a background goroutine that refreshes the
// given provider's model list every interval. The goroutine returns when
// ctx is cancelled, leaving any in-flight http request with a deadline
// abort.
//
// maxRetries caps the per-tick retry budget so a flapping upstream cannot
// wedge the loop; an interval of zero falls back to 30 minutes which is
// a sane default for the slowest plausible provider cadence.
func StartDynamicSync(ctx context.Context, reg *Registry, dp *DynamicProvider, fetcher ModelFetcher, interval time.Duration, maxRetries int, counters *DynamicSyncCounters, logger *slog.Logger) {
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	if maxRetries <= 0 {
		maxRetries = 3
	}
	if counters == nil {
		counters = &DynamicSyncCounters{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	go func() {
		// Run once immediately so the very first /v1/models response after
		// startup already reflects the upstream catalog rather than the
		// empty static fallback.
		runOnce(ctx, reg, dp, fetcher, maxRetries, counters, logger)

		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				runOnce(ctx, reg, dp, fetcher, maxRetries, counters, logger)
			}
		}
	}()
}

// runOnce performs one refresh with bounded retries. On failure the
// previously cached catalog is preserved; we never blank the registry so a
// flaky upstream cannot take a model lifecycle-of-a-tick offline.
func runOnce(ctx context.Context, reg *Registry, dp *DynamicProvider, fetcher ModelFetcher, maxRetries int, counters *DynamicSyncCounters, logger *slog.Logger) {
	var (
		models []string
		err    error
	)
	for attempt := 0; attempt <= maxRetries; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		models, err = fetcher(attemptCtx)
		cancel()
		if err == nil {
			break
		}
		atomic.AddUint64(&counters.error, 1)
		atomic.StoreInt64((*int64)(&counters.lastErr), time.Now().UnixNano())
		if errors.Is(err, context.Canceled) {
			return
		}
		if attempt == maxRetries {
			logger.Warn("dynamic model sync failed; keeping previous catalog",
				"provider", dp.Name(),
				"attempts", attempt+1,
				"err", err)
			return
		}
		// Exponential backoff with a small jitter so multiple replicas
		// hitting the same provider do not synchronize their retries.
		backoff := time.Duration(1<<attempt) * time.Second
		if backoff > 60*time.Second {
			backoff = 60 * time.Second
		}
		jitter := time.Duration(rand.Int63n(int64(backoff) / 4))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff + jitter):
		}
	}
	prev := dp.Set(models)
	reg.UpdateModels(dp.Name(), models)
	atomic.AddUint64(&counters.success, 1)
	atomic.StoreInt64((*int64)(&counters.lastOK), time.Now().UnixNano())
	logger.Info("dynamic model sync refreshed catalog",
		"provider", dp.Name(),
		"models", len(models),
		"added", countNew(prev, models),
		"removed", countNew(models, prev))
}

// countNew returns |a \ b| without allocating a map: O(n*m) is fine for
// the typical catalog size of a few hundred entries and keeps the hot
// path on the worker free of hash-table cost.
func countNew(a, b []string) int {
	if len(a) == 0 {
		return 0
	}
	var n int
	for _, x := range a {
		found := false
		for _, y := range b {
			if x == y {
				found = true
				break
			}
		}
		if !found {
			n++
		}
	}
	return n
}

// DynamicSyncRegistry collects counters for every provider in one place so
// the gateway can emit a single set for /metrics and the console API
// without each provider needing its own observer. Access is goroutine-safe
// via the embedded mutex.
type DynamicSyncRegistry struct {
	mu        sync.RWMutex
	counters  map[string]*DynamicSyncCounters
	providers map[string]*DynamicProvider
}

// NewDynamicSyncRegistry creates an empty collector map keyed by provider
// name. Pass it to StartDynamicSyncWithRegistry from main.go so all
// providers' counters end up in the same object.
func NewDynamicSyncRegistry() *DynamicSyncRegistry {
	return &DynamicSyncRegistry{
		counters:  map[string]*DynamicSyncCounters{},
		providers: map[string]*DynamicProvider{},
	}
}

// Register adds (or overwrites) the counters for a provider. Intended to
// be called once per provider during boot, right before StartDynamicSync.
func (r *DynamicSyncRegistry) Register(name string, dp *DynamicProvider, c *DynamicSyncCounters) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters[name] = c
	r.providers[name] = dp
}

// Snapshot returns a stable copy of all counters for one /metrics scrape.
func (r *DynamicSyncRegistry) Snapshot() map[string]DynamicSyncCounters {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]DynamicSyncCounters, len(r.counters))
	for k, c := range r.counters {
		out[k] = DynamicSyncCounters{
			success: atomic.LoadUint64(&c.success),
			error:   atomic.LoadUint64(&c.error),
			lastOK:  c.lastOK,
			lastErr: c.lastErr,
		}
	}
	return out
}
