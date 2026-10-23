package keel_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/jackc/pgx/v5/pgconn"
)

// recorder is an observer that records statements and the round trips they shared.
type recorder struct {
	mu     sync.Mutex
	events []keel.QueryEvent
}

func (r *recorder) BeforeQuery(ctx context.Context, _ *keel.QueryEvent) context.Context { return ctx }

func (r *recorder) AfterQuery(_ context.Context, e *keel.QueryEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, *e)
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
}

// trips counts distinct round trips: statements of one batch share a start time.
func (r *recorder) trips() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[time.Time]bool{}
	for _, e := range r.events {
		seen[e.Start] = true
	}
	return len(seen)
}

func (r *recorder) statements() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// scopedDB opens a second DB on the test database with an OnBegin hook that records the tenant in
// a transaction setting, as a row-level security setup would.
func scopedDB(t *testing.T, rec *recorder) *keel.DB {
	t.Helper()
	db(t)
	cfg := testConfig(testDSN)
	cfg.Observers = []keel.Observer{rec}
	cfg.OnBegin = func(ctx context.Context, tx keel.TxInfo) ([]keel.Statement, error) {
		org := "system"
		if v, ok := ctx.Value(tenantKey{}).(tenant); ok {
			org = string(v.org)
		}
		return []keel.Statement{{SQL: "SELECT set_config('keel.org', $1, true)", Args: []any{org}}}, nil
	}
	d, err := keel.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

func TestImplicitTransactionIsOneRoundTrip(t *testing.T) {
	rec := &recorder{}
	d := scopedDB(t, rec)
	w := seed(t, 3)
	s := fleetdb.Shipments

	rec.reset()
	org := string(w.shipments[0].OrganizationID)
	n := must(s.Where(keel.Raw("current_setting('keel.org') = ?", org)).Count(w.ctx, d))(t)
	eq(t, n, int64(3))
	eq(t, rec.trips(), 1)

	rec.reset()
	page := must(s.Order(s.ProNumber.Asc()).Page(w.ctx, d, keel.After("", 2).Total(100)))(t)
	eq(t, len(page.Items), 2)
	eq(t, rec.statements(), 2)
	eq(t, rec.trips(), 1)

	rec.reset()
	_ = must(s.Load(s.Customer(), s.Moves().Stops()).All(w.ctx, d))(t)
	eq(t, rec.trips(), 3)
}

func TestEmptyTransactionCostsNothing(t *testing.T) {
	rec := &recorder{}
	d := scopedDB(t, rec)
	ok(t, keel.Read(newTenant(), d, func(context.Context) error { return nil }))
	eq(t, rec.statements(), 0)
}

func TestWriteCommitsAndRollsBack(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Customers

	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		if !keel.InTx(ctx, d) {
			t.Fatal("no transaction on the context")
		}
		return c.Create(ctx, d, &fleet.Customer{Name: "kept", Code: "K"})
	}))

	boom := errors.New("boom")
	err := keel.Write(ctx, d, func(ctx context.Context) error {
		if err := c.Create(ctx, d, &fleet.Customer{Name: "lost", Code: "L"}); err != nil {
			return err
		}
		n, err := c.Query().Count(ctx, d)
		if err != nil {
			return err
		}
		if n != 2 {
			t.Fatalf("inside the transaction: %d rows", n)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	eq(t, must(c.Query().Count(ctx, d))(t), int64(1))
}

func TestNestedTransactions(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		return keel.Write(ctx, d, func(ctx context.Context) error {
			return fleetdb.Customers.Create(ctx, d, &fleet.Customer{Name: "nested", Code: "N"})
		})
	}))
	err := keel.Read(ctx, d, func(ctx context.Context) error {
		return keel.Write(ctx, d, func(context.Context) error { return nil })
	})
	if !errors.Is(err, keel.ErrReadOnly) {
		t.Fatalf("got %v, want ErrReadOnly", err)
	}
	err = keel.Read(ctx, d, func(ctx context.Context) error {
		return fleetdb.Customers.Create(ctx, d, &fleet.Customer{Name: "ro", Code: "RO"})
	})
	if !errors.Is(err, keel.ErrReadOnly) {
		t.Fatalf("got %v, want ErrReadOnly", err)
	}
}

func TestAfterCommit(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	ran := 0
	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		keel.AfterCommit(ctx, func(context.Context) { ran++ })
		if ran != 0 {
			t.Fatal("ran before commit")
		}
		return fleetdb.Customers.Create(ctx, d, &fleet.Customer{Name: "a", Code: "A"})
	}))
	eq(t, ran, 1)

	_ = keel.Write(ctx, d, func(ctx context.Context) error {
		keel.AfterCommit(ctx, func(context.Context) { ran++ })
		return errors.New("rollback")
	})
	eq(t, ran, 1)
}

func TestRetry(t *testing.T) {
	d := db(t)
	attempts := 0
	ok(t, keel.Write(newTenant(), d, func(context.Context) error {
		attempts++
		if attempts < 3 {
			return &pgconn.PgError{Code: "40001"}
		}
		return nil
	}, keel.Retry(3)))
	eq(t, attempts, 3)
}

func TestLockTimeoutOption(t *testing.T) {
	d := db(t)
	w := seed(t, 1)
	s := fleetdb.Shipments
	ok(t, keel.Write(w.ctx, d, func(ctx context.Context) error {
		n, err := s.Where(keel.Raw("current_setting('lock_timeout') = ?", "250ms")).Count(ctx, d)
		if err != nil {
			return err
		}
		if n != 1 {
			t.Fatalf("lock_timeout not applied")
		}
		return nil
	}, keel.LockTimeout(250*time.Millisecond)))
}

func TestTableHooks(t *testing.T) {
	rec := &recorder{}
	d := scopedDB(t, rec)
	c := fleetdb.Customers
	var mu sync.Mutex
	var log []string
	add := func(s string) {
		mu.Lock()
		log = append(log, s)
		mu.Unlock()
	}
	keel.OnCreate(d, c, keel.BeforeWrite, func(_ context.Context, m *keel.Mutation[fleet.Customer]) error {
		m.Row.Code = "HOOK-" + m.Row.Code
		add("before create")
		return nil
	})
	keel.OnCreate(d, c, keel.AfterWrite, func(_ context.Context, m *keel.Mutation[fleet.Customer]) error {
		add("after create " + m.Row.Code)
		if m.Row.Name == "reject" {
			return errors.New("rejected")
		}
		return nil
	})
	keel.OnWrite(d, c, keel.Committed, func(_ context.Context, m *keel.Mutation[fleet.Customer]) error {
		add("committed " + m.Op.String())
		return nil
	})
	keel.OnUpdate(d, c, keel.AfterWrite, func(_ context.Context, m *keel.Mutation[fleet.Customer]) error {
		if m.Changed(c.Name) {
			add("renamed from " + m.Old.Name + " to " + m.Row.Name)
		}
		return nil
	}, keel.Needs(c.Name))

	ctx := newTenant()
	cust := &fleet.Customer{Name: "Hooked", Code: "H1"}
	ok(t, c.Create(ctx, d, cust))
	eq(t, cust.Code, "HOOK-H1")

	err := c.Create(ctx, d, &fleet.Customer{Name: "reject", Code: "R"})
	if err == nil {
		t.Fatal("AfterWrite error did not fail the create")
	}
	eq(t, must(c.Where(c.Name.Eq("reject")).Exists(ctx, d))(t), false)

	cust.Name = "Renamed"
	ok(t, c.Save(ctx, d, cust))

	n := must(c.Where(c.ID.Eq(cust.ID)).Update(ctx, d, c.Name.To("Bulk")))(t)
	eq(t, n, int64(1))

	want := []string{
		"before create", "after create HOOK-H1", "committed create",
		"before create", "after create HOOK-R",
		"renamed from Hooked to Renamed", "committed update",
		"renamed from Renamed to Bulk", "committed update",
	}
	mu.Lock()
	defer mu.Unlock()
	if len(log) != len(want) {
		t.Fatalf("hook log:\n%q\nwant:\n%q", log, want)
	}
	for i := range want {
		eq(t, log[i], want[i])
	}
}

func TestInterceptor(t *testing.T) {
	rec := &recorder{}
	d := scopedDB(t, rec)
	w := seed(t, 6)
	s := fleetdb.Shipments
	d.Intercept(s, func(_ context.Context, q *keel.Intercepted) error {
		if q.Read {
			q.Where(s.Hazmat.IsFalse())
		}
		return nil
	})
	eq(t, must(s.Query().Count(w.ctx, d))(t), int64(4))

	mv := fleetdb.Moves
	d.Intercept(mv, func(_ context.Context, q *keel.Intercepted) error {
		q.Where(mv.Loaded.IsTrue())
		return nil
	})
	rows := must(s.Load(s.Moves()).Where(s.ProNumber.Eq("PRO-0001")).All(w.ctx, d))(t)
	eq(t, len(rows), 1)
	eq(t, len(rows[0].Moves), 1)
	eq(t, rows[0].Moves[0].Loaded, true)
}
