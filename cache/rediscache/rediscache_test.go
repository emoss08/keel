package rediscache_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel/cache/rediscache"
)

// fakeRedis is an in-memory Client that records the keys it is given.
type fakeRedis struct {
	m    map[string][]byte
	ttls map[string]time.Duration
}

func (f *fakeRedis) Get(_ context.Context, key string) ([]byte, bool, error) {
	v, ok := f.m[key]
	return v, ok, nil
}

func (f *fakeRedis) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	f.m[key], f.ttls[key] = value, ttl
	return nil
}

func (f *fakeRedis) Del(_ context.Context, key string) error {
	delete(f.m, key)
	return nil
}

func TestStorePrefixesKeysAndRoundTrips(t *testing.T) {
	ctx := context.Background()
	f := &fakeRedis{m: map[string][]byte{}, ttls: map[string]time.Duration{}}
	for _, tc := range []struct{ prefix, want string }{{"", rediscache.DefaultPrefix}, {"app:", "app:"}} {
		s := rediscache.New(f, tc.prefix)
		if err := s.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
			t.Fatal(err)
		}
		if f.ttls[tc.want+"k"] != time.Minute {
			t.Fatalf("keys %v, want %sk", f.m, tc.want)
		}
		v, ok, err := s.Get(ctx, "k")
		if err != nil || !ok || string(v) != "v" {
			t.Fatalf("Get = %q, %v, %v", v, ok, err)
		}
	}
	s := rediscache.New(f, "")
	if err := s.Set(ctx, "big", []byte(strings.Repeat("x", rediscache.MaxValueBytes+1)), time.Minute); err == nil {
		t.Fatal("an entry over the limit was stored")
	}
	if _, ok, _ := s.Get(ctx, "big"); ok {
		t.Fatal("an entry over the limit is served")
	}
}
