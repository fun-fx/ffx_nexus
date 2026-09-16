package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/gateway"
)

type stubPricingDrift struct{ snap gateway.PricingCheckSnapshot }

func (s stubPricingDrift) Snapshot() gateway.PricingCheckSnapshot { return s.snap }

func TestPricingDriftDisabled(t *testing.T) {
	srv := NewServer(NewHub(), nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/stats/pricing-drift", nil)
	rec := httptest.NewRecorder()
	srv.getPricingDrift(rec, req, core.User{Role: core.RoleAdmin})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var got gateway.PricingCheckSnapshot
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("worker off must report enabled=false")
	}
	if !got.BillingUnchanged {
		t.Fatal("billing_unchanged must stay true")
	}
}

func TestPricingDriftReportsSnapshot(t *testing.T) {
	srv := NewServer(NewHub(), nil, nil, nil)
	srv.SetPricingDrift(stubPricingDrift{snap: gateway.PricingCheckSnapshot{
		Enabled:          true,
		BillingUnchanged: true,
		Drifts: []gateway.PricingDrift{{
			Model: "gpt-4o-mini", OursIn: 0.15, OursOut: 0.60, CatalogIn: 1.5, CatalogOut: 0.60,
		}},
	}})
	req := httptest.NewRequest(http.MethodGet, "/api/stats/pricing-drift", nil)
	rec := httptest.NewRecorder()
	srv.getPricingDrift(rec, req, core.User{Role: core.RoleAdmin})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var got gateway.PricingCheckSnapshot
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Drifts) != 1 || got.Drifts[0].Model != "gpt-4o-mini" {
		t.Fatalf("unexpected snapshot %+v", got)
	}
}
