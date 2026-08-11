package benchmark

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ffxnexus/nexus/internal/core"
)

// --- fakes ---

// fakeStore mirrors the Postgres semantics that the runner depends on,
// in particular that UpdateBenchmarkRunProgress preserves fields it is
// not told about (the COALESCE / NULLIF clauses). A naive overwrite
// here would let a bug through that only shows up in production.
type fakeStore struct {
	mu        sync.Mutex
	rows      map[string]core.BenchmarkRun
	scheds    []core.BenchmarkSchedule
	createErr error
	updateErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]core.BenchmarkRun{}}
}

// Schedule / recent-row methods that exist solely to satisfy the
// store interface for PR-1 (schedules) and PR-4 (leaderboard) tests.
// They hold no opinion other than "round-trip what was put in".
func (f *fakeStore) CreateBenchmarkSchedule(_ context.Context, r core.BenchmarkSchedule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.scheds == nil {
		f.scheds = []core.BenchmarkSchedule{}
	}
	f.scheds = append(f.scheds, r)
	return nil
}

func (f *fakeStore) GetBenchmarkSchedule(_ context.Context, id string) (core.BenchmarkSchedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.scheds {
		if s.ID == id {
			return s, nil
		}
	}
	return core.BenchmarkSchedule{}, errors.New("not found")
}

func (f *fakeStore) ListBenchmarkSchedules(_ context.Context, _ string, _ int) ([]core.BenchmarkSchedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]core.BenchmarkSchedule, len(f.scheds))
	copy(cp, f.scheds)
	return cp, nil
}

func (f *fakeStore) DeleteBenchmarkSchedule(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, s := range f.scheds {
		if s.ID == id {
			f.scheds = append(f.scheds[:i], f.scheds[i+1:]...)
			return nil
		}
	}
	return errors.New("not found")
}

func (f *fakeStore) ListRecentSettledByModel(_ context.Context, model string, limit int) ([]core.RecentBenchmarkRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []core.RecentBenchmarkRun{}
	for _, r := range f.rows {
		if r.Model != model {
			continue
		}
		if r.Status != "completed" || r.AvgScore == nil || r.CompletedAt == nil {
			continue
		}
		out = append(out, core.RecentBenchmarkRun{
			ID:           r.ID,
			Model:        r.Model,
			AvgScore:     r.AvgScore,
			MinScore:     r.MinScore,
			MaxScore:     r.MaxScore,
			TotalSamples: r.TotalSamples,
			CompletedAt:  r.CompletedAt,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) CreateBenchmarkRun(_ context.Context, r core.BenchmarkRun) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[r.ID] = r
	return nil
}

func (f *fakeStore) UpdateBenchmarkRunProgress(_ context.Context, r core.BenchmarkRun) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.rows[r.ID]
	if !ok {
		return core.ErrNotFound
	}
	if r.ExternalID != "" {
		cur.ExternalID = r.ExternalID
	}
	if r.ViewerURL != "" {
		cur.ViewerURL = r.ViewerURL
	}
	if len(r.Metrics) > 0 {
		cur.Metrics = r.Metrics
	}
	if r.StartedAt != nil {
		cur.StartedAt = r.StartedAt
	}
	if r.CompletedAt != nil {
		cur.CompletedAt = r.CompletedAt
	}
	cur.Status = r.Status
	cur.ExternalStatus = r.ExternalStatus
	cur.AvgScore = r.AvgScore
	cur.MinScore = r.MinScore
	cur.MaxScore = r.MaxScore
	cur.TotalSamples = r.TotalSamples
	cur.Error = r.Error
	f.rows[r.ID] = cur
	return nil
}

func (f *fakeStore) GetBenchmarkRun(_ context.Context, id string) (core.BenchmarkRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok {
		return core.BenchmarkRun{}, core.ErrNotFound
	}
	return r, nil
}

func (f *fakeStore) ListBenchmarkRuns(_ context.Context, orgID string, _ int) ([]core.BenchmarkRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []core.BenchmarkRun{}
	for _, r := range f.rows {
		if orgID == "" || r.OrgID == orgID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStore) ListUnsettledBenchmarkRuns(_ context.Context, _ int) ([]core.BenchmarkRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []core.BenchmarkRun{}
	for _, r := range f.rows {
		if !Settled(r.Status) && r.ExternalID != "" {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStore) DeleteBenchmarkRun(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[id]; !ok {
		return core.ErrNotFound
	}
	delete(f.rows, id)
	return nil
}

func (f *fakeStore) ClearBenchmarkRunVKey(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok {
		return core.ErrNotFound
	}
	r.VKeyID = ""
	f.rows[id] = r
	return nil
}

func (f *fakeStore) only(t *testing.T) core.BenchmarkRun {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rows) != 1 {
		t.Fatalf("want exactly one row, have %d", len(f.rows))
	}
	for _, r := range f.rows {
		return r
	}
	return core.BenchmarkRun{}
}

type mintedKey struct {
	orgID  string
	userID string
	name   string
	models []string
	rpm    int
}

type fakeKeys struct {
	mu      sync.Mutex
	minted  []mintedKey
	revoked []string
	err     error
}

func (f *fakeKeys) CreateVirtualKey(_ context.Context, orgID, _, userID, name string,
	models []string, rpm int, _, _ float64) (core.VirtualKey, string, error) {
	if f.err != nil {
		return core.VirtualKey{}, "", f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.minted = append(f.minted, mintedKey{orgID: orgID, userID: userID, name: name, models: models, rpm: rpm})
	id := "vk-" + name
	return core.VirtualKey{ID: id, OrgID: orgID}, "nxs_live_" + name, nil
}

func (f *fakeKeys) RevokeVirtualKey(_ context.Context, _, _, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, id)
	return nil
}

func (f *fakeKeys) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.minted), len(f.revoked)
}

type fakeTokens struct {
	token  string
	teamID string
	err    error
}

func (f fakeTokens) Token(context.Context, string) (string, error)  { return f.token, f.err }
func (f fakeTokens) TeamID(context.Context, string) (string, error) { return f.teamID, f.err }

// route maps "METHOD /path" onto a canned reply so one server can serve
// a whole lifecycle.
type route struct {
	status int
	body   string
}

func mockProvider(t *testing.T, routes map[string]route) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	seen := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		mu.Lock()
		seen = append(seen, key)
		mu.Unlock()
		rt, ok := routes[key]
		if !ok {
			w.WriteHeader(http.StatusNotImplemented)
			_, _ = w.Write([]byte(`{"detail":"unexpected route ` + key + `"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rt.status)
		_, _ = w.Write([]byte(rt.body))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func newTestRunner(t *testing.T, routes map[string]route, gatewayURL string) (*Runner, *fakeStore, *fakeKeys) {
	t.Helper()
	routes = withEnvStatus(routes, "ffx/gsm8k", "env_ffx_gsm8k")
	srv, _ := mockProvider(t, routes)
	store := newFakeStore()
	keys := &fakeKeys{}
	r := NewRunner(store, keys, fakeTokens{token: "pit_test"}, gatewayURL, nil)
	r.SetAPIBase(srv.URL, srv.Client())
	return r, store, keys
}

func spec() LaunchSpec {
	return LaunchSpec{
		OrgID:        "org-a",
		ActorID:      "user-1",
		Name:         "nightly",
		Environments: []string{"ffx/gsm8k"},
		Model:        "gpt-4o-mini",
		NumExamples:  10,
		Rollouts:     2,
	}
}

// --- launch ---

func TestLaunchViaGatewayMintsScopedKeyAndRecordsRun(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/environmentshub/ffx/gsm8k/status":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"id":"env_ffx_gsm8k"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/hosted-evaluations":
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"evaluation_id":"ev_1","status":"PENDING","sandbox_id":"sb_1"}`))
		default:
			t.Errorf("unexpected route: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer srv.Close()

	store, keys := newFakeStore(), &fakeKeys{}
	r := NewRunner(store, keys, fakeTokens{token: "pit_test", teamID: "team_bill"}, "https://api.ffx.ai/", nil)
	r.SetAPIBase(srv.URL, srv.Client())

	s := spec()
	s.ViaGateway = true
	run, err := r.Launch(context.Background(), s)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	if run.ExternalID != "ev_1" || run.Status != StatusPending {
		t.Errorf("run = %+v", run)
	}
	stored := store.only(t)
	if stored.ExternalID != "ev_1" || stored.Status != StatusPending {
		t.Errorf("stored = %+v", stored)
	}
	if !stored.ViaGateway || stored.VKeyID == "" {
		t.Errorf("gateway fields not recorded: %+v", stored)
	}

	// The key must be scoped to the single model under test, so the
	// provider's sandbox cannot reach anything else through us.
	if len(keys.minted) != 1 {
		t.Fatalf("minted %d keys, want 1", len(keys.minted))
	}
	m := keys.minted[0]
	if m.userID != "user-1" {
		t.Errorf("benchmark key user = %q, want launching actor for BYOK", m.userID)
	}
	if len(m.models) != 1 || m.models[0] != "gpt-4o-mini" {
		t.Errorf("key scope = %#v, want just the model under test", m.models)
	}
	if m.rpm != runKeyRPM {
		t.Errorf("key rpm = %d, want %d", m.rpm, runKeyRPM)
	}
	if m.orgID != "org-a" {
		t.Errorf("key org = %q", m.orgID)
	}

	// The provider must be pointed at our /v1, with the key delivered
	// through the variable it was told to read.
	cfg := gotBody["eval_config"].(map[string]any)
	if cfg["api_base_url"] != "https://api.ffx.ai/v1" {
		t.Errorf("api_base_url = %#v (trailing slash must be normalised)", cfg["api_base_url"])
	}
	if cfg["api_key_var"] != "NEXUS_API_KEY" {
		t.Errorf("api_key_var = %#v", cfg["api_key_var"])
	}
	secrets := cfg["custom_secrets"].(map[string]any)
	if !strings.HasPrefix(secrets["NEXUS_API_KEY"].(string), "nxs_live_") {
		t.Errorf("secret = %#v, want the minted gateway key", secrets["NEXUS_API_KEY"])
	}
	if gotBody["team_id"] != "team_bill" {
		t.Errorf("team_id = %#v, want team_bill", gotBody["team_id"])
	}
	// Still live: the provider needs it until the run settles.
	if _, revoked := keys.counts(); revoked != 0 {
		t.Errorf("revoked %d keys during a successful launch, want 0", revoked)
	}
}

func TestLaunchWithoutGatewayRoutingMintsNoKey(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/environmentshub/ffx/gsm8k/status":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"id":"env_ffx_gsm8k"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/hosted-evaluations":
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"evaluation_id":"ev_2","status":"RUNNING"}`))
		default:
			t.Errorf("unexpected route: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer srv.Close()

	store, keys := newFakeStore(), &fakeKeys{}
	r := NewRunner(store, keys, fakeTokens{token: "pit_test"}, "https://api.ffx.ai", nil)
	r.SetAPIBase(srv.URL, srv.Client())

	if _, err := r.Launch(context.Background(), spec()); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if minted, _ := keys.counts(); minted != 0 {
		t.Errorf("minted %d keys for a provider-inference run, want 0", minted)
	}
	cfg := gotBody["eval_config"].(map[string]any)
	if _, present := cfg["api_base_url"]; present {
		t.Error("api_base_url must be absent so the provider uses its own inference")
	}
	if store.only(t).Status != StatusRunning {
		t.Errorf("status = %q", store.only(t).Status)
	}
}

func TestLaunchRefusesGatewayRoutingWithNoPublicURL(t *testing.T) {
	r, store, keys := newTestRunner(t, nil, "")
	s := spec()
	s.ViaGateway = true
	_, err := r.Launch(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "NEXUS_PUBLIC_GATEWAY_URL") {
		t.Fatalf("err = %v, want it to name the missing setting", err)
	}
	if len(store.rows) != 0 {
		t.Error("no row should be written when the request cannot be honoured")
	}
	if minted, _ := keys.counts(); minted != 0 {
		t.Error("no key should be minted when the request cannot be honoured")
	}
	if r.GatewayRoutingAvailable() {
		t.Error("GatewayRoutingAvailable() = true with no public URL")
	}
}

func TestLaunchWithoutProviderTokenTouchesNothing(t *testing.T) {
	store, keys := newFakeStore(), &fakeKeys{}
	r := NewRunner(store, keys, fakeTokens{token: ""}, "https://api.ffx.ai", nil)

	_, err := r.Launch(context.Background(), spec())
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("err = %v, want ErrNoToken", err)
	}
	if !strings.Contains(err.Error(), "paste") {
		t.Errorf("error should say how to fix it, got %q", err)
	}
	if len(store.rows) != 0 {
		t.Error("no row should be written without a credential")
	}
	if minted, _ := keys.counts(); minted != 0 {
		t.Error("no key should be minted without a credential")
	}
}

func TestLaunchValidatesBeforeMintingAKey(t *testing.T) {
	// The ordering matters: minting first would leave a live gateway
	// key behind every rejected form submission.
	r, store, keys := newTestRunner(t, nil, "https://api.ffx.ai")
	s := spec()
	s.ViaGateway = true
	s.NumExamples = 5000 // over the sample cap

	_, err := r.Launch(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("err = %v, want the cost cap", err)
	}
	if minted, _ := keys.counts(); minted != 0 {
		t.Errorf("minted %d keys for an invalid request", minted)
	}
	if len(store.rows) != 0 {
		t.Error("no row should be written for an invalid request")
	}
}

func TestLaunchRejectedByProviderIsRecordedAndKeyRevoked(t *testing.T) {
	r, store, keys := newTestRunner(t, map[string]route{
		"POST /api/v1/hosted-evaluations": {
			status: http.StatusNotFound,
			body:   `{"detail":"Environment ffx/gsm8k not found"}`,
		},
	}, "https://api.ffx.ai")

	s := spec()
	s.ViaGateway = true
	_, err := r.Launch(context.Background(), s)
	if err == nil {
		t.Fatal("want the provider error")
	}
	if !strings.Contains(err.Error(), "not published") {
		t.Errorf("error should explain the likely cause, got %q", err)
	}
	// The failure must be visible rather than vanishing.
	stored := store.only(t)
	if stored.Status != StatusFailed {
		t.Errorf("status = %q, want failed", stored.Status)
	}
	if !strings.Contains(stored.Error, "not found") {
		t.Errorf("stored error = %q", stored.Error)
	}
	// And the key it will never use must not stay live.
	if _, revoked := keys.counts(); revoked != 1 {
		t.Errorf("revoked %d keys after a refused launch, want 1", revoked)
	}
}

func TestLaunchRevokesKeyWhenTheRowCannotBeWritten(t *testing.T) {
	srv, _ := mockProvider(t, withEnvStatus(map[string]route{
		"POST /api/v1/hosted-evaluations": {
			status: http.StatusCreated,
			body:   `{"evaluation_id":"ev_1","status":"PENDING"}`,
		},
	}, "ffx/gsm8k", "env_ffx_gsm8k"))
	store := newFakeStore()
	store.createErr = errors.New("database is down")
	keys := &fakeKeys{}
	r := NewRunner(store, keys, fakeTokens{token: "pit_test"}, "https://api.ffx.ai", nil)
	r.SetAPIBase(srv.URL, srv.Client())

	s := spec()
	s.ViaGateway = true
	if _, err := r.Launch(context.Background(), s); err == nil {
		t.Fatal("want the store error")
	}
	if _, revoked := keys.counts(); revoked != 1 {
		t.Errorf("revoked %d keys, want the orphaned one cleaned up", revoked)
	}
}

// --- dry-run ---

// TestDryRunHappyPath confirms that credentials + a known slug
// round-trip cleanly through POST + PATCH without the runner
// persisting a row. The Cancel call back is what flips the test
// from "create succeeded" into "probe complete": without it the
// vendor still has a launched evaluation.
func TestDryRunHappyPath(t *testing.T) {
	seen := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/environmentshub/ffx/gsm8k/status":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"id":"env_ffx_gsm8k"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/hosted-evaluations":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"evaluation_id":"ev_dry","status":"PENDING"}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/hosted-evaluations/ev_dry/cancel":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"success":true,"status":"CANCELLED"}`))
		default:
			t.Errorf("unexpected route: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer srv.Close()

	store := newFakeStore()
	r := NewRunner(store, &fakeKeys{}, fakeTokens{token: "pit_test"}, "", nil)
	r.SetAPIBase(srv.URL, srv.Client())

	if _, err := r.DryRun(context.Background(), spec()); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if len(seen) != 3 || seen[0] != "GET /api/v1/environmentshub/ffx/gsm8k/status" ||
		seen[1] != "POST /api/v1/hosted-evaluations" ||
		seen[2] != "PATCH /api/v1/hosted-evaluations/ev_dry/cancel" {
		t.Errorf("probe order = %v, want GET status → POST → PATCH ev_dry/cancel", seen)
	}
	// Critical: the probe must NOT persist a row. Otherwise the poller
	// would log a transient failed entry on every credential change.
	if len(store.rows) != 0 {
		t.Errorf("dry-run wrote rows to the store; the description says it must not (got %v)", store.rows)
	}
}

// TestDryRunMissingEnvironmentSurfaces404 is the most common
// useful answer an operator wants: "is this slug visible to my
// account?" We pin the wording so the console can display the
// vendor's reason verbatim rather than parsing a generic 404.
func TestDryRunMissingEnvironmentSurfaces404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/environmentshub/ffx/gsm8k/status" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"environment ffx/gsm8k not visible to this account"}`))
			return
		}
		w.WriteHeader(http.StatusNotImplemented)
	}))
	defer srv.Close()

	store := newFakeStore()
	r := NewRunner(store, &fakeKeys{}, fakeTokens{token: "pit_test"}, "", nil)
	r.SetAPIBase(srv.URL, srv.Client())

	_, err := r.DryRun(context.Background(), spec())
	if err == nil {
		t.Fatal("want a 404-derived error")
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "environment") {
		t.Errorf("error = %v, want a 404 message naming the missing environment", err)
	}
	// Same invariant: a probe that bails before reaching cancel because
	// the create was rejected must still leave the store untouched.
	if len(store.rows) != 0 {
		t.Errorf("dry-run wrote rows on a 404: %+v", store.rows)
	}
}

// TestDryRunRequiresCredential rejects probes before they hit the
// vendor when there is no key configured. This mirrors Launch's
// own check so the console can tell operators to paste a key before
// telling them the environments are wrong.
func TestDryRunRequiresCredential(t *testing.T) {
	store := newFakeStore()
	r := NewRunner(store, &fakeKeys{}, fakeTokens{token: ""}, "", nil)
	// SetAPIBase intentionally omitted — we want to assert that the
	// runner guards the credential before any round-trip is allowed.
	if _, err := r.DryRun(context.Background(), spec()); err == nil {
		t.Fatal("want ErrNoToken-derived error")
	}
}

// --- poll ---

func TestPollSettlesRunAndRevokesKey(t *testing.T) {
	r, store, keys := newTestRunner(t, map[string]route{
		"POST /api/v1/hosted-evaluations": {
			status: http.StatusCreated,
			body:   `{"evaluation_id":"ev_9","status":"RUNNING"}`,
		},
		"GET /api/v1/evaluations/ev_9": {
			status: http.StatusOK,
			body: `{"evaluation_id":"ev_9","status":"COMPLETED","total_samples":20,
				"avg_score":0.82,"min_score":0,"max_score":1,
				"metrics":{"accuracy":0.82},
				"viewer_url":"https://app.primeintellect.ai/evals/ev_9"}`,
		},
	}, "https://api.ffx.ai")

	s := spec()
	s.ViaGateway = true
	if _, err := r.Launch(context.Background(), s); err != nil {
		t.Fatalf("launch: %v", err)
	}
	n, err := r.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if n != 1 {
		t.Fatalf("poll updated %d runs, want 1", n)
	}
	got := store.only(t)
	if got.Status != StatusCompleted || got.ExternalStatus != "COMPLETED" {
		t.Errorf("status = %q / %q", got.Status, got.ExternalStatus)
	}
	if got.AvgScore == nil || *got.AvgScore != 0.82 {
		t.Errorf("avg = %v", got.AvgScore)
	}
	if got.MinScore == nil || *got.MinScore != 0 {
		t.Errorf("min = %v, want a present zero", got.MinScore)
	}
	if got.TotalSamples == nil || *got.TotalSamples != 20 {
		t.Errorf("samples = %v", got.TotalSamples)
	}
	if got.ViewerURL == "" || len(got.Metrics) == 0 {
		t.Errorf("viewer/metrics missing: %+v", got)
	}
	// Settled means the provider no longer needs to reach us.
	if _, revoked := keys.counts(); revoked != 1 {
		t.Errorf("revoked %d keys on settle, want 1", revoked)
	}
	if got.VKeyID != "" {
		t.Errorf("vkey reference = %q, want cleared after revoke", got.VKeyID)
	}

	// A second pass must find nothing left to do.
	if n, err := r.PollOnce(context.Background()); err != nil || n != 0 {
		t.Errorf("second poll = (%d, %v), want (0, nil)", n, err)
	}
}

func TestPollKeepsRunningRunAndItsKey(t *testing.T) {
	r, store, keys := newTestRunner(t, map[string]route{
		"POST /api/v1/hosted-evaluations": {
			status: http.StatusCreated,
			body:   `{"evaluation_id":"ev_p","status":"PENDING"}`,
		},
		"GET /api/v1/evaluations/ev_p": {
			status: http.StatusOK,
			body:   `{"evaluation_id":"ev_p","status":"PROCESSING"}`,
		},
	}, "https://api.ffx.ai")

	s := spec()
	s.ViaGateway = true
	if _, err := r.Launch(context.Background(), s); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if _, err := r.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	got := store.only(t)
	// PROCESSING is a vendor-side intermediate state; collapsing it to
	// anything terminal would abandon a live run.
	if got.Status != StatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
	if got.VKeyID == "" {
		t.Error("an unsettled run still needs its gateway key")
	}
	if _, revoked := keys.counts(); revoked != 0 {
		t.Errorf("revoked %d keys mid-run", revoked)
	}
	// The launch response gave us the external id; a status-only poll
	// must not lose it.
	if got.ExternalID != "ev_p" {
		t.Errorf("external id = %q", got.ExternalID)
	}
}

func TestPollWithoutTokenIsQuietAndHarmless(t *testing.T) {
	store := newFakeStore()
	store.rows["r1"] = core.BenchmarkRun{
		ID: "r1", ExternalID: "ev_1", Status: StatusRunning, Provider: ProviderPrime,
	}
	r := NewRunner(store, &fakeKeys{}, fakeTokens{token: ""}, "https://api.ffx.ai", nil)

	n, err := r.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("a missing key must not be a hard error: %v", err)
	}
	if n != 0 {
		t.Errorf("updated %d runs with no credential", n)
	}
	if store.rows["r1"].Status != StatusRunning {
		t.Error("the run must stay pollable once a key is pasted")
	}
}

func TestPollContinuesPastOneFailingRun(t *testing.T) {
	r, store, _ := newTestRunner(t, map[string]route{
		// ev_bad is not registered, so it answers 501.
		"GET /api/v1/evaluations/ev_ok": {
			status: http.StatusOK,
			body:   `{"evaluation_id":"ev_ok","status":"COMPLETED","avg_score":0.5,"total_samples":4}`,
		},
	}, "https://api.ffx.ai")
	store.rows["bad"] = core.BenchmarkRun{ID: "bad", ExternalID: "ev_bad", Status: StatusRunning}
	store.rows["ok"] = core.BenchmarkRun{ID: "ok", ExternalID: "ev_ok", Status: StatusRunning}

	n, err := r.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if n != 1 {
		t.Fatalf("updated %d runs, want the healthy one", n)
	}
	if store.rows["ok"].Status != StatusCompleted {
		t.Error("the healthy run did not settle")
	}
	if store.rows["bad"].Status != StatusRunning {
		t.Error("the failing run should be retried next pass, not settled")
	}
}

// --- cancel / delete / logs ---

func TestCancelStopsProviderAndSettlesRow(t *testing.T) {
	r, store, keys := newTestRunner(t, map[string]route{
		"POST /api/v1/hosted-evaluations": {
			status: http.StatusCreated,
			body:   `{"evaluation_id":"ev_c","status":"RUNNING"}`,
		},
		"PATCH /api/v1/hosted-evaluations/ev_c/cancel": {
			status: http.StatusOK,
			body:   `{"success":true,"message":"cancelled","evaluation_id":"ev_c","status":"CANCELLED"}`,
		},
	}, "https://api.ffx.ai")

	s := spec()
	s.ViaGateway = true
	run, err := r.Launch(context.Background(), s)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if err := r.Cancel(context.Background(), run.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got := store.only(t)
	if got.Status != StatusCancelled {
		t.Errorf("status = %q", got.Status)
	}
	if _, revoked := keys.counts(); revoked != 1 {
		t.Errorf("revoked %d keys on cancel, want 1", revoked)
	}
	if got.VKeyID != "" {
		t.Errorf("vkey reference = %q, want cleared", got.VKeyID)
	}
	// A settled run cannot be cancelled twice.
	if err := r.Cancel(context.Background(), run.ID); err == nil {
		t.Error("cancelling a settled run should be refused")
	}
}

func TestCancelSettlesLocallyWhenProviderRefuses(t *testing.T) {
	// The provider losing a race must not leave our row "running"
	// forever after an operator asked it to stop.
	r, store, _ := newTestRunner(t, map[string]route{
		"PATCH /api/v1/hosted-evaluations/ev_x/cancel": {
			status: http.StatusOK,
			body:   `{"success":false,"message":"already completed","evaluation_id":"ev_x","status":"COMPLETED"}`,
		},
	}, "https://api.ffx.ai")
	store.rows["r1"] = core.BenchmarkRun{ID: "r1", ExternalID: "ev_x", Status: StatusRunning}

	if err := r.Cancel(context.Background(), "r1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if store.rows["r1"].Status != StatusCancelled {
		t.Errorf("status = %q, want cancelled locally", store.rows["r1"].Status)
	}
}

func TestDeleteCancelsALiveRunFirst(t *testing.T) {
	// Deleting the record of a live run would leave a billable job at
	// the provider with nothing tracking it.
	r, store, _ := newTestRunner(t, map[string]route{
		"PATCH /api/v1/hosted-evaluations/ev_d/cancel": {
			status: http.StatusOK,
			body:   `{"success":true,"message":"cancelled","evaluation_id":"ev_d","status":"CANCELLED"}`,
		},
	}, "https://api.ffx.ai")
	store.rows["r1"] = core.BenchmarkRun{ID: "r1", ExternalID: "ev_d", Status: StatusRunning}

	if err := r.Delete(context.Background(), "r1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(store.rows) != 0 {
		t.Errorf("row not deleted: %+v", store.rows)
	}
}

func TestLogsRequiresAStartedRun(t *testing.T) {
	r, store, _ := newTestRunner(t, nil, "https://api.ffx.ai")
	store.rows["r1"] = core.BenchmarkRun{ID: "r1", Status: StatusFailed}
	_, err := r.Logs(context.Background(), "r1")
	if err == nil || !strings.Contains(err.Error(), "never started") {
		t.Fatalf("err = %v, want an explanation", err)
	}
}

func TestListScopesToOrg(t *testing.T) {
	r, store, _ := newTestRunner(t, nil, "https://api.ffx.ai")
	store.rows["a"] = core.BenchmarkRun{ID: "a", OrgID: "org-a"}
	store.rows["b"] = core.BenchmarkRun{ID: "b", OrgID: "org-b"}

	got, err := r.List(context.Background(), "org-a", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("list = %+v", got)
	}
}

func TestUnconfiguredRunnerReportsRatherThanPanics(t *testing.T) {
	var r *Runner
	if _, err := r.Launch(context.Background(), spec()); err == nil {
		t.Error("Launch on a nil runner should error")
	}
	if _, err := r.PollOnce(context.Background()); err == nil {
		t.Error("PollOnce on a nil runner should error")
	}
	if r.GatewayRoutingAvailable() {
		t.Error("GatewayRoutingAvailable on a nil runner should be false")
	}
}
