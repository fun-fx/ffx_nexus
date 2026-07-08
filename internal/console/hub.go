// Package console serves the dashboard API and live trace feed.
package console

import (
	"context"
	"sync"

	"github.com/ffxnexus/nexus/internal/observability"
)

// Hub broadcasts traces to connected WebSocket dashboard clients. It implements
// observability.Recorder so it can be composed into the gateway's MultiRecorder.
type Hub struct {
	mu      sync.RWMutex
	clients map[chan observability.Trace]struct{}
	users   map[chan observability.Trace]string // userID per channel ("" = admin sees all)
}

// NewHub creates an empty hub.
func NewHub() *Hub {
	return &Hub{
		clients: make(map[chan observability.Trace]struct{}),
		users:   make(map[chan observability.Trace]string),
	}
}

// Record implements observability.Recorder by broadcasting to live clients.
// Sends are non-blocking; a slow client simply misses the live update.
func (h *Hub) Record(t observability.Trace) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.clients {
		if uid, ok := h.users[ch]; ok && uid != "" && uid != t.UserID {
			continue
		}
		select {
		case ch <- t:
		default:
		}
	}
}

// Close implements observability.Recorder.
func (h *Hub) Close(context.Context) error { return nil }

// subscribe registers a new client channel scoped to the given userID. Pass
// "" for unscoped (admin) subscribers. The userID is enforced in Record.
func (h *Hub) subscribe(userID string) chan observability.Trace {
	ch := make(chan observability.Trace, 64)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.users[ch] = userID
	h.mu.Unlock()
	return ch
}

// unsubscribe removes a client channel.
func (h *Hub) unsubscribe(ch chan observability.Trace) {
	h.mu.Lock()
	delete(h.clients, ch)
	delete(h.users, ch)
	h.mu.Unlock()
	close(ch)
}
