package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/ffxnexus/nexus/internal/gateway"
	"github.com/ffxnexus/nexus/internal/observability"
)

type demoProvider struct{}

func (demoProvider) Name() string                                          { return "demo" }
func (demoProvider) Models() []string                                      { return []string{"demo"} }
func (demoProvider) ChatCompletion(_ context.Context, _ gateway.ChatCompletionRequest) (*gateway.ChatCompletionResponse, error) {
	return nil, errors.New("n/a")
}

func (demoProvider) ChatCompletionStream(_ context.Context, _ gateway.ChatCompletionRequest) (<-chan gateway.StreamEvent, error) {
	ch := make(chan gateway.StreamEvent, 4)
	go func() {
		defer close(ch)
		zero := 0
		// 1) Open the tool call with id+name
		ch <- gateway.StreamEvent{Chunk: &gateway.ChatCompletionChunk{
			ID: "demo", Object: "chat.completion.chunk", Model: "demo",
			Choices: []gateway.ChunkChoice{{Index: 0, Delta: gateway.Delta{
				Role: "assistant",
				ToolCalls: []gateway.ToolCall{{Index: &zero, ID: "call_1", Type: "function", Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "lookup"}}},
			}}},
		}}
		// 2) Append argument chunk
		ch <- gateway.StreamEvent{Chunk: &gateway.ChatCompletionChunk{
			Choices: []gateway.ChunkChoice{{Index: 0, Delta: gateway.Delta{
				ToolCalls: []gateway.ToolCall{{Index: &zero, Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Arguments: `{"q":"weather"}`}}},
			}}},
		}}
		// 3) Finish with tool_calls reason
		ch <- gateway.StreamEvent{Chunk: &gateway.ChatCompletionChunk{
			Choices: []gateway.ChunkChoice{{Index: 0, FinishReason: "tool_calls"}},
		}}
		ch <- gateway.StreamEvent{Done: true}
	}()
	return ch, nil
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprintf(os.Stderr, "-- upstream curl body --\n%s\n-- end body --\n", string(body))

		reg := gateway.NewRegistry()
		reg.Register(demoProvider{})
		h := gateway.NewHandler(reg, observability.NoopRecorder{}, nil, slog.Default())

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(200)

		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		req = req.WithContext(r.Context())
		h.Responses(rec, req)
		scanner := bufio.NewScanner(rec.Body)
		scanner.Buffer(make([]byte, 0, 4096), 1<<20)
		flusher, _ := w.(http.Flusher)
		for scanner.Scan() {
			_, _ = io.WriteString(w, scanner.Text()+"\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	payload := `{
		"model":"demo",
		"stream":true,
		"input":[
			{"role":"user","content":"what is the weather?"}
		],
		"tools":[
			{"type":"function","name":"lookup","description":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}
		]
	}`
	req, err := http.NewRequest("POST", srv.URL+"/v1/responses", strings.NewReader(payload))
	if err != nil {
		fmt.Println("req err:", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println("do err:", err)
		return
	}
	defer resp.Body.Close()
	fmt.Println("HTTP", resp.Status)
	rdr := bufio.NewScanner(resp.Body)
	rdr.Buffer(make([]byte, 0, 4096), 1<<20)
	for rdr.Scan() {
		fmt.Println(rdr.Text())
	}
	_ = json.RawMessage{}
}
