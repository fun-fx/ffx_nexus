package observability

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestProdOTLPRecorderLiveBody runs the production OTLPRecorder against
// a local TCP listener so we can capture the raw HTTP body the binary
// sends. Operators use this to compare against what an OTLP receiver
// expects; saves debugging time when a collector returns 400.
func TestProdOTLPRecorderLiveBody(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-body probe in short mode")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	capturedBody := make(chan []byte, 4)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case capturedBody <- body:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"partialSuccess":{}}`))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)
	defer srv.Shutdown(nil)

	addr := "http://" + listener.Addr().String() + "/v1/traces"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := NewOTLPRecorder(OTLPOptions{
		Endpoint:   addr,
		BatchSize:  1,
		FlushEvery: 100 * time.Millisecond,
		BufferSize: 8,
		Timeout:    5 * time.Second,
	}, log)
	if rec == nil {
		t.Fatal("recorder nil despite non-empty endpoint")
	}
	defer rec.Close(context.Background())

	rec.Record(Trace{
		TraceID:       "abc123def456abc123def456abc123de",
		OperationName: "chat",
		ProviderName:  "openai",
		RequestModel:  "gpt-4o-mini",
		StatusCode:    502,
		ErrorType:     "no_api_key",
		Timestamp:     time.Now().UTC(),
	})

	select {
	case body := <-capturedBody:
		t.Logf("=== captured body ===\n%s\n=== end ===\n", string(body))
		if len(body) == 0 {
			t.Fatal("empty body captured")
		}
		if body[0] != '{' {
			t.Errorf("body does not start with '{': first byte=%q", body[0])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for body")
	}
}
