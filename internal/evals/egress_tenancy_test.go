package evals

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/egress"
	"github.com/ffxnexus/nexus/internal/observability"
)

// The scenario: org A configures a destination, org B's data is what leaves.
//
// This is the shape of the worst defect the review found, and it deserves a test
// separate from the profile-filter unit tests for one reason: the filter is a
// single condition in one function, and the consequence of losing it is not a
// wrong HTTP status but another tenant's prompts arriving at a URL the attacker
// chose. A test asserting on the filter's return value would still pass if the
// call site were deleted. This one asserts on what reaches a socket.

// judgeSpy stands in for the endpoint org A puts in its profile, and records
// every body it is sent.
type judgeSpy struct {
	mu     sync.Mutex
	bodies []string
	url    string
}

func newJudgeSpy(t *testing.T) *judgeSpy {
	t.Helper()
	spy := &judgeSpy{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spy.mu.Lock()
		spy.bodies = append(spy.bodies, string(body))
		spy.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{"content": `{"score":0.9,"rationale":"ok"}`},
			}},
		})
	}))
	t.Cleanup(srv.Close)
	spy.url = srv.URL
	return spy
}

func (s *judgeSpy) sawContaining(needle string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.bodies {
		if strings.Contains(b, needle) {
			return true
		}
	}
	return false
}

// spyProfile is an org's judge profile pointed at the spy.
func spyProfile(id, org, endpoint string) EvalProfile {
	p := orgJudgeProfile(id, org)
	p.Endpoint.BaseURL = endpoint
	return p
}

// traceWithContent carries a string distinctive enough that finding it in a
// request body is unambiguous.
func traceWithContent(org, content string) observability.Trace {
	return observability.Trace{
		TraceID:        "trace-" + org,
		OrgID:          org,
		RequestModel:   "gpt-4o",
		ResponseModel:  "gpt-4o",
		InputMessages:  `[{"role":"user","content":"` + content + `"}]`,
		OutputMessages: `[{"role":"assistant","content":"answer"}]`,
	}
}

// runEvaluators materialises the evaluators for a trace and runs each one, which
// is what the worker does. Errors are ignored: the assertion is on what the spy
// received, not on whether scoring succeeded.
func runEvaluators(t *testing.T, w *Worker, trace observability.Trace, profiles []EvalProfile) {
	t.Helper()
	for _, ev := range w.collectEvaluators(trace, profiles, nil, 1.0, anySecret) {
		_, _ = ev.Evaluate(context.Background(), trace)
	}
}

// Org A's judge endpoint must never receive org B's trace content.
func TestOneOrgsJudgeEndpointNeverReceivesAnotherOrgsContent(t *testing.T) {
	egress.TestingAllowLoopback(t) // the spy is an httptest server on loopback

	spy := newJudgeSpy(t)
	w := tenancyWorker(t)
	profiles := []EvalProfile{spyProfile("org-a-judge", "org-a", spy.url)}

	const orgBSecret = "ORG-B-CONFIDENTIAL-PROMPT-DO-NOT-EGRESS"
	runEvaluators(t, w, traceWithContent("org-b", orgBSecret), profiles)

	if spy.sawContaining(orgBSecret) {
		t.Fatal("org B's prompt content was POSTed to the endpoint org A configured.\n" +
			"This is cross-tenant data egress rather than an authorization error: " +
			"the content has left the installation and cannot be recalled.")
	}

	// Non-vacuity. Org A's own trace must reach the same endpoint, or the
	// assertion above would hold simply because nothing is ever dispatched.
	const orgAContent = "ORG-A-OWN-PROMPT"
	runEvaluators(t, w, traceWithContent("org-a", orgAContent), profiles)
	if !spy.sawContaining(orgAContent) {
		t.Error("org A's own trace never reached org A's endpoint, so the negative " +
			"assertion above proves nothing about the tenant filter")
	}
}

// A cluster-wide profile is the operator's deliberate decision that every org's
// traces go to one destination. That must keep working, or the classification in
// docs/tenancy-model.md §4 is wrong.
func TestAClusterWideProfileStillReceivesEveryOrgsContent(t *testing.T) {
	egress.TestingAllowLoopback(t)

	spy := newJudgeSpy(t)
	w := tenancyWorker(t)
	// No OrgID: operator-seeded, cluster-wide.
	profiles := []EvalProfile{spyProfile("operator-judge", "", spy.url)}

	for _, org := range []string{"org-a", "org-b"} {
		content := "CONTENT-FROM-" + org
		runEvaluators(t, w, traceWithContent(org, content), profiles)
		if !spy.sawContaining(content) {
			t.Errorf("a cluster-wide profile did not receive %s's content; operator-"+
				"seeded coverage is supposed to apply to every org", org)
		}
	}
}

// A judge endpoint pointing into the cluster or at instance metadata must fail to
// connect, whichever org configured it. This is the egress guard rather than the
// tenant filter: the org owns the profile legitimately and the destination is
// still forbidden.
func TestAJudgeEndpointCannotReachClusterInternalAddresses(t *testing.T) {
	egress.TestingStrict(t) // this test is ABOUT the destination policy

	for _, target := range []struct{ name, base string }{
		{"cloud instance metadata", "http://169.254.169.254"},
		{"the pod's own console port", "http://127.0.0.1:8081"},
		{"a ClusterIP", "http://10.4.1.9:8123"},
		{"the IPv6 form of loopback", "http://[::1]:8081"},
	} {
		judge := NewSLMJudge(JudgeConfig{
			BaseURL: target.base,
			Model:   "judge-model",
			Timeout: 2 * time.Second,
		})
		if judge == nil {
			t.Fatalf("%s: judge was not constructed", target.name)
		}
		_, err := judge.Evaluate(context.Background(), traceWithContent("org-a", "prompt"))
		if err == nil {
			t.Errorf("%s: the judge reached %s", target.name, target.base)
			continue
		}
		if !strings.Contains(err.Error(), "not permitted") {
			t.Errorf("%s: blocked for the wrong reason, so the guard may not be what "+
				"stopped it: %v", target.name, err)
		}
	}
}

// The metadata-exfiltration path end to end: what a successful fetch would have
// produced is a score rationale, which the console renders. Proving the fetch
// fails is only half the point; this pins that no score is produced either, so a
// future change that swallows the error cannot turn it back into a stored value.
func TestABlockedJudgeProducesNoScoreRatherThanAnErrorScore(t *testing.T) {
	egress.TestingStrict(t)

	judge := NewSLMJudge(JudgeConfig{
		BaseURL: "http://169.254.169.254/latest/meta-data/iam/security-credentials",
		Model:   "judge-model",
		Timeout: 2 * time.Second,
	})
	scores, err := judge.Evaluate(context.Background(), traceWithContent("org-a", "prompt"))
	if err == nil {
		t.Fatal("the judge reported success against the metadata service")
	}
	if len(scores) > 0 {
		t.Errorf("a blocked judge returned %d score(s); whatever the metadata service "+
			"would have replied must never become a persisted rationale: %+v",
			len(scores), scores)
	}
}
