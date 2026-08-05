package benchmark

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestResolveEnvironmentIDPassesThroughInternalID(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "pit_test", nil)
	id, err := c.resolveEnvironmentID(context.Background(), "b4aufeb65gi793j4r4lcn0rz")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id != "b4aufeb65gi793j4r4lcn0rz" {
		t.Fatalf("id = %q", id)
	}
}

func TestResolveEnvironmentIDMapsSlug(t *testing.T) {
	c, _ := serveRoutes(t, map[string]route{
		"GET /api/v1/environmentshub/primeintellect/gsm8k/status": {
			status: http.StatusOK,
			body:   `{"data":{"id":"b4aufeb65gi793j4r4lcn0rz"}}`,
		},
	})
	id, err := c.resolveEnvironmentID(context.Background(), "primeintellect/gsm8k")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id != "b4aufeb65gi793j4r4lcn0rz" {
		t.Fatalf("id = %q", id)
	}
}

func TestResolveEnvironmentIDSurfacesMissingSlug(t *testing.T) {
	c, _ := serveRoutes(t, map[string]route{
		"GET /api/v1/environmentshub/your-org/nope/status": {
			status: http.StatusNotFound,
			body:   `{"detail":"Not Found"}`,
		},
	})
	_, err := c.resolveEnvironmentID(context.Background(), "your-org/nope")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want 404", err)
	}
}
