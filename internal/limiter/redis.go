package limiter

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis is a Limiter backed by Redis, shared across all gateway replicas.
type Redis struct {
	rdb *redis.Client
	now func() time.Time
}

// NewRedis connects to Redis using a URL (redis://host:port/db) and returns a
// Limiter. It pings to verify connectivity.
func NewRedis(ctx context.Context, url string) (*Redis, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	rdb := redis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, err
	}
	return &Redis{rdb: rdb, now: time.Now}, nil
}

// Close releases the client.
func (r *Redis) Close() error { return r.rdb.Close() }

// Allow implements Limiter using a fixed per-minute window counter.
func (r *Redis) Allow(ctx context.Context, keyID string, rpmLimit int) (bool, error) {
	if rpmLimit <= 0 {
		return true, nil
	}
	k := rpmKey(keyID, minuteWindow(r.now()))
	pipe := r.rdb.TxPipeline()
	incr := pipe.Incr(ctx, k)
	pipe.Expire(ctx, k, 2*time.Minute)
	if _, err := pipe.Exec(ctx); err != nil {
		return false, err
	}
	return incr.Val() <= int64(rpmLimit), nil
}

// MonthlySpend implements Limiter.
func (r *Redis) MonthlySpend(ctx context.Context, keyID string) (float64, error) {
	v, err := r.rdb.Get(ctx, spendKey(keyID, monthWindow(r.now()))).Float64()
	if err == redis.Nil {
		return 0, nil
	}
	return v, err
}

// AddSpend implements Limiter.
func (r *Redis) AddSpend(ctx context.Context, keyID string, costUSD float64) error {
	if costUSD <= 0 {
		return nil
	}
	k := spendKey(keyID, monthWindow(r.now()))
	pipe := r.rdb.TxPipeline()
	pipe.IncrByFloat(ctx, k, costUSD)
	// Keep month buckets ~2 months then let them expire.
	pipe.Expire(ctx, k, 62*24*time.Hour)
	_, err := pipe.Exec(ctx)
	return err
}
