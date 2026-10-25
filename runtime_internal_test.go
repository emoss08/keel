package keel

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTenantLimiterBoundsAndForgets(t *testing.T) {
	l := newTenantLimiter(2)
	sc := &scopeState{tenant: []any{"org", "bu"}}
	var running, peak atomic.Int32
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slot, owned, err := l.enter(context.Background(), sc)
			if err != nil || !owned {
				t.Errorf("enter: %v, owned %v", err, owned)
				return
			}
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(100 * time.Microsecond)
			running.Add(-1)
			slot.release()
		}()
	}
	wg.Wait()
	if p := peak.Load(); p > 2 || p < 1 {
		t.Fatalf("peak concurrency %d, want at most 2", p)
	}
	if n := l.size(); n != 0 {
		t.Fatalf("%d idle tenants left in the map", n)
	}
	if l.waits.Load() == 0 {
		t.Fatal("no acquisition waited")
	}
}

func TestTenantLimiterCancelledWaiter(t *testing.T) {
	l := newTenantLimiter(1)
	sc := &scopeState{tenant: []any{"org"}}
	held, _, err := l.enter(context.Background(), sc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := l.enter(ctx, sc); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want a deadline", err)
	}

	shared, owned, err := l.enter(context.WithValue(context.Background(), slotKey{}, held), sc)
	if err != nil || owned || shared != held {
		t.Fatalf("a context carrying the slot did not share it: %v, owned %v", err, owned)
	}
	other := &scopeState{tenant: []any{"other"}}
	slot, owned, err := l.enter(context.WithValue(context.Background(), slotKey{}, held), other)
	if err != nil || !owned {
		t.Fatalf("another tenant: %v, owned %v", err, owned)
	}
	slot.release()

	if s, _, _ := l.enter(context.Background(), &scopeState{system: true}); s != nil {
		t.Fatal("a system scope took a slot")
	}
	if s, _, _ := l.enter(context.Background(), &scopeState{}); s != nil {
		t.Fatal("unscoped work took a slot")
	}

	got := make(chan *tenantSlot)
	go func() {
		s, _, err := l.enter(context.Background(), sc)
		if err != nil {
			t.Error(err)
		}
		got <- s
	}()
	for l.waits.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	held.release()
	(<-got).release()
	if n := l.size(); n != 0 {
		t.Fatalf("%d idle tenants left in the map", n)
	}
}

func TestTenantLimiterHandOffRace(t *testing.T) {
	l := newTenantLimiter(1)
	sc := &scopeState{tenant: []any{"org"}}
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%5)*100*time.Microsecond)
			defer cancel()
			slot, owned, err := l.enter(ctx, sc)
			if err != nil {
				return
			}
			if !owned {
				t.Error("not owned")
			}
			slot.release()
		}()
	}
	wg.Wait()
	slot, _, err := l.enter(context.Background(), sc)
	if err != nil {
		t.Fatalf("the slot leaked: %v", err)
	}
	slot.release()
	if n := l.size(); n != 0 {
		t.Fatalf("%d idle tenants left in the map", n)
	}
}

func TestFuncPackage(t *testing.T) {
	cases := map[string]string{
		"github.com/emoss08/keel.(*runner).batch":                 "github.com/emoss08/keel",
		"github.com/emoss08/keel.(*Query[...]).List":              "github.com/emoss08/keel",
		"github.com/emoss08/keel/internal/fleet/fleetdb.(*T).Get": "github.com/emoss08/keel/internal/fleet/fleetdb",
		"example.com/app/store.(*Repo).List.func1":                "example.com/app/store",
		"main.main": "main",
		"github.com/emoss08/keel_test.TestBudget.func2": "github.com/emoss08/keel_test",
	}
	for fn, want := range cases {
		if got := funcPackage(fn); got != want {
			t.Errorf("funcPackage(%q) = %q, want %q", fn, got, want)
		}
	}
	if keelPackage != "github.com/emoss08/keel" {
		t.Fatalf("keelPackage = %q", keelPackage)
	}
	for fn, internal := range map[string]bool{
		"github.com/emoss08/keel.(*runner).batch":                 true,
		"github.com/emoss08/keel/internal/fleet/fleetdb.(*T).Get": true,
		"example.com/app/store.(*Repo).List":                      false,
		"github.com/emoss08/keel_test.TestBudget":                 false,
		"github.com/emoss08/keel/analytics.(*Runner).Run":         false,
	} {
		if got := internalFrame(fn); got != internal {
			t.Errorf("internalFrame(%q) = %v, want %v", fn, got, internal)
		}
	}
}

func TestBudgetCountsTextsOncePerRoundTrip(t *testing.T) {
	ctx := Budget(context.Background(), MaxRepeats(2))
	a, b := &stmt{sql: "SELECT a"}, &stmt{sql: "SELECT b"}
	if err := chargeBudgets(ctx, []*stmt{a, a, b}, 1); err != nil {
		t.Fatal(err)
	}
	if err := chargeBudgets(ctx, []*stmt{a}, 1); err != nil {
		t.Fatal(err)
	}
	r, _ := BudgetReportFrom(ctx)
	if r.RoundTrips != 2 || r.Statements != 4 || len(r.Repeats) != 1 || r.Repeats[0].Count != 2 {
		t.Fatalf("report %+v", r)
	}
	if len(r.Repeats[0].CallSites) != 1 {
		t.Fatalf("call sites %v", r.Repeats[0].CallSites)
	}
	err := chargeBudgets(ctx, []*stmt{a}, 1)
	var be *BudgetError
	if !errors.As(err, &be) || be.Limit != "repeats" || be.Max != 2 {
		t.Fatalf("got %v", err)
	}
	if err := refuseBudgets(ctx); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("an exceeded budget did not refuse: %v", err)
	}
}
