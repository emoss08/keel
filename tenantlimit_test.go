package keel_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
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
