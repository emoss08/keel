// Package cache holds result caches for keel/analytics: byte values by key, each with a time to
// live. Memory is a size-bounded otter cache for one process; Postgres keeps values in an unlogged
// table every process shares; Redis adapts any Redis client through a three-method interface.
// Loader puts one of them in front of a computation, so concurrent misses of a key in a process
// compute it once.
//
// A key must identify everything the value depends on: the statement, its arguments (the tenant
// among them), the row rules, the output format and the data versions of the tables it read.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// Store is a cache of byte values by key.
type Store interface {
	// Get returns the value stored under key and true, or false when there is none or it expired.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Set stores value under key for ttl; a ttl of zero or less keeps it until it is evicted.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// ErrTooLarge is returned by Set for a value larger than the store accepts. The key's previous
// value is removed, so a reader never sees a stale one.
var ErrTooLarge = errors.New("cache: value too large")

// maxKeyBytes is the longest key the shared stores keep as it is; longer keys are hashed.
const maxKeyBytes = 200

// storedKey returns key as a shared store keeps it: itself, or a SHA-256 digest when it is longer
// than maxKeyBytes, so index entries stay small.
func storedKey(key string) string {
	if len(key) <= maxKeyBytes {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])
}
