package cache_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/emoss08/keel/analytics/cache"
)

// pool connects to the test server, skipping when it is unreachable unless KEEL_TEST_REQUIRE is set.
func pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://keel:keel@localhost:55432/keel?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := pgxpool.New(ctx, dsn)
	if err == nil {
		err = p.Ping(ctx)
	}
	if err != nil {
		if os.Getenv("KEEL_TEST_REQUIRE") != "" {
			t.Fatalf("postgres: %v", err)
		}
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// tableName returns a fresh table name and drops the table when the test ends.
func tableName(t testing.TB, p *pgxpool.Pool) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "keel_cache_test_" + hex.EncodeToString(b)
	t.Cleanup(func() { _, _ = p.Exec(context.Background(), "DROP TABLE IF EXISTS "+name) })
	return name
}

func TestPostgresCreatesTableOnceUnderConcurrency(t *testing.T) {
	p := pool(t)
	name := tableName(t, p)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := cache.NewPostgres(p, cache.PostgresOptions{Table: name})
			if err == nil {
				err = s.Set(ctx, "k", []byte("v"), time.Minute)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var persistence string
	if err := p.QueryRow(ctx, "SELECT relpersistence::text FROM pg_class WHERE relname = $1", name).Scan(&persistence); err != nil {
		t.Fatal(err)
	}
	if persistence != "u" {
		t.Fatalf("relpersistence = %q, want unlogged", persistence)
	}
}

func TestPostgresGetSetExpiry(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	s, err := cache.NewPostgres(p, cache.PostgresOptions{Table: tableName(t, p), MaxValueBytes: 16, CleanupEvery: -1})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Get(ctx, "k"); ok || err != nil {
		t.Fatalf("empty Get = %v %v", ok, err)
	}
	if err := s.Set(ctx, "k", []byte("v1"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "k", []byte("v2"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.Get(ctx, "k"); !ok || err != nil || string(v) != "v2" {
		t.Fatalf("Get = %q %v %v", v, ok, err)
	}
	if err := s.Set(ctx, "short", []byte("x"), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "forever", []byte("y"), 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, ok, _ := s.Get(ctx, "short"); ok {
		t.Fatal("served after its TTL")
	}
	if _, ok, _ := s.Get(ctx, "forever"); !ok {
		t.Fatal("an entry without a TTL is gone")
	}
	if err := s.Set(ctx, "k", make([]byte, 17), time.Minute); !errors.Is(err, cache.ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if _, ok, _ := s.Get(ctx, "k"); ok {
		t.Fatal("the previous value survived a rejected Set")
	}
	long := strings.Repeat("k", 1000)
	if err := s.Set(ctx, long, []byte("l"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := s.Get(ctx, long); !ok || string(v) != "l" {
		t.Fatal("a long key was not found again")
	}
	if err := s.Delete(ctx, long); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(ctx, long); ok {
		t.Fatal("Delete left the value")
	}
}

func TestPostgresCleanup(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	name := tableName(t, p)
	s, err := cache.NewPostgres(p, cache.PostgresOptions{Table: name, CleanupBatch: 7, CleanupEvery: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "keep", []byte("v"), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "INSERT INTO "+name+" SELECT 'x' || g, 'v', now() - interval '1 second' FROM generate_series(1, 50) g"); err != nil {
		t.Fatal(err)
	}
	n, err := s.Cleanup(ctx)
	if err != nil || n != 50 {
		t.Fatalf("Cleanup = %d %v, want 50", n, err)
	}
	var left int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM "+name).Scan(&left); err != nil || left != 1 {
		t.Fatalf("left %d rows (%v)", left, err)
	}
}

func TestPostgresBackgroundCleanup(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	name := tableName(t, p)
	s, err := cache.NewPostgres(p, cache.PostgresOptions{Table: name, CleanupEvery: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "a", []byte("v"), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := s.Set(ctx, "b", []byte("v"), time.Hour); err != nil {
		t.Fatal(err)
	}
	s.Close()
	var left int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM "+name+" WHERE key = 'a'").Scan(&left); err != nil || left != 0 {
		t.Fatalf("the expired row is still there: %d (%v)", left, err)
	}
}

func TestPostgresRejectsBadTable(t *testing.T) {
	if _, err := cache.NewPostgres(nil, cache.PostgresOptions{Table: "bad; drop"}); err == nil {
		t.Fatal("a table name with SQL in it was accepted")
	}
}
