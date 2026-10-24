package cache_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/emoss08/keel/analytics/cache"
)

// benchKeys are keys shaped like a report cache's: a statement hash, a tenant and versions.
var benchKeys = func() []string {
	out := make([]string, 4096)
	for i := range out {
		out[i] = "report:9f2c41d07a:org_" + strconv.Itoa(i%97) + ":v" + strconv.Itoa(i)
	}
	return out
}()

func BenchmarkMemoryGetParallel(b *testing.B) {
	ctx := context.Background()
	m := cache.NewMemory(cache.MemoryOptions{})
	val := make([]byte, 2048)
	for _, k := range benchKeys {
		_ = m.Set(ctx, k, val, time.Hour)
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_, _, _ = m.Get(ctx, benchKeys[i&4095])
			i++
		}
	})
}

func BenchmarkMemorySetParallel(b *testing.B) {
	ctx := context.Background()
	m := cache.NewMemory(cache.MemoryOptions{MaxBytes: 4 << 20})
	val := make([]byte, 2048)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_ = m.Set(ctx, benchKeys[i&4095], val, time.Hour)
			i++
		}
	})
}

func BenchmarkLoaderHitParallel(b *testing.B) {
	ctx := context.Background()
	m := cache.NewMemory(cache.MemoryOptions{})
	l := cache.NewLoader(m, cache.LoaderOptions{})
	val := make([]byte, 2048)
	for _, k := range benchKeys {
		_ = m.Set(ctx, k, val, time.Hour)
	}
	load := func(context.Context) ([]byte, error) { return val, nil }
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_, _, _ = l.Load(ctx, benchKeys[i&4095], time.Hour, load)
			i++
		}
	})
}

func BenchmarkPostgresGet(b *testing.B) {
	p := pool(b)
	ctx := context.Background()
	s, err := cache.NewPostgres(p, cache.PostgresOptions{Table: tableName(b, p)})
	if err != nil {
		b.Fatal(err)
	}
	val := make([]byte, 2048)
	for _, k := range benchKeys[:256] {
		if err := s.Set(ctx, k, val, time.Hour); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if _, ok, err := s.Get(ctx, benchKeys[i&255]); !ok || err != nil {
			b.Fatal(ok, err)
		}
	}
}
