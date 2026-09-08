package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// LogSink receives MCP tool execution records asynchronously.
type LogSink interface {
	Record(log ToolLog)
}

// ToolLog is the in-memory record emitted after each tool call.
type ToolLog struct {
	ID           string
	Timestamp    time.Time
	OrgID        string
	UserID       string
	VirtualKeyID string
	ServerID     string
	ServerLabel  string
	ToolName     string
	Status       string // success | error
	LatencyMs    int64
	CostUSD      float64
	LLMTraceID   string
	SessionID    string
	TurnID       string
	RequestID    string
	Arguments    string
	Result       string
	ErrorMessage string
	Metadata     string
}

// Store is the persistence surface the manager reads server rows from.
type Store interface {
	List(ctx context.Context, orgID string) ([]ServerRecord, error)
	Get(ctx context.Context, id string) (*ServerRecord, error)
}

// Manager maintains MCP client connections and executes tool calls.
type Manager struct {
	store Store
	log   *slog.Logger
	sink  LogSink

	mu      sync.RWMutex
	clients map[string]*clientState // server id -> state
}

type clientState struct {
	record     ServerRecord
	spec       *ServerSpec
	cli        *client.Client
	tools      []Tool
	state      string
	lastError  string
	lastSync   time.Time
}

// NewManager creates an MCP manager. Call Reload to connect configured servers.
func NewManager(store Store, sink LogSink, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		store:   store,
		sink:    sink,
		log:     log,
		clients: make(map[string]*clientState),
	}
}

// SetLogSink replaces the async log sink (used during boot wiring).
func (m *Manager) SetLogSink(sink LogSink) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sink = sink
}

// Reload loads all enabled servers from the store and reconnects.
func (m *Manager) Reload(ctx context.Context) error {
	if m.store == nil {
		return nil
	}
	rows, err := m.store.List(ctx, "")
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(rows))
	for _, rec := range rows {
		seen[rec.ID] = struct{}{}
		if !rec.Enabled {
			m.setDisabled(rec)
			continue
		}
		if err := m.connectServer(ctx, rec); err != nil {
			m.log.Warn("mcp server connect failed", "server", rec.Name, "err", err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, st := range m.clients {
		if _, ok := seen[id]; !ok {
			if st.cli != nil {
				_ = st.cli.Close()
			}
			delete(m.clients, id)
		}
	}
	return nil
}

func (m *Manager) setDisabled(rec ServerRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.clients[rec.ID]; ok && st.cli != nil {
		_ = st.cli.Close()
	}
	spec, _ := DecodeSpec([]byte(rec.SpecYAML))
	m.clients[rec.ID] = &clientState{
		record: rec,
		spec:   spec,
		state:  "disabled",
	}
}

// Upsert reconnects one server after a CRUD change.
func (m *Manager) Upsert(ctx context.Context, rec ServerRecord) error {
	if !rec.Enabled {
		m.setDisabled(rec)
		return nil
	}
	return m.connectServer(ctx, rec)
}

// Remove disconnects and drops a server from the pool.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.clients[id]; ok {
		if st.cli != nil {
			_ = st.cli.Close()
		}
		delete(m.clients, id)
	}
}

// Reconnect forces a reconnect for one server.
func (m *Manager) Reconnect(ctx context.Context, id string) error {
	if m.store == nil {
		return errors.New("mcp: store not configured")
	}
	rec, err := m.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if !rec.Enabled {
		m.setDisabled(*rec)
		return nil
	}
	return m.connectServer(ctx, *rec)
}

func (m *Manager) connectServer(ctx context.Context, rec ServerRecord) error {
	spec, err := DecodeSpec([]byte(rec.SpecYAML))
	if err != nil {
		m.markError(rec, spec, err)
		return err
	}

	m.mu.Lock()
	if old, ok := m.clients[rec.ID]; ok && old.cli != nil {
		_ = old.cli.Close()
	}
	m.clients[rec.ID] = &clientState{record: rec, spec: spec, state: "connecting"}
	m.mu.Unlock()

	cli, err := dialClient(spec)
	if err != nil {
		m.markError(rec, spec, err)
		return err
	}
	if err := cli.Start(ctx); err != nil {
		_ = cli.Close()
		m.markError(rec, spec, err)
		return err
	}
	initCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.Timeout())*time.Millisecond)
	defer cancel()
	_, err = cli.Initialize(initCtx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "nexus-gateway",
				Version: "0.7.1",
			},
		},
	})
	if err != nil {
		_ = cli.Close()
		m.markError(rec, spec, err)
		return err
	}

	listCtx, listCancel := context.WithTimeout(ctx, time.Duration(spec.Timeout())*time.Millisecond)
	defer listCancel()
	toolsRes, err := cli.ListTools(listCtx, mcp.ListToolsRequest{})
	if err != nil {
		_ = cli.Close()
		m.markError(rec, spec, err)
		return err
	}
	tools := make([]Tool, 0, len(toolsRes.Tools))
	for _, t := range toolsRes.Tools {
		tools = append(tools, Tool{Name: t.Name, Description: t.Description})
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	m.mu.Lock()
	m.clients[rec.ID] = &clientState{
		record:    rec,
		spec:      spec,
		cli:       cli,
		tools:     tools,
		state:     "healthy",
		lastSync:  time.Now(),
	}
	m.mu.Unlock()
	return nil
}

func dialClient(spec *ServerSpec) (*client.Client, error) {
	switch spec.ConnectionType() {
	case "stdio":
		env := os.Environ()
		for k, v := range spec.Connection.Env {
			env = append(env, k+"="+v)
		}
		return client.NewStdioMCPClient(spec.Connection.Command, env, spec.Connection.Args...)
	case "http":
		opts := []transport.StreamableHTTPCOption{}
		if len(spec.Connection.Headers) > 0 {
			opts = append(opts, transport.WithHTTPHeaders(spec.Connection.Headers))
		}
		return client.NewStreamableHttpClient(spec.Connection.URL, opts...)
	default:
		return nil, fmt.Errorf("unsupported connection type %q", spec.Connection.Type)
	}
}

func (m *Manager) markError(rec ServerRecord, spec *ServerSpec, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clients[rec.ID] = &clientState{
		record:    rec,
		spec:      spec,
		state:     "error",
		lastError: err.Error(),
	}
}

// ListServers returns runtime status for org-scoped servers.
func (m *Manager) ListServers(orgID, virtualKeyID string) []ServerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ServerStatus, 0, len(m.clients))
	for _, st := range m.clients {
		if orgID != "" && st.record.OrgID != "" && st.record.OrgID != orgID {
			continue
		}
		if st.spec != nil && virtualKeyID != "" && !st.spec.AllowsVirtualKey(virtualKeyID) {
			continue
		}
		if !st.record.Enabled {
			continue
		}
		out = append(out, m.statusFrom(st))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ListAllServers returns every server status for the console (admin view).
func (m *Manager) ListAllServers(orgID string) []ServerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ServerStatus, 0, len(m.clients))
	for _, st := range m.clients {
		if orgID != "" && st.record.OrgID != "" && st.record.OrgID != orgID {
			continue
		}
		out = append(out, m.statusFrom(st))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m *Manager) statusFrom(st *clientState) ServerStatus {
	connType := ""
	if st.spec != nil {
		connType = st.spec.ConnectionType()
	}
	return ServerStatus{
		ServerRecord:   st.record,
		ConnectionType: connType,
		State:          st.state,
		ToolCount:      len(st.tools),
		Tools:          append([]Tool(nil), st.tools...),
		LastError:      st.lastError,
	}
}

// ListTools proxies tools/list for one server.
func (m *Manager) ListTools(ctx context.Context, orgID, serverID, virtualKeyID string) ([]Tool, error) {
	st, err := m.getAllowed(orgID, serverID, virtualKeyID)
	if err != nil {
		return nil, err
	}
	if st.state != "healthy" || st.cli == nil {
		return nil, fmt.Errorf("mcp server %q is %s", st.record.Name, st.state)
	}
	return append([]Tool(nil), st.tools...), nil
}

// CallTool executes tools/call and records a log entry.
func (m *Manager) CallTool(ctx context.Context, orgID, serverID string, cc CallContext, req CallRequest) (*CallResult, error) {
	st, err := m.getAllowed(orgID, serverID, cc.VirtualKeyID)
	if err != nil {
		return nil, err
	}
	if st.state != "healthy" || st.cli == nil {
		return nil, fmt.Errorf("mcp server %q is %s", st.record.Name, st.state)
	}
	if strings.TrimSpace(req.Name) == "" {
		return nil, errors.New("tool name is required")
	}

	argsJSON, _ := json.Marshal(req.Arguments)
	start := time.Now()
	timeout := time.Duration(st.spec.Timeout()) * time.Millisecond
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	toolReq := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      req.Name,
			Arguments: req.Arguments,
		},
	}
	toolRes, callErr := st.cli.CallTool(callCtx, toolReq)
	latency := time.Since(start).Milliseconds()

	result := &CallResult{LatencyMs: latency}
	var resultJSON string
	status := "success"
	errMsg := ""
	if callErr != nil {
		status = "error"
		errMsg = callErr.Error()
	} else {
		raw, _ := json.Marshal(toolRes)
		resultJSON = string(raw)
		result.RawJSON = resultJSON
		result.IsError = toolRes.IsError
		if toolRes.IsError {
			status = "error"
		}
		result.Content = toolRes.Content
	}

	m.emitLog(ToolLog{
		ID:           newLogID(),
		Timestamp:    time.Now().UTC(),
		OrgID:        cc.OrgID,
		UserID:       cc.UserID,
		VirtualKeyID: cc.VirtualKeyID,
		ServerID:     st.record.ID,
		ServerLabel:  st.record.Name,
		ToolName:     req.Name,
		Status:       status,
		LatencyMs:    latency,
		LLMTraceID:   firstNonEmpty(req.LLMTraceID, cc.LLMTraceID),
		SessionID:    firstNonEmpty(req.SessionID, cc.SessionID),
		TurnID:       firstNonEmpty(req.TurnID, cc.TurnID),
		RequestID:    cc.RequestID,
		Arguments:    string(argsJSON),
		Result:       resultJSON,
		ErrorMessage: errMsg,
	})

	if callErr != nil {
		return result, callErr
	}
	return result, nil
}

func (m *Manager) getAllowed(orgID, serverID, virtualKeyID string) (*clientState, error) {
	m.mu.RLock()
	st, ok := m.clients[serverID]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrServerNotFound
	}
	if orgID != "" && st.record.OrgID != "" && st.record.OrgID != orgID {
		return nil, ErrServerNotFound
	}
	if st.spec != nil && virtualKeyID != "" && !st.spec.AllowsVirtualKey(virtualKeyID) {
		return nil, errors.New("mcp: virtual key not allowed for this server")
	}
	return st, nil
}

func (m *Manager) emitLog(entry ToolLog) {
	if m.sink == nil {
		return
	}
	m.sink.Record(entry)
}

func newLogID() string {
	return fmt.Sprintf("mcplog_%d", time.Now().UnixNano())
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// TestConnection runs tools/list for one server and returns tool count.
func (m *Manager) TestConnection(ctx context.Context, id string) (int, error) {
	if err := m.Reconnect(ctx, id); err != nil {
		return 0, err
	}
	m.mu.RLock()
	st, ok := m.clients[id]
	m.mu.RUnlock()
	if !ok {
		return 0, ErrServerNotFound
	}
	return len(st.tools), nil
}
