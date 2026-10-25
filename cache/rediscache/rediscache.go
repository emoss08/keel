// Package rediscache puts keel's result cache (keel.CacheConfig.Shared) on a Redis-compatible
// server (Redis, Valkey, KeyDB, Dragonfly), reached through the application's own client, so keel
// depends on no client library. The client is the three-method interface keel/analytics/cache
// uses, so one adapter serves both:
//
//	type redisClient struct{ c redis.UniversalClient }
//
//	func (r redisClient) Get(ctx context.Context, key string) ([]byte, bool, error) {
//	    v, err := r.c.Get(ctx, key).Bytes()
//	    if errors.Is(err, redis.Nil) {
//	        return nil, false, nil
//	    }
//	    return v, err == nil, err
//	}
//
//	func (r redisClient) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
//	    return r.c.Set(ctx, key, value, ttl).Err()
//	}
//
//	func (r redisClient) Del(ctx context.Context, key string) error {
//	    return r.c.Del(ctx, key).Err()
//	}
//
//	cfg.Cache = &keel.CacheConfig{Shared: rediscache.New(redisClient{rdb}, "")}
package rediscache

import (
	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/cache"
)

// DefaultPrefix starts every key the store writes when New is given no prefix.
const DefaultPrefix = "keel:cache:"

// MaxValueBytes caps an entry the store writes, matching keel's default
// CacheConfig.MaxEntryBytes; a larger one is refused and the key's previous value deleted.
const MaxValueBytes = 1 << 20

// Client is the part of a Redis client the store needs: Get, Set with a time to live, and Del.
type Client = cache.RedisClient

// New returns a keel.SharedCache that keeps entries in Redis through client, under keys that
// start with prefix (DefaultPrefix when empty). Entry keys are SHA-256 digests, so they are short.
func New(client Client, prefix string) keel.SharedCache {
	if prefix == "" {
		prefix = DefaultPrefix
	}
	return cache.NewRedis(client, cache.RedisOptions{Prefix: prefix, MaxValueBytes: MaxValueBytes})
}
