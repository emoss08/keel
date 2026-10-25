package keel_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/cache"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// cacheDB opens a DB on the test database with data versions and the result cache on. Versions
// are written only when flushed, so a test decides when other processes see a write.
func cacheDB(t *testing.T, rec *recorder, edit func(cfg *keel.Config)) *keel.DB {
	t.Helper()
	db(t)
	cfg := testConfig(testDSN)
	cfg.DataVersions = keel.DataVersionsConfig{Enabled: true, Interval: time.Hour}
	cfg.Cache = &keel.CacheConfig{Local: keel.LRU(8 << 20)}
	if rec != nil {
		cfg.Observers = []keel.Observer{rec}
	}
	if edit != nil {
		edit(&cfg)
	}
	d, err := keel.Open(context.Background(), cfg)
	ok(t, err)
	t.Cleanup(d.Close)
	waitFor(t, "the cache listener", func() bool { return d.CacheStats().Listening })
	return d
}

// cachedCustomers lists the tenant's customers by code through the cache.
func cachedCustomers(t *testing.T, ctx context.Context, d *keel.DB) []*fleet.Customer {
	t.Helper()
	c := fleetdb.Customers
	return must(c.Order(c.Code.Asc()).Cache(keel.TTL(time.Minute)).All(ctx, d))(t)
}

// codes returns the customers' codes.
func codes(rows []*fleet.Customer) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Code
	}
	return out
}

func TestCacheHitSendsNothingAndDecodesFreshRows(t *testing.T) {
	rec := &recorder{}
	d := cacheDB(t, rec, nil)
	w := seed(t, 8)
	s := fleetdb.Shipments
	m := s.Moves()
	query := func() *keel.Query[*fleet.Shipment] {
		return s.Where(s.ProNumber.In("PRO-0000", "PRO-0001")).
			Load(s.Customer(), m.Order(m.Sequence.Asc()), m.Stops(), s.Tags()).
			Order(s.ProNumber.Asc()).
			Cache(keel.TTL(time.Minute))
	}

	first := must(query().All(w.ctx, d))(t)
	st := d.CacheStats()
	eq(t, st.Misses, int64(1))
	eq(t, st.Hits, int64(0))

	rec.reset()
	second := must(query().All(w.ctx, d))(t)
	eq(t, rec.statements(), 0)
	eq(t, d.CacheStats().Hits, int64(1))

	eq(t, len(second), len(first))
	for i := range first {
		a, b := first[i], second[i]
		if a == b {
			t.Fatal("a hit returned the rows of the miss")
		}
		eq(t, b.ProNumber, a.ProNumber)
		eq(t, b.Customer.Code, a.Customer.Code)
		eq(t, len(b.Moves), len(a.Moves))
		eq(t, len(b.Tags), len(a.Tags))
		for j := range a.Moves {
			if a.Moves[j] == b.Moves[j] {
				t.Fatal("a hit shares a loaded row with the miss")
			}
			eq(t, b.Moves[j].Sequence, a.Moves[j].Sequence)
			eq(t, len(b.Moves[j].Stops), len(a.Moves[j].Stops))
		}
		eq(t, b.Attributes["index"] != nil, true)
	}
	second[0].ProNumber = "changed"
	second[0].Labels[0] = "changed"
	third := must(query().All(w.ctx, d))(t)
	eq(t, third[0].ProNumber, "PRO-0000")
	eq(t, third[0].Labels[0], first[0].Labels[0])
}

func TestCacheServesEveryReadTerminal(t *testing.T) {
	rec := &recorder{}
	d := cacheDB(t, rec, nil)
	w := seed(t, 8)
	s := fleetdb.Shipments
	ttl := keel.TTL(time.Minute)
	twice := func(name string, read func() (any, error)) {
		t.Helper()
		want, err := read()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		hits := d.CacheStats().Hits
		rec.reset()
		got, err := read()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n := rec.statements(); n != 0 {
			t.Fatalf("%s: a hit sent %d statements", name, n)
		}
		if d.CacheStats().Hits != hits+1 {
			t.Fatalf("%s: not served from the cache", name)
		}
		if gs, ws := describe(got), describe(want); gs != ws {
			t.Fatalf("%s: hit %s, miss %s", name, gs, ws)
		}
	}
	id := w.shipments[0].ID
	twice("One", func() (any, error) {
		r, err := s.Where(s.ID.Eq(id)).Cache(ttl).One(w.ctx, d)
		return proOf(r), err
	})
	twice("First", func() (any, error) {
		r, err := s.Order(s.ProNumber.Desc()).Cache(ttl).First(w.ctx, d)
		return proOf(r), err
	})
	twice("List", func() (any, error) {
		rows, err := s.Order(s.ProNumber.Asc()).Limit(3).Cache(ttl).List(w.ctx, d)
		return pros(rows), err
	})
	twice("Count", func() (any, error) {
		return s.Where(s.Hazmat.Eq(false)).Cache(ttl).Count(w.ctx, d)
	})
	twice("CountUpTo", func() (any, error) {
		return s.Cache(ttl).CountUpTo(w.ctx, d, 5)
	})
	twice("CountEstimate", func() (any, error) {
		return s.Cache(ttl).CountEstimate(w.ctx, d, 3)
	})
	twice("Exists", func() (any, error) {
		return s.Where(s.ProNumber.Eq("PRO-0003")).Cache(ttl).Exists(w.ctx, d)
	})
	twice("Page", func() (any, error) {
		p, err := s.Order(s.ProNumber.Asc()).Cache(ttl).Page(w.ctx, d, keel.After("", 3).Total(100))
		return []any{pros(p.Items), p.Next, p.Total, p.TotalExact}, err
	})
	twice("Batch", func() (any, error) {
		var rows []*fleet.Shipment
		var n int64
		err := keel.Batch(w.ctx, d,
			keel.ListInto(s.Order(s.ProNumber.Asc()).Limit(2).Cache(ttl), &rows),
			keel.CountInto(s.Where(s.Hazmat.Eq(true)).Cache(keel.TTL(time.Second)), &n))
		return []any{pros(rows), n}, err
	})
}

// proOf returns a shipment's PRO number, or "" for nil.
func proOf(s *fleet.Shipment) string {
	if s == nil {
		return ""
	}
	return s.ProNumber
}

// describe formats a value for comparison.
func describe(v any) string { return fmt.Sprintf("%v", v) }

func TestCacheSeesThisProcessesWritesAtOnce(t *testing.T) {
	d := cacheDB(t, nil, nil)
	ctx := newTenant()
	c := fleetdb.Customers
	ok(t, c.Create(ctx, d, &fleet.Customer{Name: "A", Code: "A"}))

	eq(t, len(cachedCustomers(t, ctx, d)), 1)
	eq(t, len(cachedCustomers(t, ctx, d)), 1)
	eq(t, d.CacheStats().Hits, int64(1))

	ok(t, c.Create(ctx, d, &fleet.Customer{Name: "B", Code: "B"}))
	eq(t, len(cachedCustomers(t, ctx, d)), 2)
	hits := d.CacheStats().Hits
	eq(t, len(cachedCustomers(t, ctx, d)), 2)
	eq(t, d.CacheStats().Hits, hits+1)

	_ = must(c.Where(c.Code.Eq("A")).Update(ctx, d, c.Name.To("A, renamed")))(t)
	eq(t, cachedCustomers(t, ctx, d)[0].Name, "A, renamed")

	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		return c.Create(ctx, d, &fleet.Customer{Name: "C", Code: "C"})
	}))
	eq(t, len(cachedCustomers(t, ctx, d)), 3)

	ok(t, d.FlushDataVersions(context.Background()))
	eq(t, len(cachedCustomers(t, ctx, d)), 3)
	hits = d.CacheStats().Hits
	eq(t, len(cachedCustomers(t, ctx, d)), 3)
	eq(t, d.CacheStats().Hits, hits+1)
}

func TestCacheInvalidatedByAnotherProcessesNotification(t *testing.T) {
	a := cacheDB(t, nil, nil)
	b := cacheDB(t, nil, nil)
	ctx := newTenant()
	c := fleetdb.Customers
	ok(t, c.Create(ctx, a, &fleet.Customer{Name: "A", Code: "A"}))
	ok(t, a.FlushDataVersions(context.Background()))

	eq(t, len(cachedCustomers(t, ctx, b)), 1)
	eq(t, len(cachedCustomers(t, ctx, b)), 1)
	eq(t, b.CacheStats().Hits, int64(1))

	ok(t, c.Create(ctx, a, &fleet.Customer{Name: "B", Code: "B"}))
	ok(t, a.FlushDataVersions(context.Background()))
	waitFor(t, "the other process's notification", func() bool {
		return len(cachedCustomers(t, ctx, b)) == 2
	})
	hits := b.CacheStats().Hits
	eq(t, len(cachedCustomers(t, ctx, b)), 2)
	eq(t, b.CacheStats().Hits, hits+1)
}

func TestCacheKeepsTenantsApart(t *testing.T) {
	d := cacheDB(t, nil, nil)
	x, y := newTenant(), newTenant()
	c := fleetdb.Customers
	ok(t, c.Create(x, d, &fleet.Customer{Name: "X", Code: "X"}))
	ok(t, c.Create(y, d, &fleet.Customer{Name: "Y", Code: "Y"}))

	eq(t, codes(cachedCustomers(t, x, d))[0], "X")
	eq(t, codes(cachedCustomers(t, y, d))[0], "Y")
	st := d.CacheStats()
	eq(t, st.Hits, int64(0))
	eq(t, st.Misses, int64(2))
	eq(t, codes(cachedCustomers(t, x, d))[0], "X")
	eq(t, codes(cachedCustomers(t, y, d))[0], "Y")
	eq(t, d.CacheStats().Hits, int64(2))

	ok(t, c.Create(x, d, &fleet.Customer{Name: "X2", Code: "X2"}))
	eq(t, len(cachedCustomers(t, x, d)), 2)
	hits := d.CacheStats().Hits
	eq(t, len(cachedCustomers(t, y, d)), 1)
	eq(t, d.CacheStats().Hits, hits+1)
}

// cacheKeyCtx holds the extra key material of a context in TestCacheKeySeparatesEntries.
type cacheKeyCtx struct{}

func TestCacheKeySeparatesEntries(t *testing.T) {
	d := cacheDB(t, nil, func(cfg *keel.Config) {
		cfg.Cache.Key = func(ctx context.Context) string {
			v, _ := ctx.Value(cacheKeyCtx{}).(string)
			return v
		}
	})
	ctx := newTenant()
	ok(t, fleetdb.Customers.Create(ctx, d, &fleet.Customer{Name: "A", Code: "A"}))
	alice := context.WithValue(ctx, cacheKeyCtx{}, "alice")
	bob := context.WithValue(ctx, cacheKeyCtx{}, "bob")
	cachedCustomers(t, alice, d)
	cachedCustomers(t, bob, d)
	eq(t, d.CacheStats().Misses, int64(2))
	cachedCustomers(t, alice, d)
	cachedCustomers(t, bob, d)
	eq(t, d.CacheStats().Hits, int64(2))
}

func TestCacheOpenRefusesUnsafeConfigs(t *testing.T) {
	db(t)
	cfg := testConfig(testDSN)
	cfg.Cache = &keel.CacheConfig{}
	if d, err := keel.Open(context.Background(), cfg); err == nil {
		d.Close()
		t.Fatal("Open accepted a cache without data versions")
	}
	cfg.DataVersions = keel.DataVersionsConfig{Enabled: true}
	cfg.OnBegin = func(context.Context, keel.TxInfo) ([]keel.Statement, error) { return nil, nil }
	if d, err := keel.Open(context.Background(), cfg); err == nil {
		d.Close()
		t.Fatal("Open accepted a cache without Key under OnBegin")
	}
	cfg.Cache.Key = func(context.Context) string { return "" }
	d, err := keel.Open(context.Background(), cfg)
	ok(t, err)
	d.Close()
}

func TestCacheBypasses(t *testing.T) {
	rec := &recorder{}
	d := cacheDB(t, rec, nil)
	ctx := newTenant()
	c := fleetdb.Customers
	ok(t, c.Create(ctx, d, &fleet.Customer{Name: "A", Code: "A"}))
	cachedCustomers(t, ctx, d)

	bypassed := func(name string, fn func() error) {
		t.Helper()
		before := d.CacheStats()
		rec.reset()
		if err := fn(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		after := d.CacheStats()
		if after.Bypasses != before.Bypasses+1 || after.Hits != before.Hits {
			t.Fatalf("%s: %+v after %+v, want one bypass", name, after, before)
		}
		if rec.statements() == 0 {
			t.Fatalf("%s: sent nothing", name)
		}
	}
	bypassed("Write", func() error {
		return keel.Write(ctx, d, func(ctx context.Context) error {
			cachedCustomers(t, ctx, d)
			return nil
		})
	})
	bypassed("a RepeatableRead Read that has begun", func() error {
		return keel.Read(ctx, d, func(ctx context.Context) error {
			if _, err := c.Query().Count(ctx, d); err != nil {
				return err
			}
			before := d.CacheStats().Bypasses
			cachedCustomers(t, ctx, d)
			if d.CacheStats().Bypasses != before+1 {
				t.Fatal("a begun snapshot transaction was not bypassed")
			}
			return nil
		}, keel.RepeatableRead)
	})
	bypassed("system scope", func() error {
		_, err := c.AllTenants().Order(c.Code.Asc()).Limit(5).Cache().List(systemCtx("cache test"), d)
		return err
	})
	bypassed("Lock", func() error {
		_, err := c.Order(c.Code.Asc()).Lock(keel.ForShare).Cache().All(ctx, d)
		return err
	})
	bypassed("a batch with an item that does not ask", func() error {
		var rows []*fleet.Customer
		var n int64
		return keel.Batch(ctx, d, keel.AllInto(c.Order(c.Code.Asc()).Cache(), &rows), keel.CountInto(c.Query(), &n))
	})

	hits := d.CacheStats().Hits
	rec.reset()
	ok(t, keel.Read(ctx, d, func(ctx context.Context) error {
		cachedCustomers(t, ctx, d)
		return nil
	}))
	eq(t, d.CacheStats().Hits, hits+1)
	eq(t, rec.statements(), 0)
}

func TestCacheInsideAnUnbegunSnapshotTransaction(t *testing.T) {
	rec := &recorder{}
	d := cacheDB(t, rec, nil)
	ctx := newTenant()
	c := fleetdb.Customers
	ok(t, c.Create(ctx, d, &fleet.Customer{Name: "A", Code: "A"}))
	ok(t, d.FlushDataVersions(context.Background()))
	cachedCustomers(t, ctx, d)

	ok(t, keel.Read(ctx, d, func(ctx context.Context) error {
		hits := d.CacheStats().Hits
		rec.reset()
		eq(t, len(cachedCustomers(t, ctx, d)), 1)
		eq(t, d.CacheStats().Hits, hits+1)
		eq(t, rec.statements(), 0)

		rows := must(c.Order(c.Code.Asc()).All(ctx, d))(t)
		eq(t, len(rows), 1)
		level := must(keel.QueryValue[string](ctx, d, "SELECT current_setting('transaction_isolation')"))(t)
		eq(t, level, "repeatable read")

		bypasses := d.CacheStats().Bypasses
		eq(t, len(cachedCustomers(t, ctx, d)), 1)
		eq(t, d.CacheStats().Bypasses, bypasses+1)
		return nil
	}, keel.RepeatableRead))

	fresh := newTenant()
	ok(t, c.Create(fresh, d, &fleet.Customer{Name: "B", Code: "B"}))
	ok(t, d.FlushDataVersions(context.Background()))
	ok(t, keel.Read(fresh, d, func(ctx context.Context) error {
		misses := d.CacheStats().Misses
		eq(t, len(cachedCustomers(t, ctx, d)), 1)
		eq(t, d.CacheStats().Misses, misses+1)
		eq(t, must(keel.QueryValue[string](ctx, d, "SELECT current_setting('transaction_isolation')"))(t),
			"repeatable read")
		return nil
	}, keel.RepeatableRead))
	hits := d.CacheStats().Hits
	eq(t, len(cachedCustomers(t, fresh, d)), 1)
	eq(t, d.CacheStats().Hits, hits+1)
}

func TestCacheNeverTakesASecondPoolConnection(t *testing.T) {
	d := cacheDB(t, nil, func(cfg *keel.Config) {
		pc, err := pgxpool.ParseConfig(testDSN)
		ok(t, err)
		pc.MaxConns = 1
		cfg.Pool = pc
	})
	c := fleetdb.Customers
	bounded := func(ctx context.Context) context.Context {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		t.Cleanup(cancel)
		return ctx
	}

	ctx := bounded(newTenant())
	ok(t, c.Create(ctx, d, &fleet.Customer{Name: "A", Code: "A"}))
	eq(t, len(cachedCustomers(t, ctx, d)), 1)

	inTx := bounded(newTenant())
	ok(t, c.Create(inTx, d, &fleet.Customer{Name: "B", Code: "B"}))
	ok(t, keel.Read(inTx, d, func(ctx context.Context) error {
		if _, err := c.Query().Count(ctx, d); err != nil {
			return err
		}
		eq(t, len(cachedCustomers(t, ctx, d)), 1)
		return nil
	}))

	unbegun := bounded(newTenant())
	ok(t, c.Create(unbegun, d, &fleet.Customer{Name: "C", Code: "C"}))
	ok(t, keel.Read(unbegun, d, func(ctx context.Context) error {
		eq(t, len(cachedCustomers(t, ctx, d)), 1)
		eq(t, len(cachedCustomers(t, ctx, d)), 1)
		return nil
	}, keel.RepeatableRead))
}

func TestCacheRecoversFromListenerLoss(t *testing.T) {
	a := cacheDB(t, nil, nil)
	b := cacheDB(t, nil, nil)
	ctx := newTenant()
	c := fleetdb.Customers
	ok(t, c.Create(ctx, a, &fleet.Customer{Name: "A", Code: "A"}))
	ok(t, a.FlushDataVersions(context.Background()))
	eq(t, len(cachedCustomers(t, ctx, b)), 1)
	eq(t, len(cachedCustomers(t, ctx, b)), 1)
	eq(t, b.CacheStats().Hits, int64(1))

	lost := b.CacheStats().Errors
	_, err := a.Pool().Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid() AND query = 'LISTEN keel_data_versions'`)
	ok(t, err)
	waitFor(t, "the listener loss", func() bool { return b.CacheStats().Errors > lost })

	ok(t, c.Create(ctx, a, &fleet.Customer{Name: "B", Code: "B"}))
	ok(t, a.FlushDataVersions(context.Background()))

	waitFor(t, "the listener to reconnect", func() bool { return b.CacheStats().Listening })
	eq(t, len(cachedCustomers(t, ctx, b)), 2)
	hits := b.CacheStats().Hits
	eq(t, len(cachedCustomers(t, ctx, b)), 2)
	eq(t, b.CacheStats().Hits, hits+1)
}

func TestCacheSharedStore(t *testing.T) {
	shared := cache.NewMemory(cache.MemoryOptions{MaxBytes: 8 << 20})
	share := func(cfg *keel.Config) { cfg.Cache.Shared = shared }
	recB := &recorder{}
	a := cacheDB(t, nil, share)
	b := cacheDB(t, recB, share)
	ctx := newTenant()
	c := fleetdb.Customers
	ok(t, c.Create(ctx, a, &fleet.Customer{Name: "A", Code: "A"}))
	ok(t, a.FlushDataVersions(context.Background()))

	eq(t, len(cachedCustomers(t, ctx, a)), 1)
	recB.reset()
	eq(t, len(cachedCustomers(t, ctx, b)), 1)
	eq(t, b.CacheStats().Hits, int64(1))
	for _, e := range recB.events {
		if e.Table == "customers" {
			t.Fatal("a shared hit read the table")
		}
	}

	ok(t, c.Create(ctx, a, &fleet.Customer{Name: "B", Code: "B"}))
	eq(t, len(cachedCustomers(t, ctx, a)), 2)
	eq(t, len(cachedCustomers(t, ctx, a)), 2)
}

func TestCacheSharedStoreErrorsAreMisses(t *testing.T) {
	var mu sync.Mutex
	var reported []error
	d := cacheDB(t, nil, func(cfg *keel.Config) {
		cfg.Cache.Shared = failingStore{}
		cfg.Cache.OnError = func(err error) {
			mu.Lock()
			reported = append(reported, err)
			mu.Unlock()
		}
	})
	ctx := newTenant()
	ok(t, fleetdb.Customers.Create(ctx, d, &fleet.Customer{Name: "A", Code: "A"}))
	ok(t, d.FlushDataVersions(context.Background()))
	eq(t, len(cachedCustomers(t, ctx, d)), 1)
	eq(t, len(cachedCustomers(t, ctx, d)), 1)
	eq(t, d.CacheStats().Hits, int64(1))
	mu.Lock()
	defer mu.Unlock()
	if len(reported) == 0 || !errors.Is(reported[0], errStoreDown) {
		t.Fatalf("reported %v", reported)
	}
}

// errStoreDown is failingStore's error.
var errStoreDown = errors.New("store down")

// failingStore is a shared store that fails every call.
type failingStore struct{}

func (failingStore) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, errStoreDown
}

func (failingStore) Set(context.Context, string, []byte, time.Duration) error { return errStoreDown }
