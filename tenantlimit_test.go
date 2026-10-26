package keel_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTenantConcurrency(t *testing.T) {
	d := openDB(t, func(cfg *keel.Config) { cfg.TenantConcurrency = 1 })
	a, b := newTenant(), newTenant()
	c := fleetdb.Customers
	ok(t, c.Create(a, d, &fleet.Customer{Name: "A", Code: "A"}))

	count := func(ctx context.Context, timeout time.Duration) error {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		_, err := c.Query().Count(ctx, d)
		return err
	}

	waiter := make(chan error, 1)
	ok(t, keel.Read(a, d, func(ctx context.Context) error {
		_, err := c.Query().Count(ctx, d)
		ok(t, err)

		before := d.TenantWaits()
		err = count(a, 100*time.Millisecond)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a second terminal of the tenant: got %v, want a deadline", err)
		}
		eq(t, d.TenantWaits(), before+1)

		ok(t, count(b, 5*time.Second))
		sys := systemCtx("tenant concurrency test")
		_, err = c.AllTenants().Count(sys, d)
		ok(t, err)

		ok(t, keel.Read(ctx, d, func(ctx context.Context) error {
			_, err := c.Query().Count(ctx, d)
			return err
		}, keel.Detached()))

		go func() { waiter <- count(a, 10*time.Second) }()
		deadline := time.Now().Add(5 * time.Second)
		for d.TenantWaits() < before+2 {
			if time.Now().After(deadline) {
				t.Fatal("the waiting terminal did not wait")
			}
			time.Sleep(time.Millisecond)
		}
		return nil
	}))
	ok(t, <-waiter)
	ok(t, count(a, 5*time.Second))
}

func TestTenantConcurrencyWriteHooksShareTheSlot(t *testing.T) {
	d := openDB(t, func(cfg *keel.Config) { cfg.TenantConcurrency = 1 })
	c := fleetdb.Customers
	errDuplicate := errors.New("duplicate code")
	keel.OnCreate(d, c, keel.BeforeWrite, func(ctx context.Context, m *keel.Mutation[*fleet.Customer]) error {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		n, err := c.Where(c.Code.Eq(m.Row.Code)).Count(ctx, d)
		if err != nil {
			return err
		}
		if n > 0 {
			return errDuplicate
		}
		return nil
	})
	ctx := newTenant()
	ok(t, c.Create(ctx, d, &fleet.Customer{Name: "Hooked", Code: "SLOT"}))
	if err := c.Create(ctx, d, &fleet.Customer{Name: "Again", Code: "SLOT"}); !errors.Is(err, errDuplicate) {
		t.Fatalf("got %v, want the hook's error", err)
	}
	eq(t, d.TenantWaits(), int64(0))
}

func TestTenantConcurrencyOffByDefault(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	ok(t, keel.Read(ctx, d, func(context.Context) error {
		_, err := fleetdb.Customers.Query().Count(ctx, d)
		return err
	}))
	eq(t, d.TenantWaits(), int64(0))

	cfg := testConfig(testDSN)
	cfg.TenantConcurrency = -1
	if _, err := keel.Open(context.Background(), cfg); err == nil {
		t.Fatal("Open accepted a negative TenantConcurrency")
	}
}

type panicObserver struct{ armed bool }

func (o *panicObserver) BeforeQuery(ctx context.Context, _ *keel.QueryEvent) context.Context {
	return ctx
}

func (o *panicObserver) AfterQuery(context.Context, *keel.QueryEvent) {
	if o.armed {
		o.armed = false
		panic("observer failed")
	}
}

func TestTenantConcurrencySlotSurvivesAPanic(t *testing.T) {
	obs := &panicObserver{}
	d := openDB(t, func(cfg *keel.Config) {
		cfg.TenantConcurrency = 1
		cfg.Observers = []keel.Observer{obs}
	})
	a := newTenant()
	c := fleetdb.Customers
	ok(t, c.Create(a, d, &fleet.Customer{Name: "A", Code: "A"}))

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the observer's panic did not reach the caller")
			}
		}()
		obs.armed = true
		_, _ = c.Query().Count(a, d)
	}()

	ctx, cancel := context.WithTimeout(a, 5*time.Second)
	defer cancel()
	n, err := c.Query().Count(ctx, d)
	ok(t, err)
	eq(t, n, int64(1))
	eq(t, d.Pool().Stat().AcquiredConns(), int32(0))
}

func TestSlowWaitsReportATenantSlotWaitWithWhoHoldsTheSlot(t *testing.T) {
	reports := make(chan keel.SlowWait, 4)
	d := openDB(t, func(cfg *keel.Config) {
		cfg.TenantConcurrency = 1
		cfg.SlowWaits = keel.SlowWaitConfig{
			After:  50 * time.Millisecond,
			Report: func(_ context.Context, w *keel.SlowWait) { reports <- *w },
		}
	})
	a := newTenant()
	c := fleetdb.Customers

	waited := make(chan error, 1)
	ok(t, keel.Read(a, d, func(ctx context.Context) error {
		_, err := c.Query().Count(ctx, d)
		ok(t, err)
		go func() {
			_, err := c.Query().Count(a, d)
			waited <- err
		}()
		select {
		case w := <-reports:
			eq(t, w.Kind, keel.WaitTenantSlot)
			eq(t, w.Kind.String(), "tenant_slot")
			eq(t, w.Limit, 1)
			eq(t, w.Waiters, 1)
			if len(w.Holders) != 1 || w.Holders[0].Held < w.Waited {
				t.Fatalf("holders %+v, waited %v", w.Holders, w.Waited)
			}
			if !stackNames(w.Stack, "TestSlowWaitsReportATenantSlotWaitWithWhoHoldsTheSlot.func2.1") {
				t.Fatalf("the waiter's stack does not start at its caller: %q", w.Stack)
			}
			if !stackNames(w.Holders[0].Stack, "TestSlowWaitsReportATenantSlotWaitWithWhoHoldsTheSlot") {
				t.Fatalf("the holder's stack does not name the transaction's caller: %q", w.Holders[0].Stack)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a wait past the threshold was not reported")
		}
		return nil
	}))
	ok(t, <-waited)
}

func TestSlowWaitsReportAConnectionWaitWithThePoolState(t *testing.T) {
	reports := make(chan keel.SlowWait, 4)
	d := openDB(t, func(cfg *keel.Config) {
		pc, err := pgxpool.ParseConfig(cfg.DSN)
		ok(t, err)
		pc.MaxConns = 1
		cfg.Pool = pc
		cfg.SlowWaits = keel.SlowWaitConfig{
			After:  50 * time.Millisecond,
			Report: func(_ context.Context, w *keel.SlowWait) { reports <- *w },
		}
	})
	a := newTenant()
	c := fleetdb.Customers

	waited := make(chan error, 1)
	ok(t, keel.Read(a, d, func(ctx context.Context) error {
		_, err := c.Query().Count(ctx, d)
		ok(t, err)
		go func() {
			_, err := c.Query().Count(a, d)
			waited <- err
		}()
		select {
		case w := <-reports:
			eq(t, w.Kind, keel.WaitConnection)
			eq(t, w.Pool.Max, int32(1))
			eq(t, w.Pool.Acquired, int32(1))
			if w.Tenant == "" {
				t.Fatal("the report does not name the waiting work's tenant")
			}
			if !stackNames(w.Stack, "TestSlowWaitsReportAConnectionWaitWithThePoolState.func2.1") {
				t.Fatalf("the waiter's stack does not start at its caller: %q", w.Stack)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a connection wait past the threshold was not reported")
		}
		return nil
	}))
	ok(t, <-waited)
}

// stackNames reports whether the first frame of stack is in a function whose name ends in fn.
func stackNames(stack []string, fn string) bool {
	if len(stack) == 0 {
		return false
	}
	name, _, _ := strings.Cut(stack[0], " ")
	return strings.HasSuffix(name, "."+fn)
}
