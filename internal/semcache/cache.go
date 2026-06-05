package semcache

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Hit is a cache lookup result.
type Hit struct {
	ResponseJSON []byte  // marshaled ChatCompletionResponse
	Similarity   float64 // cosine similarity of the matched entry
}

// Cache stores and retrieves completions by semantic similarity. The scope
// namespaces entries per tenant (org/virtual key) so cached responses are never
// shared across tenants.
type Cache interface {
	Lookup(ctx context.Context, scope, model, prompt string, vec []float32) (*Hit, error)
	Store(ctx context.Context, scope, model, prompt string, vec []float32, responseJSON []byte) error
}

type storedEntry struct {
	Embedding []float32 `json:"e"`
	Response  []byte    `json:"r"`
	ExpiresAt int64     `json:"x,omitempty"`
}

func redisKey(scope, model string) string { return "nexus:sem:" + scope + ":" + model }

func effectiveConfig(cfg Config) Config {
	if cfg.TTL <= 0 {
		cfg.TTL = 24 * time.Hour
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = 0.92
	}
	if cfg.MaxEntriesPerModel <= 0 {
		cfg.MaxEntriesPerModel = 500
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	return cfg
}

// findBest scans entries for the highest cosine similarity above threshold.
func findBest(vec []float32, entries []storedEntry, threshold float64, now int64) (*Hit, float64) {
	var bestSim float64
	var bestResp []byte
	for _, e := range entries {
		if e.ExpiresAt > 0 && e.ExpiresAt < now {
			continue
		}
		sim := cosineSimilarity(vec, e.Embedding)
		if sim >= threshold && sim > bestSim {
			bestSim = sim
			bestResp = e.Response
		}
	}
	if bestResp == nil {
		return nil, 0
	}
	return &Hit{ResponseJSON: bestResp, Similarity: bestSim}, bestSim
}

// Memory is an in-process cache for tests and single-node dev.
type Memory struct {
	mu   sync.RWMutex
	cfg  Config
	data map[string][]storedEntry // model -> entries
}

// NewMemory creates an in-memory semantic cache.
func NewMemory(cfg Config) *Memory {
	return &Memory{cfg: effectiveConfig(cfg), data: make(map[string][]storedEntry)}
}

func (m *Memory) Lookup(_ context.Context, scope, model, _ string, vec []float32) (*Hit, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	now := time.Now().Unix()
	hit, _ := findBest(vec, m.data[redisKey(scope, model)], m.cfg.Threshold, now)
	return hit, nil
}

func (m *Memory) Store(_ context.Context, scope, model, _ string, vec []float32, responseJSON []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := redisKey(scope, model)
	e := storedEntry{
		Embedding: vec,
		Response:  append([]byte(nil), responseJSON...),
		ExpiresAt: time.Now().Add(m.cfg.TTL).Unix(),
	}
	m.data[key] = append([]storedEntry{e}, m.data[key]...)
	if len(m.data[key]) > m.cfg.MaxEntriesPerModel {
		m.data[key] = m.data[key][:m.cfg.MaxEntriesPerModel]
	}
	return nil
}

// Redis stores semantic cache entries in Redis lists (one list per model).
type Redis struct {
	rdb      *redis.Client
	cfg      Config
	embedder Embedder
}

// NewRedis connects to Redis and returns a semantic cache. embedder may be nil
// when callers supply pre-computed vectors to Lookup/Store.
func NewRedis(ctx context.Context, url string, embedder Embedder, cfg Config) (*Redis, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	rdb := redis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, err
	}
	return &Redis{rdb: rdb, cfg: effectiveConfig(cfg), embedder: embedder}, nil
}

// Close releases the Redis client.
func (c *Redis) Close() error { return c.rdb.Close() }

// Embedder returns the configured embedder (may be nil).
func (c *Redis) Embedder() Embedder { return c.embedder }

func (c *Redis) Lookup(ctx context.Context, scope, model, _ string, vec []float32) (*Hit, error) {
	raws, err := c.rdb.LRange(ctx, redisKey(scope, model), 0, int64(c.cfg.MaxEntriesPerModel-1)).Result()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	var entries []storedEntry
	for _, raw := range raws {
		var e storedEntry
		if json.Unmarshal([]byte(raw), &e) == nil {
			entries = append(entries, e)
		}
	}
	hit, _ := findBest(vec, entries, c.cfg.Threshold, now)
	return hit, nil
}

func (c *Redis) Store(ctx context.Context, scope, model, _ string, vec []float32, responseJSON []byte) error {
	e := storedEntry{
		Embedding: vec,
		Response:  responseJSON,
		ExpiresAt: time.Now().Add(c.cfg.TTL).Unix(),
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	key := redisKey(scope, model)
	pipe := c.rdb.TxPipeline()
	pipe.LPush(ctx, key, raw)
	pipe.LTrim(ctx, key, 0, int64(c.cfg.MaxEntriesPerModel-1))
	_, err = pipe.Exec(ctx)
	return err
}

// Service wraps a Cache with an Embedder for convenience on the hot path.
type Service struct {
	cache    Cache
	embedder Embedder
	cfg      Config
}

// NewService builds a semantic cache service. Returns nil when disabled or when
// embedder is nil.
func NewService(cache Cache, embedder Embedder, cfg Config) *Service {
	if !cfg.Enabled || embedder == nil || cache == nil {
		return nil
	}
	return &Service{cache: cache, embedder: embedder, cfg: effectiveConfig(cfg)}
}

// Enabled reports whether the service is active.
func (s *Service) Enabled() bool { return s != nil }

// Lookup embeds the prompt and searches the cache. The returned vector can be
// passed to Store on a miss to avoid a second embedding call. The embedding is
// bounded by the configured timeout so a slow/unhealthy embeddings endpoint can
// never stall the request hot path beyond that budget — the caller degrades to a
// normal upstream call on error.
func (s *Service) Lookup(ctx context.Context, scope, model, prompt string) (*Hit, []float32, error) {
	ectx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	vec, err := s.embedder.Embed(ectx, prompt)
	if err != nil {
		return nil, nil, err
	}
	hit, err := s.cache.Lookup(ctx, scope, model, prompt, vec)
	return hit, vec, err
}

// Store saves a response using a pre-computed embedding vector.
func (s *Service) Store(ctx context.Context, scope, model, prompt string, vec []float32, responseJSON []byte) error {
	if len(vec) == 0 {
		var err error
		vec, err = s.embedder.Embed(ctx, prompt)
		if err != nil {
			return err
		}
	}
	return s.cache.Store(ctx, scope, model, prompt, vec, responseJSON)
}

// Threshold returns the configured similarity threshold.
func (s *Service) Threshold() float64 { return s.cfg.Threshold }

// ConfigString returns a short description for startup logs.
func (s *Service) ConfigString() string {
	if s == nil {
		return ""
	}
	return fmt.Sprintf("threshold=%.2f ttl=%s max=%d", s.cfg.Threshold, s.cfg.TTL, s.cfg.MaxEntriesPerModel)
}
