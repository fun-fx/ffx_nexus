package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeModerationsProvider is a ModerationsProvider used in handler tests.
// Behaves like OpenAI for the moderation route without hitting the network.
type fakeModerationsProvider struct {
	gotReq ModerationRequest
	resp   *ModerationResponse
}

func (p *fakeModerationsProvider) Name() string              { return "fake-mod" }
func (p *fakeModerationsProvider) Models() []string          { return nil }
func (p *fakeModerationsProvider) EmbeddingModels() []string { return nil }
func (p *fakeModerationsProvider) ImageModels() []string     { return nil }
func (p *fakeModerationsProvider) ModerationModels() []string {
	return []string{"omni-moderation-latest"}
}
func (p *fakeModerationsProvider) Moderate(_ context.Context, req ModerationRequest) (*ModerationResponse, error) {
	p.gotReq = req
	if p.resp != nil {
		return p.resp, nil
	}
	return &ModerationResponse{
		ID:    "modr-fake",
		Model: req.Model,
		Results: []ModerationResult{{
			Flagged: false,
		}},
	}, nil
}
func (p *fakeModerationsProvider) ChatCompletion(_ context.Context, _ ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return &ChatCompletionResponse{}, nil
}
func (p *fakeModerationsProvider) ChatCompletionStream(_ context.Context, _ ChatCompletionRequest) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, 1)
	ch <- StreamEvent{Done: true}
	close(ch)
	return ch, nil
}

// fakeImageProvider is an ImageGenerationProvider used in handler tests.
type fakeImageProvider struct {
	gotReq ImageGenerationRequest
	resp   *ImageGenerationResponse
}

func (p *fakeImageProvider) Name() string               { return "fake-img" }
func (p *fakeImageProvider) Models() []string           { return nil }
func (p *fakeImageProvider) EmbeddingModels() []string  { return nil }
func (p *fakeImageProvider) ModerationModels() []string { return nil }
func (p *fakeImageProvider) ImageModels() []string      { return []string{"dall-e-3"} }
func (p *fakeImageProvider) GenerateImages(_ context.Context, req ImageGenerationRequest) (*ImageGenerationResponse, error) {
	p.gotReq = req
	if p.resp != nil {
		return p.resp, nil
	}
	return &ImageGenerationResponse{
		Created: 1700000000,
		Data:    []ImageDataItem{{URL: "https://example.invalid/x.png"}},
	}, nil
}
func (p *fakeImageProvider) ChatCompletion(_ context.Context, _ ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return &ChatCompletionResponse{}, nil
}
func (p *fakeImageProvider) ChatCompletionStream(_ context.Context, _ ChatCompletionRequest) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, 1)
	ch <- StreamEvent{Done: true}
	close(ch)
	return ch, nil
}

// Handler tests ------------------------------

func newHandlerFor(t *testing.T, providersToRegister ...Provider) *Handler {
	t.Helper()
	h := &Handler{registry: NewRegistry()}
	for _, p := range providersToRegister {
		h.registry.Register(p)
	}
	return h
}

func TestModerationsDispatchesAndDefaultsModel(t *testing.T) {
	fp := &fakeModerationsProvider{}
	h := newHandlerFor(t, fp)

	body := `{"input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/moderations", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.Moderations(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body.String())
	}
	if fp.gotReq.Model != "omni-moderation-latest" {
		t.Fatalf("want default model; got %q", fp.gotReq.Model)
	}
	var resp ModerationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID == "" || len(resp.Results) != 1 {
		t.Fatalf("unexpected response shape: %+v", resp)
	}
}

func TestModerationsEmptyInputForwardsToProvider(t *testing.T) {
	// The handler enforces structural validation only — empty input is
	// passed through so the upstream policy rejects it (the same way
	// /v1/embeddings behaves today). This test is a regression guard:
	// don't accidentally introduce stricter validation here that would
	// diverge from the embeddings contract.
	fp := &fakeModerationsProvider{}
	h := newHandlerFor(t, fp)
	req := httptest.NewRequest(http.MethodPost, "/v1/moderations",
		strings.NewReader(`{"model":"omni-moderation-latest","input":""}`))
	w := httptest.NewRecorder()
	h.Moderations(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (forward + upstream-reject), got %d (%s)", w.Code, w.Body.String())
	}
}

func TestModerationsUnknownModelReturns404(t *testing.T) {
	fp := &fakeModerationsProvider{}
	h := newHandlerFor(t, fp)
	req := httptest.NewRequest(http.MethodPost, "/v1/moderations",
		strings.NewReader(`{"model":"mystery-mod","input":"hi"}`))
	w := httptest.NewRecorder()
	h.Moderations(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestImagesDispatches(t *testing.T) {
	fp := &fakeImageProvider{}
	h := newHandlerFor(t, fp)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"dall-e-3","prompt":"a cat","n":1,"response_format":"url"}`))
	w := httptest.NewRecorder()
	h.Images(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body.String())
	}
	if fp.gotReq.Prompt != "a cat" || fp.gotReq.N == nil || *fp.gotReq.N != 1 {
		t.Fatalf("provider did not receive the request: %+v", fp.gotReq)
	}
	var resp ImageGenerationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Created == 0 || len(resp.Data) != 1 {
		t.Fatalf("unexpected response shape: %+v", resp)
	}
}

func TestImagesDefaultsToDallE3(t *testing.T) {
	fp := &fakeImageProvider{}
	h := newHandlerFor(t, fp)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"prompt":"a dog"}`))
	w := httptest.NewRecorder()
	h.Images(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if fp.gotReq.Model != "dall-e-3" {
		t.Fatalf("want default image model; got %q", fp.gotReq.Model)
	}
}

func TestImagesRejectsEmptyPrompt(t *testing.T) {
	fp := &fakeImageProvider{}
	h := newHandlerFor(t, fp)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"prompt":"   "}`))
	w := httptest.NewRecorder()
	h.Images(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

// Registry discovery tests ------------------------------

// Registry discovery is exercised in providers/moderation_image_test.go using
// the real OpenAI adapter — gateway can't import providers (cycle).
