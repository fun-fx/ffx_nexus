package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- newTrace session_id population ----

// Each test below exercises extractSessionID + the wire→context hop
// newTrace reads, rather than directly poking trace fields. We bind
// the marker onto the request context the same way ChatCompletions
// does so the integration sticks even if newTrace internals move.

// sessionReqWith binds a session_id onto a request context the same
// way ChatCompletions/Responses do, so newTrace reads it consistently
// in the unit tests.
func sessionReqWith(sessionID string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if sessionID != "" {
		r = r.WithContext(context.WithValue(r.Context(), ctxKeySessionID, sessionID))
	}
	return r
}

// sessionBuild wraps newTrace so we can assert on the SessionID field
// without exercising the rest of the chat path. newTrace only reads
// fields off req + the request context, so a Handler stub is fine.
func sessionBuild(body string) string {
	h := &Handler{}
	id := extractSessionID([]byte(body))
	r := sessionReqWith(id)
	var req ChatCompletionRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		return "<unmarshal-failed>"
	}
	tr := h.newTrace(r, req, "grid")
	return tr.SessionID
}

// When the wire payload contains metadata.session_id, it lands on the
// trace so the overview can fold N consecutive sub-step calls from
// a single agent loop into one session row.
func TestNewTrace_PullsSessionIDFromMetadata(t *testing.T) {
	body := `{
		"model": "code-prime",
		"messages": [{"role":"user","content":"hi"}],
		"metadata": {"session_id": "agent-conv-42"}
	}`
	if got := sessionBuild(body); got != "agent-conv-42" {
		t.Fatalf("session_id = %q; want agent-conv-42", got)
	}
}

// sessionId and conversation_id are also accepted so OpenAI Responses
// and Anthropic-shaped clients that use other naming conventions still
// fold into one row.
func TestNewTrace_SessionIDAliases(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"model":"x","messages":[],"metadata":{"sessionId":"sid-1"}}`, "sid-1"},
		{`{"model":"x","messages":[],"metadata":{"conversation_id":"cid-2"}}`, "cid-2"},
	}
	for _, tc := range cases {
		if got := sessionBuild(tc.body); got != tc.want {
			t.Errorf("session_id for %s = %q; want %q", tc.body, got, tc.want)
		}
	}
}

// When the wire has no session identifier the trace falls back to the
// OpenAI `user` field with a stable prefix so two callers sharing
// user:<id> collapse together without colliding with random sessions.
func TestNewTrace_FallsBackToUserField(t *testing.T) {
	body := `{"model":"x","messages":[],"user":"u-123"}`
	if got := sessionBuild(body); got != "user:u-123" {
		t.Fatalf("session_id = %q; want user:u-123", got)
	}
}

// metadata.session_id wins over user: when both are present so an
// agent loop's explicit id beats the per-end-user tag.
func TestNewTrace_MetadataBeatsUserFallback(t *testing.T) {
	body := `{
		"model":"x","messages":[],
		"user":"u-1",
		"metadata": {"session_id":"agent-7"}
	}`
	if got := sessionBuild(body); got != "agent-7" {
		t.Fatalf("session_id = %q; want agent-7", got)
	}
}

// Empty `user` AND empty metadata leaves SessionID blank; the
// frontend fills that gap with a time-window heuristic.
func TestNewTrace_NoMarkerLeavesBlank(t *testing.T) {
	body := `{"model":"x","messages":[]}`
	if got := sessionBuild(body); got != "" {
		t.Fatalf("session_id = %q; want blank", got)
	}
}

// ---- end-to-end through the handler ----

// A chat-completion request that carries metadata.session_id lands on
// the wire cost header AND the trace; the cost code we just bolted on
// did not silently clobber the roll-up field. We assert on the wire
// because httptest only exposes the response, not the recorder.
func TestChatCompletions_SessionIDCoexistsWithCost(t *testing.T) {
	p := &upstreamCostProvider{modelName: "code-prime", cost: 0.005}
	h := newTestHandler(p)
	body := `{"model":"code-prime","messages":[{"role":"user","content":"hi"}],"metadata":{"session_id":"coin"}}`
	rec := doChat(h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("x-nexus-cost-usd"); got != "0.005000" {
		t.Fatalf("x-nexus-cost-usd = %q; want 0.005000", got)
	}
}

// The streaming path stays green when the request carries a session
// marker — session_id is trace-only, so we just want the cost
// trailer to land on a real net connection (httptest's recorder
// silently keeps headers after WriteHeader, but the streaming code
// has to be wary of that).
func TestChatCompletions_Stream_SessionIDCarriesThrough(t *testing.T) {
	if testing.Short() {
		t.Skip("net loopback required")
	}
	p := &upstreamCostProvider{modelName: "code-prime", cost: 0.0189, stream: true}
	h := newTestHandler(p)

	srv := httptest.NewServer(http.HandlerFunc(h.ChatCompletions))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(
		`{"model":"code-prime","messages":[{"role":"user","content":"hi"}],"stream":true,"metadata":{"session_id":"streammid"}}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if got := resp.Trailer.Get("x-nexus-cost-usd"); got != "0.018900" {
		t.Errorf("trailer = %q; want 0.018900", got)
	}
}
