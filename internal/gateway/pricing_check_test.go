package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func catalogJSON(entries map[string]catalogPrice) []byte {
	b, err := json.Marshal(entries)
	if err != nil {
		panic(err)
	}
	return b
}

func TestCompareCatalogJSON_Match(t *testing.T) {
	body := catalogJSON(map[string]catalogPrice{
		"gpt-4o-mini": {InputCostPerToken: 0.15 / 1e6, OutputCostPerToken: 0.60 / 1e6},
	})
	got, err := compareCatalogJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range got {
		if d.Model == "gpt-4o-mini" {
			t.Fatalf("gpt-4o-mini should match the table, got %+v", d)
		}
	}
}

func TestCompareCatalogJSON_Drift(t *testing.T) {
	body := catalogJSON(map[string]catalogPrice{
		"gpt-4o-mini": {InputCostPerToken: 1.50 / 1e6, OutputCostPerToken: 0.60 / 1e6},
	})
	got, err := compareCatalogJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range got {
		if d.Model == "gpt-4o-mini" {
			found = true
			if d.OursIn != 0.15 || d.CatalogIn != 1.50 {
				t.Fatalf("unexpected drift %+v", d)
			}
		}
	}
	if !found {
		t.Fatal("expected gpt-4o-mini drift")
	}
}

func TestCompareCatalogJSON_IgnoresGrid(t *testing.T) {
	body := catalogJSON(map[string]catalogPrice{
		"grid/code-prime": {InputCostPerToken: 99 / 1e6, OutputCostPerToken: 99 / 1e6},
		"code-prime":      {InputCostPerToken: 99 / 1e6, OutputCostPerToken: 99 / 1e6},
	})
	got, err := compareCatalogJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range got {
		if strings.HasPrefix(d.Model, "grid/") {
			t.Fatalf("grid instruments must be skipped, got %+v", d)
		}
	}
}

func TestCompareCatalogJSON_MissingOrNonPositiveSkipped(t *testing.T) {
	body := catalogJSON(map[string]catalogPrice{
		"gpt-4o-mini": {InputCostPerToken: -1, OutputCostPerToken: 0.60 / 1e6},
	})
	got, err := compareCatalogJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range got {
		if d.Model == "gpt-4o-mini" {
			t.Fatalf("non-positive catalog prices must skip, got %+v", d)
		}
	}
}

func TestCatalogKeysCoverNonGridPricingTable(t *testing.T) {
	for model := range pricingTable {
		if strings.HasPrefix(model, "grid/") {
			if _, ok := catalogKeys[model]; ok {
				t.Errorf("grid model %q must not be in catalogKeys", model)
			}
			continue
		}
		if _, ok := catalogKeys[model]; !ok {
			t.Errorf("pricingTable key %q missing from catalogKeys", model)
		}
	}
}

func TestPriceDriftedTolerance(t *testing.T) {
	if priceDrifted(0.15, 0.155) {
		t.Fatal("delta 0.005 should be within $0.01 floor")
	}
	if !priceDrifted(2.50, 5.00) {
		t.Fatal("2x change must count as drift")
	}
}

func TestPricingChecker_KeepsPreviousOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(catalogJSON(map[string]catalogPrice{
			"gpt-4o-mini": {InputCostPerToken: 1.50 / 1e6, OutputCostPerToken: 0.60 / 1e6},
		}))
	}))
	t.Cleanup(srv.Close)

	c := NewPricingChecker(PricingCheckOptions{
		URL:      srv.URL,
		Interval: time.Hour,
		Client:   srv.Client(),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	c.RunOnce(context.Background())
	first := c.Snapshot()
	if len(first.Drifts) == 0 {
		t.Fatal("expected drift on first check")
	}

	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(fail.Close)
	c.url = fail.URL
	c.client = fail.Client()
	c.RunOnce(context.Background())
	second := c.Snapshot()
	if second.LastError == "" {
		t.Fatal("expected last_error after failed fetch")
	}
	if len(second.Drifts) != len(first.Drifts) {
		t.Fatalf("failed check must keep previous drifts, got %d want %d", len(second.Drifts), len(first.Drifts))
	}
	if second.ErrorsTotal < 1 {
		t.Fatal("expected error counter increment")
	}
}

func TestPricingChecker_BodyCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytesRepeat('x', 64))
	}))
	t.Cleanup(srv.Close)

	c := NewPricingChecker(PricingCheckOptions{
		URL:      srv.URL,
		Interval: time.Hour,
		Client:   srv.Client(),
		MaxBytes: 16,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	c.RunOnce(context.Background())
	snap := c.Snapshot()
	if snap.LastError == "" {
		t.Fatal("expected body-cap error")
	}
	if !strings.Contains(snap.LastError, "exceeds") {
		t.Fatalf("last_error = %q, want exceeds", snap.LastError)
	}
}

func TestPricingChecker_TimeoutKeepsPrevious(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(catalogJSON(map[string]catalogPrice{
			"gpt-4o-mini": {InputCostPerToken: 0.15 / 1e6, OutputCostPerToken: 0.60 / 1e6},
		}))
	}))
	t.Cleanup(srv.Close)

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(catalogJSON(map[string]catalogPrice{
			"gpt-4o-mini": {InputCostPerToken: 9 / 1e6, OutputCostPerToken: 9 / 1e6},
		}))
	}))
	t.Cleanup(ok.Close)

	c := NewPricingChecker(PricingCheckOptions{
		URL:      ok.URL,
		Interval: time.Hour,
		Client:   ok.Client(),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	c.RunOnce(context.Background())
	if len(c.Snapshot().Drifts) == 0 {
		t.Fatal("precondition: first check should drift")
	}

	c.url = srv.URL
	c.client = &http.Client{Timeout: 20 * time.Millisecond}
	c.RunOnce(context.Background())
	snap := c.Snapshot()
	if snap.LastError == "" {
		t.Fatal("expected timeout error")
	}
	if len(snap.Drifts) == 0 {
		t.Fatal("timeout must keep previous drifts")
	}
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
