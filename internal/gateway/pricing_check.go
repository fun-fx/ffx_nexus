package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ffxnexus/nexus/internal/egress"
)

// DefaultPricingCatalogURL is LiteLLM's public GitHub price map. It is a
// community snapshot of vendor list prices, not a billing source: Nexus
// never writes these numbers into CostUSD or gateway_traces.
const DefaultPricingCatalogURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

const (
	defaultPricingCheckInterval = 6 * time.Hour
	pricingCatalogMaxBytes      = 8 << 20
	pricingCatalogTimeout       = 10 * time.Second
	pricingDriftAbsFloor        = 0.01 // USD / 1M tokens
	pricingDriftRelTol          = 0.01 // 1%
)

// catalogKeys maps each non-Grid pricingTable key to the LiteLLM JSON
// keys we will accept. First hit wins. A static test fails if a new
// pricingTable entry is added without a row here, so catalog coverage
// cannot silently drop.
var catalogKeys = map[string][]string{
	"gpt-4o":       {"gpt-4o", "openai/gpt-4o"},
	"gpt-4o-mini":  {"gpt-4o-mini", "openai/gpt-4o-mini"},
	"gpt-4.1":      {"gpt-4.1", "openai/gpt-4.1"},
	"gpt-4.1-mini": {"gpt-4.1-mini", "openai/gpt-4.1-mini"},
	"gpt-4.1-nano": {"gpt-4.1-nano", "openai/gpt-4.1-nano"},
	"o3":           {"o3", "openai/o3"},
	"o4-mini":      {"o4-mini", "openai/o4-mini"},

	"claude-opus-4-1":          {"claude-opus-4-1", "anthropic/claude-opus-4-1"},
	"claude-sonnet-4-5":        {"claude-sonnet-4-5", "anthropic/claude-sonnet-4-5"},
	"claude-haiku-4-5":         {"claude-haiku-4-5", "anthropic/claude-haiku-4-5"},
	"claude-3-7-sonnet-latest": {"claude-3-7-sonnet-latest", "anthropic/claude-3-7-sonnet-latest"},
	"claude-3-5-haiku-latest":  {"claude-3-5-haiku-latest", "anthropic/claude-3-5-haiku-latest"},

	"gemini-2.5-pro":   {"gemini-2.5-pro", "gemini/gemini-2.5-pro"},
	"gemini-2.5-flash": {"gemini-2.5-flash", "gemini/gemini-2.5-flash"},
	"gemini-2.0-flash": {"gemini-2.0-flash", "gemini/gemini-2.0-flash"},

	"groq/llama-3.3-70b-versatile": {"groq/llama-3.3-70b-versatile", "llama-3.3-70b-versatile"},
	"groq/llama-3.3-70b-specdec":   {"groq/llama-3.3-70b-specdec", "llama-3.3-70b-specdec"},
	"groq/llama-3.1-8b-instant":    {"groq/llama-3.1-8b-instant", "llama-3.1-8b-instant"},
	"groq/llama-3.1-70b-versatile": {"groq/llama-3.1-70b-versatile", "llama-3.1-70b-versatile"},
	"groq/llama3-8b-8192":          {"groq/llama3-8b-8192", "llama3-8b-8192"},
	"groq/llama3-70b-8192":         {"groq/llama3-70b-8192", "llama3-70b-8192"},
	"groq/mixtral-8x7b-32768":      {"groq/mixtral-8x7b-32768", "mixtral-8x7b-32768"},
	"groq/gemma2-9b-it":            {"groq/gemma2-9b-it", "gemma2-9b-it"},
	"groq/llama-guard-3-8b":        {"groq/llama-guard-3-8b", "llama-guard-3-8b"},

	"mistral/mistral-large-latest":  {"mistral/mistral-large-latest", "mistral-large-latest"},
	"mistral/mistral-medium-latest": {"mistral/mistral-medium-latest", "mistral-medium-latest"},
	"mistral/mistral-small-latest":  {"mistral/mistral-small-latest", "mistral-small-latest"},
	"mistral/mistral-small-2409":    {"mistral/mistral-small-2409", "mistral-small-2409"},
	"mistral/codestral-latest":      {"mistral/codestral-latest", "codestral-latest"},
	"mistral/codestral-2405":        {"mistral/codestral-2405", "codestral-2405"},
	"mistral/open-mistral-7b":       {"mistral/open-mistral-7b", "open-mistral-7b"},
	"mistral/open-mixtral-8x7b":     {"mistral/open-mixtral-8x7b", "open-mixtral-8x7b"},
	"mistral/ministral-8b-latest":   {"mistral/ministral-8b-latest", "ministral-8b-latest"},
	"mistral/ministral-3b-latest":   {"mistral/ministral-3b-latest", "ministral-3b-latest"},
	"mistral/pixtral-12b-2409":      {"mistral/pixtral-12b-2409", "pixtral-12b-2409"},
}

// PricingDrift is one model whose static table disagrees with the
// published catalog. It is a signal to edit pricing.go, not a rewrite
// of already-recorded gateway_traces rows.
type PricingDrift struct {
	Model      string  `json:"model"`
	OursIn     float64 `json:"ours_in_per_m"`
	OursOut    float64 `json:"ours_out_per_m"`
	CatalogIn  float64 `json:"catalog_in_per_m"`
	CatalogOut float64 `json:"catalog_out_per_m"`
}

// PricingCheckSnapshot is the last successful (or last failed) check.
// BillingUnchanged is always true: CostUSD never reads this snapshot.
type PricingCheckSnapshot struct {
	Enabled          bool           `json:"enabled"`
	CheckedAt        time.Time      `json:"checked_at,omitempty"`
	Source           string         `json:"source,omitempty"`
	Drifts           []PricingDrift `json:"drifts"`
	LastError        string         `json:"last_error,omitempty"`
	BillingUnchanged bool           `json:"billing_unchanged"`
	ErrorsTotal      uint64         `json:"-"`
}

// PricingCheckOptions configures the background catalog compare.
type PricingCheckOptions struct {
	URL      string
	Interval time.Duration
	Client   *http.Client
	Logger   *slog.Logger
	MaxBytes int64
	// OnSnapshot is invoked after every attempt (success or fail) so
	// Prometheus can scrape without importing this package's lock.
	OnSnapshot func(PricingCheckSnapshot)
}

type catalogPrice struct {
	InputCostPerToken  float64 `json:"input_cost_per_token"`
	OutputCostPerToken float64 `json:"output_cost_per_token"`
}

// PricingChecker periodically fetches a published price map and diffs it
// against pricingTable. It never mutates the table.
type PricingChecker struct {
	url      string
	interval time.Duration
	client   *http.Client
	log      *slog.Logger
	maxBytes int64
	onSnap   func(PricingCheckSnapshot)

	mu   sync.RWMutex
	snap PricingCheckSnapshot
}

// NewPricingChecker builds a checker. Start must be called to run it.
func NewPricingChecker(opts PricingCheckOptions) *PricingChecker {
	url := strings.TrimSpace(opts.URL)
	if url == "" {
		url = DefaultPricingCatalogURL
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = defaultPricingCheckInterval
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = pricingCatalogMaxBytes
	}
	client := opts.Client
	if client == nil {
		client = egress.Client(egress.Operator, pricingCatalogTimeout)
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &PricingChecker{
		url:      url,
		interval: interval,
		client:   client,
		log:      log,
		maxBytes: maxBytes,
		onSnap:   opts.OnSnapshot,
		snap: PricingCheckSnapshot{
			Enabled:          true,
			Source:           url,
			Drifts:           []PricingDrift{},
			BillingUnchanged: true,
		},
	}
}

// StartPricingCheck constructs the checker and runs it in a goroutine.
func StartPricingCheck(ctx context.Context, opts PricingCheckOptions) *PricingChecker {
	c := NewPricingChecker(opts)
	go c.loop(ctx)
	return c
}

// Snapshot returns a copy of the last check. Safe for concurrent use.
func (c *PricingChecker) Snapshot() PricingCheckSnapshot {
	if c == nil {
		return PricingCheckSnapshot{Enabled: false, Drifts: []PricingDrift{}, BillingUnchanged: true}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := c.snap
	if out.Drifts == nil {
		out.Drifts = []PricingDrift{}
	} else {
		out.Drifts = append([]PricingDrift(nil), out.Drifts...)
	}
	return out
}

func (c *PricingChecker) loop(ctx context.Context) {
	c.RunOnce(ctx)
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.RunOnce(ctx)
		}
	}
}

// RunOnce fetches and compares. On failure the previous drift list is kept.
func (c *PricingChecker) RunOnce(ctx context.Context) {
	drifts, err := c.fetchAndCompare(ctx)
	c.mu.Lock()
	c.snap.Enabled = true
	c.snap.Source = c.url
	c.snap.BillingUnchanged = true
	c.snap.CheckedAt = time.Now().UTC()
	if err != nil {
		c.snap.LastError = err.Error()
		c.snap.ErrorsTotal++
		c.log.Warn("pricing catalog check failed; keeping previous snapshot", "err", err, "url", c.url)
	} else {
		c.snap.LastError = ""
		c.snap.Drifts = drifts
		if len(drifts) == 0 {
			c.log.Debug("pricing catalog check matched", "source", c.url)
		} else {
			for _, d := range drifts {
				c.log.Warn("pricing drift",
					"model", d.Model,
					"ours_in_per_m", d.OursIn,
					"ours_out_per_m", d.OursOut,
					"catalog_in_per_m", d.CatalogIn,
					"catalog_out_per_m", d.CatalogOut)
			}
		}
	}
	snap := c.snap
	if snap.Drifts == nil {
		snap.Drifts = []PricingDrift{}
	} else {
		snap.Drifts = append([]PricingDrift(nil), snap.Drifts...)
	}
	onSnap := c.onSnap
	c.mu.Unlock()
	if onSnap != nil {
		onSnap(snap)
	}
}

func (c *PricingChecker) fetchAndCompare(ctx context.Context) ([]PricingDrift, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("catalog GET %s: status %d", c.url, resp.StatusCode)
	}
	limited := io.LimitReader(resp.Body, c.maxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > c.maxBytes {
		return nil, fmt.Errorf("catalog body exceeds %d bytes", c.maxBytes)
	}
	return compareCatalogJSON(body)
}

type rawCatalog map[string]json.RawMessage

func compareCatalogJSON(body []byte) ([]PricingDrift, error) {
	var raw rawCatalog
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("catalog json: %w", err)
	}
	picked := pickCatalogPrices(raw)
	raw = nil
	return driftAgainstTable(picked), nil
}

func pickCatalogPrices(raw rawCatalog) map[string]catalogPrice {
	out := make(map[string]catalogPrice, len(catalogKeys))
	for model, aliases := range catalogKeys {
		for _, alias := range aliases {
			blob, ok := raw[alias]
			if !ok {
				continue
			}
			var p catalogPrice
			if err := json.Unmarshal(blob, &p); err != nil {
				continue
			}
			if p.InputCostPerToken <= 0 || p.OutputCostPerToken <= 0 {
				continue
			}
			out[model] = p
			break
		}
	}
	return out
}

func driftAgainstTable(catalog map[string]catalogPrice) []PricingDrift {
	out := make([]PricingDrift, 0)
	for model, ours := range pricingTable {
		if strings.HasPrefix(model, "grid/") {
			continue
		}
		p, ok := catalog[model]
		if !ok {
			continue
		}
		catIn := p.InputCostPerToken * 1e6
		catOut := p.OutputCostPerToken * 1e6
		if !priceDrifted(ours.inPerM, catIn) && !priceDrifted(ours.outPerM, catOut) {
			continue
		}
		out = append(out, PricingDrift{
			Model:      model,
			OursIn:     ours.inPerM,
			OursOut:    ours.outPerM,
			CatalogIn:  catIn,
			CatalogOut: catOut,
		})
	}
	return out
}

func priceDrifted(ours, catalog float64) bool {
	delta := math.Abs(ours - catalog)
	if delta <= pricingDriftAbsFloor {
		return false
	}
	denom := math.Max(math.Abs(ours), math.Abs(catalog))
	if denom == 0 {
		return delta > 0
	}
	return delta/denom > pricingDriftRelTol
}
