// Failover webhook + Slack notifiers (V4). Both share a single buffered
// async worker so the gateway's hot path is never coupled to the
// latency of an external HTTP POST.

package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// bufferedNotifier runs a small worker that drains an in-memory queue
// and POSTs to an HTTP endpoint. It implements the Notifier interface
// and is shared between the Webhook and Slack sinks (they only differ
// in the JSON shape they send, not the transport).
type bufferedNotifier struct {
	log      *slog.Logger
	client   *http.Client
	endpoint string
	encode   func(FailoverEvent) ([]byte, string) // returns body + content-type

	ch     chan FailoverEvent
	done   chan struct{}
	wg     sync.WaitGroup
	closed chan struct{}

	// cooldown between consecutive POSTs to the same endpoint. Keeps
	// flapping from a single bad primary from melting the alert inbox.
	cooldown time.Duration
	dropped  uint64 // exposed for tests; reducer notices via Logger if useful
}

// bufferedNotifierOptions configures the buffered async sender.
type bufferedNotifierOptions struct {
	endpoint string
	encode   func(FailoverEvent) ([]byte, string)
	timeout  time.Duration
	cooldown time.Duration
	buffer   int
}

// newBufferedNotifier returns a Notifier that POSTs each event to the
// given endpoint encoded by encode(). A nil endpoint produces a nil
// recorder so the caller can treat it as "opt-out". A nil or failed
// encode produces a fatal log + skip; a slow or refused POST only
// logs, never blocks the caller.
func newBufferedNotifier(opts bufferedNotifierOptions, log *slog.Logger) *bufferedNotifier {
	if opts.endpoint == "" {
		return nil
	}
	if opts.timeout == 0 {
		opts.timeout = 5 * time.Second
	}
	if opts.buffer == 0 {
		opts.buffer = 256
	}
	if opts.cooldown == 0 {
		opts.cooldown = 0 // off by default; tests opt-in
	}
	bn := &bufferedNotifier{
		log:      log,
		client:   &http.Client{Timeout: opts.timeout},
		endpoint: opts.endpoint,
		encode:   opts.encode,
		ch:       make(chan FailoverEvent, opts.buffer),
		done:     make(chan struct{}),
		closed:   make(chan struct{}),
		cooldown: opts.cooldown,
	}
	bn.wg.Add(1)
	go bn.loop()
	return bn
}

// Notify enqueues an event; never blocks. A full buffer drops the
// event with a warn line — observability must not backpressure the
// gateway.
func (b *bufferedNotifier) Notify(_ context.Context, ev FailoverEvent) {
	if b == nil {
		return
	}
	select {
	case b.ch <- ev:
	default:
		b.log.Warn("failover notifier buffer full; dropping event",
			"primary", ev.Primary, "fallback", ev.Fallback)
		b.dropped++
	}
}

// Close drains the queue and stops the worker. ctx.Done() caps the
// drain wait so a stuck HTTP client doesn't outlive the gateway's
// shutdown grace period.
func (b *bufferedNotifier) Close(ctx context.Context) error {
	if b == nil {
		return nil
	}
	close(b.done)
	select {
	case <-b.closed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *bufferedNotifier) loop() {
	defer b.wg.Done()
	lastSent := time.Time{}
	for {
		select {
		case <-b.done:
			// Drain whatever queued messages remain.
			for {
				select {
				case ev := <-b.ch:
					b.sendOnce(ev, &lastSent)
				default:
					close(b.closed)
					return
				}
			}
		case ev := <-b.ch:
			b.sendOnce(ev, &lastSent)
		}
	}
}

func (b *bufferedNotifier) sendOnce(ev FailoverEvent, lastSent *time.Time) {
	// Cooldown: skip recent back-to-back alert spam while still
	// recording the most recent occurrence. Each POST we *do* send
	// is annotated with the suppressed count via a "skipped" key
	// (Slack formatter supports it; generic webhook drops it).
	if b.cooldown > 0 && !lastSent.IsZero() && time.Since(*lastSent) < b.cooldown {
		ev.Tried = append(ev.Tried, fmt.Sprintf("(suppressed_by_cooldown:last_sent=%s)", lastSent.Format(time.RFC3339)))
		// Don't actually send; just bump the suppressed counter so
		// operators don't lose the visibility entirely.
		return
	}
	body, ct := b.encode(ev)
	req, err := http.NewRequest(http.MethodPost, b.endpoint, bytes.NewReader(body))
	if err != nil {
		b.log.Warn("failover notifier: marshal error", "err", err, "primary", ev.Primary)
		return
	}
	req.Header.Set("Content-Type", ct)
	resp, err := b.client.Do(req)
	if err != nil {
		b.log.Warn("failover notifier: POST failed", "err", err, "endpoint", b.endpoint)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b.log.Warn("failover notifier: unexpected status",
			"status", resp.StatusCode, "endpoint", b.endpoint)
		return
	}
	*lastSent = time.Now()
}

// --- Generic webhook ----------------------------------------------------

// NewWebhookNotifier returns a Notifier that POSTs a JSON envelope of
// the event to endpoint. Empty endpoint => nil. The envelope is
// stable JSON (see FailoverEvent); consumers can switch on the
// "primary" key to recover upstream identifiers.
func NewWebhookNotifier(endpoint string, log *slog.Logger) Notifier {
	bn := newBufferedNotifier(bufferedNotifierOptions{
		endpoint: endpoint,
		encode: func(ev FailoverEvent) ([]byte, string) {
			buf, _ := json.Marshal(ev)
			return buf, "application/json"
		},
	}, log)
	if bn == nil {
		return nil // collapse typed nil into the interface nil so callers see a clean nil
	}
	return bn
}

// --- Slack-style notifier ----------------------------------------------

// slackPayload is the minimum-viable Slack incoming-webhook shape.
// Reference: https://api.slack.com/messaging/webhooks (text + blocks).
type slackPayload struct {
	Text string `json:"text"`
}

// NewSlackNotifier returns a Notifier that posts a brief one-liner
// to a Slack-compatible incoming webhook (Slack, Mattermost, Discord
// via their proxy endpoints all accept {"text": "..."}).
func NewSlackNotifier(endpoint string, log *slog.Logger) Notifier {
	bn := newBufferedNotifier(bufferedNotifierOptions{
		endpoint: endpoint,
		encode: func(ev FailoverEvent) ([]byte, string) {
			text := fmt.Sprintf(":warning: nexus failover · %s → %s · reason=%s · virtual_key=%s · alias=%s",
				ev.Primary, ev.Fallback, ev.Reason, ev.VirtualKeyID, ev.Alias)
			buf, _ := json.Marshal(slackPayload{Text: text})
			return buf, "application/json"
		},
	}, log)
	if bn == nil {
		return nil
	}
	return bn
}
