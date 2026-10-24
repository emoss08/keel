package cache_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emoss08/keel/analytics/cache"
)

// clock is a settable clock for expiry tests.
type clock struct{ now atomic.Int64 }

func (c *clock) Now() time.Time          { return time.Unix(0, c.now.Load()) }
func (c *clock) Advance(d time.Duration) { c.now.Add(int64(d)) }

func TestMemoryGetSet(t *testing.T) {
	ctx := context.Background()
	m := cache.NewMemory(cache.MemoryOptions{})
	if _, ok, _ := m.Get(ctx, "a"); ok {
		t.Fatal("an empty store had a value")
	}
	buf := []byte("one")
	if err := m.Set(ctx, "a", buf, time.Minute); err != nil {
		t.Fatal(err)
	}
	buf[0] = 'X'
	v, ok, err := m.Get(ctx, "a")
	if err != nil || !ok || string(v) != "one" {
		t.Fatalf("Get = %q %v %v; Set must copy the value", v, ok, err)
	}
	if err := m.Set(ctx, "a", []byte("two"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := m.Get(ctx, "a"); string(v) != "two" {
		t.Fatalf("overwritten value = %q", v)
	}
	if err := m.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := m.Get(ctx, "a"); ok {
		t.Fatal("a deleted value was returned")
	}
	st := m.Stats()
	if st.Entries != 0 || st.Bytes != 0 || st.Hits != 2 || st.Misses != 2 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestMemoryExpiry(t *testing.T) {
	ctx := context.Background()
	c := &clock{}
	c.now.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	m := cache.NewMemory(cache.MemoryOptions{Now: c.Now})
	_ = m.Set(ctx, "short", []byte("x"), time.Second)
	_ = m.Set(ctx, "forever", []byte("y"), 0)
	c.Advance(999 * time.Millisecond)
	if _, ok, _ := m.Get(ctx, "short"); !ok {
		t.Fatal("expired before its TTL")
	}
	c.Advance(time.Millisecond)
	if _, ok, _ := m.Get(ctx, "short"); ok {
		t.Fatal("served after its TTL")
	}
	c.Advance(1000 * time.Hour)
	if _, ok, _ := m.Get(ctx, "forever"); !ok {
		t.Fatal("an entry without a TTL expired")
	}
	if st := m.Stats(); st.Entries != 1 {
		t.Fatalf("the expired entry was not removed: %+v", st)
	}
}

func TestMemoryEvictsLeastRecentlyUsed(t *testing.T) {
	ctx := context.Background()
	m := cache.NewMemory(cache.MemoryOptions{Shards: 1, MaxBytes: 3 * (96 + 1 + 10)})
	val := []byte("0123456789")
	for _, k := range []string{"a", "b", "c"} {
		if err := m.Set(ctx, k, val, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, _ := m.Get(ctx, "a"); !ok {
		t.Fatal("a was evicted before the store was full")
	}
	_ = m.Set(ctx, "d", val, 0)
	if _, ok, _ := m.Get(ctx, "b"); ok {
		t.Fatal("b, the least recently used, survived")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok, _ := m.Get(ctx, k); !ok {
			t.Fatalf("%s was evicted", k)
		}
	}
	st := m.Stats()
	if st.Evictions != 1 || st.Entries != 3 || st.Bytes != 3*107 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestMemoryTooLarge(t *testing.T) {
	ctx := context.Background()
	m := cache.NewMemory(cache.MemoryOptions{Shards: 1, MaxBytes: 200})
	_ = m.Set(ctx, "k", []byte("small"), 0)
	err := m.Set(ctx, "k", make([]byte, 500), 0)
	if !errors.Is(err, cache.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if _, ok, _ := m.Get(ctx, "k"); ok {
		t.Fatal("the previous value survived a rejected Set")
	}
}

func TestMemoryShardsRoundUp(t *testing.T) {
	ctx := context.Background()
	m := cache.NewMemory(cache.MemoryOptions{Shards: 5, MaxBytes: 8 << 20})
	for i := range 1000 {
		_ = m.Set(ctx, strconv.Itoa(i), []byte("v"), 0)
	}
	if st := m.Stats(); st.Entries != 1000 {
		t.Fatalf("entries = %d", st.Entries)
	}
}

func TestMemoryConcurrent(t *testing.T) {
	ctx := context.Background()
	m := cache.NewMemory(cache.MemoryOptions{MaxBytes: 1 << 16})
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 2000 {
				k := fmt.Sprintf("k%d", (g*7+i)%300)
				if i%3 == 0 {
					_ = m.Set(ctx, k, []byte(k), time.Minute)
				} else if v, ok, _ := m.Get(ctx, k); ok && string(v) != k {
					t.Errorf("Get(%s) = %s", k, v)
				}
			}
		}()
	}
	wg.Wait()
	if st := m.Stats(); st.Bytes > 1<<16 {
		t.Fatalf("held %d bytes over the bound", st.Bytes)
	}
}
