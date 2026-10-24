package cache_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emoss08/keel/analytics/cache"
)

// fakeRedis is an in-memory RedisClient that records expiries.
type fakeRedis struct {
	mu   sync.Mutex
	data map[string][]byte
	ttls map[string]time.Duration
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{data: map[string][]byte{}, ttls: map[string]time.Duration{}}
}

func (f *fakeRedis) Get(_ context.Context, key string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.data[key]
	return v, ok, nil
}

func (f *fakeRedis) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[key], f.ttls[key] = value, ttl
	return nil
}

func (f *fakeRedis) Del(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, key)
	return nil
}

func TestRedis(t *testing.T) {
	ctx := context.Background()
	f := newFakeRedis()
	r := cache.NewRedis(f, cache.RedisOptions{MaxValueBytes: 8})
	if err := r.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if f.ttls["keel:analytics:k"] != time.Minute {
		t.Fatalf("keys = %v", f.ttls)
	}
	v, ok, err := r.Get(ctx, "k")
	if err != nil || !ok || string(v) != "v" {
		t.Fatalf("Get = %q %v %v", v, ok, err)
	}
	if err := r.Set(ctx, "k", []byte("123456789"), time.Minute); !errors.Is(err, cache.ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if _, ok, _ := r.Get(ctx, "k"); ok {
		t.Fatal("the previous value survived a rejected Set")
	}
	long := strings.Repeat("x", 300)
	_ = r.Set(ctx, long, []byte("l"), -time.Second)
	for k, ttl := range f.ttls {
		if strings.Contains(k, "sha256:") && ttl != 0 {
			t.Fatalf("a negative TTL became %v", ttl)
		}
		if len(k) > 300 {
			t.Fatalf("a long key was stored as is: %d bytes", len(k))
		}
	}
	if v, ok, _ := r.Get(ctx, long); !ok || string(v) != "l" {
		t.Fatal("a hashed key was not found again")
	}
	if err := r.Delete(ctx, long); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := r.Get(ctx, long); ok {
		t.Fatal("Delete left the value")
	}
}
