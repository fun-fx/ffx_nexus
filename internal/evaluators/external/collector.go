package external

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evals"
)

// CollectFunc is the per-vendor result-fetcher implemented by
// adapters. It is called once per polling tick; the returned
// JSON.RawMessage is fed through Apply to produce an evals.Score.
type CollectFunc func(ctx context.Context, endpoint string) ([]json.RawMessage, error)

// Collector drives the "collect" side of each plugin. It runs a
// background goroutine per polling-mode plugin and exposes the
// Webhook receiver endpoint for webhook-mode plugins.
//
// Webhook mode is intentionally bare: the receiver is a single
// function that decrypts the signed payload, parses it via the
// plugin's ResultMapping, and drops the resulting Score into the
// supplied Sink. There is no per-plugin endpoint granularity —
// admins configure one shared signing key per installation.
type Collector struct {
	mu       sync.Mutex
	reg      *evalplugin.Registry
	collects map[evalplugin.ServiceType]CollectFunc
	sink     evals.Sink
	client   *http.Client
}

// NewCollector builds a Collector. The sink is the same instance
// the worker uses (ClickHouse or Postgres) so plugin-generated
// scores share storage with heuristic- and judge-generated scores.
func NewCollector(reg *evalplugin.Registry, sink evals.Sink, httpClient *http.Client) *Collector {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Collector{
		reg:      reg,
		sink:     sink,
		client:   httpClient,
		collects: make(map[evalplugin.ServiceType]CollectFunc),
	}
}

// Register attaches a CollectFunc for a vendor. Mirrors
// Dispatcher.Register's ergonomics.
func (c *Collector) Register(t evalplugin.ServiceType, fn CollectFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.collects[t] = fn
}

// Run starts the per-plugin pollers and blocks until ctx is done.
// Each polling-mode plugin spawns its own goroutine. Webhook
// receivers don't need a goroutine — they listen on the configured
// HTTP routes from the console mux.
func (c *Collector) Run(ctx context.Context) error {
	enabled := c.reg.Enabled()
	for _, rec := range enabled {
		if rec.Plugin.Spec.Collect.Mode != "poll" {
			continue
		}
		interval := rec.Plugin.Spec.Collect.Interval.Std()
		if interval <= 0 {
			interval = 60 * time.Second
		}
		go c.runPoll(ctx, rec.Plugin, interval)
	}
	<-ctx.Done()
	return nil
}

func (c *Collector) runPoll(ctx context.Context, p *evalplugin.Plugin, interval time.Duration) {
	c.mu.Lock()
	fn := c.collects[p.Spec.Service.Type]
	c.mu.Unlock()
	if fn == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			payloads, err := fn(ctx, p.Spec.Service.Endpoint)
			if err != nil {
				continue
			}
			c.applyAll(ctx, p, payloads)
		}
	}
}

// applyAll converts each fetched JSON record into a Score and
// writes them via the supplied Sink. Adapter implementations
// should keep payloads small; we cap at 1000 per tick as a
// safety net against pathological backfills.
func (c *Collector) applyAll(ctx context.Context, p *evalplugin.Plugin, payloads []json.RawMessage) {
	if c.sink == nil || len(payloads) == 0 {
		return
	}
	if len(payloads) > 1000 {
		payloads = payloads[:1000]
	}
	scores := make([]evals.Score, 0, len(payloads))
	for _, raw := range payloads {
		sc, err := Apply(raw, p.Spec.Collect.Mapping, p.Metadata.Name)
		if err != nil {
			continue
		}
		scores = append(scores, sc)
	}
	if len(scores) == 0 {
		return
	}
	_ = c.sink.WriteScores(ctx, scores)
}

// Webhook processes a single inbound HTTP request from an external
// service. Adapters don't take part in this path — the unmarshal +
// ResultMapping plumbing is identical regardless of vendor. The
// caller (console mux) is responsible for verifying the request
// signature before invoking.
func (c *Collector) Webhook(pluginName string, body io.Reader) error {
	if c.sink == nil {
		return errors.New("sink is not configured")
	}
	rec, ok := c.reg.Lookup(pluginName)
	if !ok {
		return fmt.Errorf("plugin %q not loaded", pluginName)
	}
	if rec.Plugin.Spec.Collect.Mode != "webhook" {
		return fmt.Errorf("plugin %q is not in webhook collect mode", pluginName)
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	// Accept either a single object or an array; multi-record
	// deliveries (batched webhooks) split before applying.
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		arr = []json.RawMessage{raw}
	}
	c.applyAll(context.Background(), rec.Plugin, arr)
	return nil
}

// helperString is a defensive wrapper around strings.HasPrefix used
// by adapters when checking vendor-specific response content-types.
func helperString(s, prefix string) bool { return strings.HasPrefix(s, prefix) }

// httpStatusError wraps a non-2xx HTTP response so the caller can
// inspect status without re-querying resp.
type httpStatusError struct{ Status int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("unexpected HTTP status %d", e.Status) }

// ensureResponse is the shared "did we succeed?" check used by
// adapters to keep status-class handling consistent.
func ensureResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &httpStatusError{Status: resp.StatusCode}
}
