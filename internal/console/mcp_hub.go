package console

import (
	"context"
	"sync"

	"github.com/ffxnexus/nexus/internal/observability"
)

// MCPHub broadcasts MCP logs to connected WebSocket clients.
type MCPHub struct {
	mu      sync.RWMutex
	clients map[chan observability.MCPLog]struct{}
	users   map[chan observability.MCPLog]string
}

// NewMCPHub creates an empty MCP log hub.
func NewMCPHub() *MCPHub {
	return &MCPHub{
		clients: make(map[chan observability.MCPLog]struct{}),
		users:   make(map[chan observability.MCPLog]string),
	}
}

// Record implements observability.MCPLogRecorder.
func (h *MCPHub) Record(l observability.MCPLog) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.clients {
		if uid, ok := h.users[ch]; ok && uid != "" && uid != l.UserID {
			continue
		}
		select {
		case ch <- l:
		default:
		}
	}
}

// Close implements observability.MCPLogRecorder.
func (h *MCPHub) Close(context.Context) error { return nil }

func (h *MCPHub) subscribe(userID string) chan observability.MCPLog {
	ch := make(chan observability.MCPLog, 64)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.users[ch] = userID
	h.mu.Unlock()
	return ch
}

func (h *MCPHub) unsubscribe(ch chan observability.MCPLog) {
	h.mu.Lock()
	delete(h.clients, ch)
	delete(h.users, ch)
	h.mu.Unlock()
	close(ch)
}
