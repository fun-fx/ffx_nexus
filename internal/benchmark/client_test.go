package benchmark

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// capture records what the client actually put on the wire. The vendor
// contract is the whole value of this package, so the tests assert on
// the request shape and not just on a decoded reply.
type capture struct {
	method string
	path   string
	auth   string
	accept string
	body   map[string]any
}

func serve(t *testing.T, status int, reply string) (*Client, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.auth = r.Header.Get("Authorization")
		got.accept = r.Header.Get("Accept")
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &got.body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "pit_test", srv.Client()), got
}

func goodLaunch() LaunchRequest {
	return LaunchRequest{
		Environments: []string{"ffx/gsm8k"},
		Model:        "gpt-4o-mini",
		NumExamples:  5,
		Rollouts:     1,
	}
}

func TestLaunchSendsVendorShape(t *testing.T) {
	c, got := serve(t, http.StatusCreated,
		`{"evaluation_id":"ev_1","status":"PENDING","sandbox_id":"sb_1"}`)

	res, err := c.Launch(context.Background(), goodLaunch())
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.EvaluationID != "ev_1" || res.Status != "PENDING" {
		t.Fatalf("decoded wrong: %+v", res)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %s, want POST", got.method)
	}
	if got.path != "/api/v1/hosted-evaluations" {
		t.Errorf("path = %s", got.path)
	}
	if got.auth != "Bearer pit_test" {
		t.Errorf("auth = %q", got.auth)
	}
	// environment_ids and inference_model are the vendor's names; a
	// rename here is a silent 422 in production.
	envs, ok := got.body["environment_ids"].([]any)
	if !ok || len(envs) != 1 || envs[0] != "ffx/gsm8k" {
		t.Errorf("environment_ids = %#v", got.body["environment_ids"])
	}
	if got.body["inference_model"] != "gpt-4o-mini" {
		t.Errorf("inference_model = %#v", got.body["inference_model"])
	}
	cfg, ok := got.body["eval_config"].(map[string]any)
	if !ok {
		t.Fatalf("eval_config missing: %#v", got.body)
	}
	if cfg["num_examples"] != float64(5) || cfg["rollouts_per_example"] != float64(1) {
		t.Errorf("eval_config = %#v", cfg)
	}
	// Absent rather than zero: the vendor rejects timeout_minutes
	// below 120, so a zero would turn every default run into a 422.
	if _, present := cfg["timeout_minutes"]; present {
		t.Errorf("timeout_minutes must be omitted when unset, got %#v", cfg["timeout_minutes"])
	}
	// Likewise the gateway fields: sending an empty api_base_url would
	// override the vendor's own inference default with nothing.
	for _, k := range []string{"api_base_url", "api_key_var", "custom_secrets"} {
		if _, present := cfg[k]; present {
			t.Errorf("%s must be omitted when not routing through the gateway", k)
		}
	}
}

func TestLaunchRoutesInferenceThroughGateway(t *testing.T) {
	c, got := serve(t, http.StatusCreated, `{"evaluation_id":"ev_2","status":"PENDING"}`)

	req := goodLaunch()
	req.BaseURL = "https://nexus.example.ai/v1"
	req.KeyVar = "NEXUS_API_KEY"
	req.Secrets = map[string]string{"NEXUS_API_KEY": "nxs_live_secret"}
	req.TimeoutMinutes = 240

	if _, err := c.Launch(context.Background(), req); err != nil {
		t.Fatalf("launch: %v", err)
	}
	cfg := got.body["eval_config"].(map[string]any)
	if cfg["api_base_url"] != "https://nexus.example.ai/v1" {
		t.Errorf("api_base_url = %#v", cfg["api_base_url"])
	}
	if cfg["api_key_var"] != "NEXUS_API_KEY" {
		t.Errorf("api_key_var = %#v", cfg["api_key_var"])
	}
	secrets, ok := cfg["custom_secrets"].(map[string]any)
	if !ok || secrets["NEXUS_API_KEY"] != "nxs_live_secret" {
		t.Errorf("custom_secrets = %#v", cfg["custom_secrets"])
	}
	if cfg["timeout_minutes"] != float64(240) {
		t.Errorf("timeout_minutes = %#v", cfg["timeout_minutes"])
	}
}

func TestLaunchValidationRejectsBadRequests(t *testing.T) {
	// No server: validation must fail before any network call, so a
	// nil transport would panic if the order were wrong.
	c := NewClient("http://127.0.0.1:1", "pit_test", nil)

	cases := []struct {
		name string
		want string
		mut  func(*LaunchRequest)
	}{
		{"no environment", "environment", func(r *LaunchRequest) { r.Environments = nil }},
		{"empty slug", "must not be empty", func(r *LaunchRequest) { r.Environments = []string{""} }},
		{"no model", "model is required", func(r *LaunchRequest) { r.Model = "" }},
		{"zero examples", "num_examples", func(r *LaunchRequest) { r.NumExamples = 0 }},
		{"all examples sentinel", "num_examples", func(r *LaunchRequest) { r.NumExamples = -1 }},
		{"zero rollouts", "rollouts", func(r *LaunchRequest) { r.Rollouts = 0 }},
		{"over cap", "cap", func(r *LaunchRequest) { r.NumExamples = 1000; r.Rollouts = 3 }},
		{"short timeout", "timeout_minutes", func(r *LaunchRequest) { r.TimeoutMinutes = 5 }},
		{"gateway without key var", "api key variable", func(r *LaunchRequest) {
			r.BaseURL = "https://nexus.example.ai/v1"
		}},
		{"gateway without secret", "need a secret", func(r *LaunchRequest) {
			r.BaseURL = "https://nexus.example.ai/v1"
			r.KeyVar = "NEXUS_API_KEY"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := goodLaunch()
			tc.mut(&req)
			_, err := c.Launch(context.Background(), req)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLaunchAtCapIsAllowed(t *testing.T) {
	c, _ := serve(t, http.StatusCreated, `{"evaluation_id":"ev_3","status":"PENDING"}`)
	req := goodLaunch()
	req.NumExamples = MaxTotalSamples
	req.Rollouts = 1
	if _, err := c.Launch(context.Background(), req); err != nil {
		t.Fatalf("exactly at the cap must be allowed: %v", err)
	}
}

func TestLaunchWithoutTokenDoesNotCallVendor(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "", nil)
	if _, err := c.Launch(context.Background(), goodLaunch()); !errors.Is(err, ErrNoToken) {
		t.Fatalf("err = %v, want ErrNoToken", err)
	}
}

func TestLaunchRejectsMissingEvaluationID(t *testing.T) {
	// A 201 with no id leaves us unable to poll. Treating it as
	// success would create a row that never settles.
	c, _ := serve(t, http.StatusCreated, `{"status":"PENDING","error":"sandbox quota exceeded"}`)
	_, err := c.Launch(context.Background(), goodLaunch())
	if err == nil {
		t.Fatal("want error when evaluation_id is absent")
	}
	if !strings.Contains(err.Error(), "sandbox quota exceeded") {
		t.Fatalf("error should carry the vendor message, got %q", err)
	}
}

func TestStatusDecodesAggregate(t *testing.T) {
	c, got := serve(t, http.StatusOK, `{
		"evaluation_id":"ev_9","name":"nightly","status":"COMPLETED",
		"is_hosted":true,"inference_model":"gpt-4o-mini",
		"environment_names":["ffx/gsm8k"],"total_samples":20,
		"avg_score":0.82,"min_score":0.0,"max_score":1.0,
		"metrics":{"accuracy":0.82},
		"viewer_url":"https://app.primeintellect.ai/evals/ev_9",
		"started_at":"2026-08-03T01:00:00Z","completed_at":"2026-08-03T01:12:00Z"}`)

	ev, err := c.Status(context.Background(), "ev_9")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got.path != "/api/v1/evaluations/ev_9" {
		t.Errorf("path = %s, want the general evaluations route", got.path)
	}
	if got.method != http.MethodGet {
		t.Errorf("method = %s", got.method)
	}
	if ev.Status != "COMPLETED" || ev.TotalSamples != 20 {
		t.Errorf("decoded wrong: %+v", ev)
	}
	if ev.AvgScore == nil || *ev.AvgScore != 0.82 {
		t.Errorf("avg_score = %v", ev.AvgScore)
	}
	// A real zero must survive as a zero, not collapse into "absent".
	if ev.MinScore == nil || *ev.MinScore != 0 {
		t.Errorf("min_score = %v, want a present zero", ev.MinScore)
	}
	if ev.ViewerURL == "" || ev.Metrics["accuracy"] != 0.82 {
		t.Errorf("viewer/metrics wrong: %+v", ev)
	}
	if ev.StartedAt == nil || ev.CompletedAt == nil {
		t.Errorf("timestamps not decoded: %+v", ev)
	}
}

func TestStatusLeavesUnscoredRunNil(t *testing.T) {
	c, _ := serve(t, http.StatusOK, `{"evaluation_id":"ev_r","status":"RUNNING"}`)
	ev, err := c.Status(context.Background(), "ev_r")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if ev.AvgScore != nil {
		t.Fatalf("avg_score = %v, want nil for a run with no result yet", *ev.AvgScore)
	}
}

func TestStatusRequiresID(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "pit_test", nil)
	if _, err := c.Status(context.Background(), ""); err == nil {
		t.Fatal("want error for empty evaluation id")
	}
}

func TestErrorMessagesNameTheActualCause(t *testing.T) {
	cases := []struct {
		name   string
		status int
		reply  string
		want   []string
	}{
		{
			name:   "unpublished environment",
			status: http.StatusNotFound,
			reply:  `{"detail":"Environment primeintellect/gsm8k not found"}`,
			want:   []string{"404", "Environment primeintellect/gsm8k not found", "not published"},
		},
		{
			name:   "bad token",
			status: http.StatusUnauthorized,
			reply:  `{"detail":"Authorization failed"}`,
			want:   []string{"401", "provider API key"},
		},
		{
			name:   "validation errors array",
			status: http.StatusUnprocessableEntity,
			reply:  `{"errors":[{"param":"eval_config","details":"rollouts_per_example too large"}]}`,
			want:   []string{"422", "eval_config", "rollouts_per_example too large"},
		},
		{
			name:   "no balance",
			status: http.StatusPaymentRequired,
			reply:  `{"detail":"Insufficient balance"}`,
			want:   []string{"402", "billing", "Insufficient balance"},
		},
		{
			name:   "intercepted by a proxy",
			status: http.StatusBadGateway,
			reply:  `<!DOCTYPE html><html><body>Bad gateway</body></html>`,
			want:   []string{"HTML", "502", "intercepted"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := serve(t, tc.status, tc.reply)
			_, err := c.Launch(context.Background(), goodLaunch())
			if err == nil {
				t.Fatal("want an error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

func TestCancelReportsRefusal(t *testing.T) {
	c, got := serve(t, http.StatusOK,
		`{"success":false,"message":"evaluation already completed","evaluation_id":"ev_1","status":"COMPLETED"}`)
	err := c.Cancel(context.Background(), "ev_1")
	if err == nil || !strings.Contains(err.Error(), "already completed") {
		t.Fatalf("err = %v, want the vendor refusal message", err)
	}
	if got.method != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", got.method)
	}
	if got.path != "/api/v1/hosted-evaluations/ev_1/cancel" {
		t.Errorf("path = %s", got.path)
	}
}

func TestCancelSuccess(t *testing.T) {
	c, _ := serve(t, http.StatusOK,
		`{"success":true,"message":"cancelled","evaluation_id":"ev_1","status":"CANCELLED"}`)
	if err := c.Cancel(context.Background(), "ev_1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
}

func TestModelsAndLogs(t *testing.T) {
	c, got := serve(t, http.StatusOK,
		`{"models":[{"id":"openai/gpt-4o-mini","name":"openai/gpt-4o-mini","provider":"Openai","pricing":{"prompt":0.15,"completion":0.6}}]}`)
	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 1 || models[0].ID != "openai/gpt-4o-mini" {
		t.Fatalf("models = %+v", models)
	}
	if models[0].Pricing.Completion != 0.6 {
		t.Errorf("pricing not decoded: %+v", models[0].Pricing)
	}
	if got.path != "/api/v1/hosted-evaluations/models" {
		t.Errorf("path = %s", got.path)
	}

	c2, got2 := serve(t, http.StatusOK, `{"logs":"line one\nline two","evaluation_id":"ev_1"}`)
	logs, err := c2.Logs(context.Background(), "ev_1")
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(logs, "line two") {
		t.Errorf("logs = %q", logs)
	}
	if got2.path != "/api/v1/hosted-evaluations/ev_1/logs" {
		t.Errorf("path = %s", got2.path)
	}
}

func TestNormalizeStatusCoversVendorStateMachine(t *testing.T) {
	cases := map[string]string{
		"PENDING":    StatusPending,
		"RUNNING":    StatusRunning,
		"PROCESSING": StatusRunning,
		"COMPLETED":  StatusCompleted,
		"FAILED":     StatusFailed,
		"TIMEOUT":    StatusFailed,
		"CANCELLED":  StatusCancelled,
		// An unrecognised state must keep the run alive: treating a
		// new vendor state as terminal would abandon a live run.
		"SOME_NEW_STATE": StatusRunning,
		"":               StatusRunning,
	}
	for vendor, want := range cases {
		if got := NormalizeStatus(vendor); got != want {
			t.Errorf("NormalizeStatus(%q) = %q, want %q", vendor, got, want)
		}
	}
}

func TestSettledOnlyForTerminalStates(t *testing.T) {
	for _, s := range []string{StatusCompleted, StatusFailed, StatusCancelled} {
		if !Settled(s) {
			t.Errorf("Settled(%q) = false", s)
		}
	}
	for _, s := range []string{StatusPending, StatusRunning, "anything"} {
		if Settled(s) {
			t.Errorf("Settled(%q) = true", s)
		}
	}
}

func TestTotalSamples(t *testing.T) {
	r := LaunchRequest{NumExamples: 20, Rollouts: 3}
	if r.TotalSamples() != 60 {
		t.Fatalf("TotalSamples() = %d", r.TotalSamples())
	}
}
