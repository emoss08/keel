package cache

import (
	"context"
	"fmt"
	"time"
)

// DefaultRedisPrefix starts every key the Redis store writes.
const DefaultRedisPrefix = "keel:analytics:"

// RedisClient is the part of a Redis client the Redis store needs, so keel depends on no client
// library. With go-redis it is a few lines:
//
//	type redisClient struct{ c *redis.Client }
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
type RedisClient interface {
	// Get returns the value of key and true, or false when the key does not exist.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Set writes value under key with an expiry of ttl; zero means none.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// Del removes key; a missing key is not an error.
	Del(ctx context.Context, key string) error
}

// RedisOptions configure a Redis store.
type RedisOptions struct {
	// Prefix starts every key; default DefaultRedisPrefix.
	Prefix string
	// MaxValueBytes caps a value; a larger one is ErrTooLarge. Default DefaultMaxValueBytes.
	MaxValueBytes int
}

// Redis is a Store on a Redis-compatible server (Redis, Valkey, KeyDB, Dragonfly), reached through
// the application's own client. Keys are prefixed, and keys longer than 200 bytes are hashed.
type Redis struct {
	client RedisClient
	prefix string
	max    int
}

// NewRedis returns a Redis store over client.
func NewRedis(client RedisClient, o RedisOptions) *Redis {
	if o.Prefix == "" {
		o.Prefix = DefaultRedisPrefix
	}
	if o.MaxValueBytes <= 0 {
		o.MaxValueBytes = DefaultMaxValueBytes
	}
	return &Redis{client: client, prefix: o.Prefix, max: o.MaxValueBytes}
}

// Delete removes the value under key.
func (r *Redis) Delete(ctx context.Context, key string) error {
	if err := r.client.Del(ctx, r.prefix+storedKey(key)); err != nil {
		return fmt.Errorf("cache: redis del: %w", err)
	}
	return nil
}

// Get returns the value under key.
func (r *Redis) Get(ctx context.Context, key string) ([]byte, bool, error) {
	v, ok, err := r.client.Get(ctx, r.prefix+storedKey(key))
	if err != nil {
		return nil, false, fmt.Errorf("cache: redis get: %w", err)
	}
	return v, ok, nil
}

// Set stores value under key for ttl. A value over MaxValueBytes is ErrTooLarge, and the key's
// previous value is deleted, so it is not served.
func (r *Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	k := r.prefix + storedKey(key)
	if len(value) > r.max {
		if err := r.client.Del(ctx, k); err != nil {
			return fmt.Errorf("cache: redis del: %w", err)
		}
		return fmt.Errorf("%w: %d bytes, over %d", ErrTooLarge, len(value), r.max)
	}
	if ttl < 0 {
		ttl = 0
	}
	if err := r.client.Set(ctx, k, value, ttl); err != nil {
		return fmt.Errorf("cache: redis set: %w", err)
	}
	return nil
}
