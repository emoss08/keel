package keel

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// enterSlot takes a slot for work under sc, the way a runner or a transaction does.
func enterSlot(ctx context.Context, l *tenantLimiter, sc *scopeState) (*slotHold, error) {
	h := &slotHold{}
	err := l.enter(ctx, sc, h)
	return h, err
}

func TestTenantLimiterBoundsAndForgets(t *testing.T) {
	l := newTenantLimiter(2, nil)
	sc := &scopeState{tenant: []any{"org", "bu"}}
	var running, peak atomic.Int32
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := enterSlot(context.Background(), l, sc)
			if err != nil || !h.owned {
				t.Errorf("enter: %v, owned %v", err, h.owned)
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
			h.release()
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
	l := newTenantLimiter(1, nil)
	sc := &scopeState{tenant: []any{"org"}}
	held, err := enterSlot(context.Background(), l, sc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := enterSlot(ctx, l, sc); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want a deadline", err)
	}

	shared, err := enterSlot(context.WithValue(context.Background(), slotKey{}, held.slot), l, sc)
	if err != nil || shared.owned || shared.slot != held.slot {
		t.Fatalf("a context carrying the slot did not share it: %v, owned %v", err, shared.owned)
	}
	other := &scopeState{tenant: []any{"other"}}
	h, err := enterSlot(context.WithValue(context.Background(), slotKey{}, held.slot), l, other)
	if err != nil || !h.owned {
		t.Fatalf("another tenant: %v, owned %v", err, h.owned)
	}
	h.release()

	if h, _ := enterSlot(context.Background(), l, &scopeState{system: true}); h.slot != nil {
		t.Fatal("a system scope took a slot")
	}
	if h, _ := enterSlot(context.Background(), l, &scopeState{}); h.slot != nil {
		t.Fatal("unscoped work took a slot")
	}

	got := make(chan *slotHold)
	go func() {
		h, err := enterSlot(context.Background(), l, sc)
		if err != nil {
			t.Error(err)
		}
		got <- h
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
	l := newTenantLimiter(1, nil)
	sc := &scopeState{tenant: []any{"org"}}
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%5)*100*time.Microsecond)
			defer cancel()
			h, err := enterSlot(ctx, l, sc)
			if err != nil {
				return
			}
			if !h.owned {
				t.Error("not owned")
			}
			h.release()
		}()
	}
	wg.Wait()
	h, err := enterSlot(context.Background(), l, sc)
	if err != nil {
		t.Fatalf("the slot leaked: %v", err)
	}
	h.release()
	if n := l.size(); n != 0 {
		t.Fatalf("%d idle tenants left in the map", n)
	}
}

func TestTenantLimiterReportsASlowWaitWithTheHolders(t *testing.T) {
	reports := make(chan SlowWait, 1)
	l := newTenantLimiter(1, newSlowWaits(SlowWaitConfig{
		After:  20 * time.Millisecond,
		Report: func(_ context.Context, w *SlowWait) { reports <- *w },
	}))
	sc := &scopeState{tenant: []any{"org"}}
	held, err := enterSlot(context.Background(), l, sc)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan *slotHold)
	go func() {
		h, err := enterSlot(context.Background(), l, sc)
		if err != nil {
			t.Error(err)
		}
		done <- h
	}()

	var w SlowWait
	select {
	case w = <-reports:
	case <-time.After(5 * time.Second):
		t.Fatal("a wait past the threshold was not reported")
	}
	if w.Kind != WaitTenantSlot || w.Tenant != TenantKeyOf("org") || w.Limit != 1 || w.Waiters != 1 {
		t.Fatalf("report %+v", w)
	}
	if w.Waited < 20*time.Millisecond || len(w.Holders) != 1 || w.Holders[0].Held < w.Waited {
		t.Fatalf("report %+v", w)
	}

	held.release()
	(<-done).release()
	select {
	case w := <-reports:
		t.Fatalf("a wait was reported twice: %+v", w)
	default:
	}
	if n := l.size(); n != 0 {
		t.Fatalf("%d idle tenants left in the map", n)
	}
}

func TestSlowWaitsReportACacheFillWait(t *testing.T) {
	reports := make(chan SlowWait, 2)
	db := &DB{slow: newSlowWaits(SlowWaitConfig{
		After:  10 * time.Millisecond,
		Report: func(_ context.Context, w *SlowWait) { reports <- *w },
	})}
	fill := make(chan struct{})
	done := make(chan struct{})
	go func() {
		db.waitFill(context.Background(), fill, "tenant")
		close(done)
	}()
	select {
	case w := <-reports:
		if w.Kind != WaitCacheFill || w.Tenant != "tenant" || w.Waited < 10*time.Millisecond {
			t.Fatalf("report %+v", w)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cache fill wait past the threshold was not reported")
	}
	close(fill)
	<-done

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	(&DB{}).waitFill(ctx, make(chan struct{}), "tenant")
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

func TestRunnerIdleReturnsAConnectionNothingWasSentOn(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := Open(ctx, Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	held := func(want int32) {
		t.Helper()
		if got := db.pool.Stat().AcquiredConns(); got != want {
			t.Errorf("acquired connections: got %d, want %d", got, want)
		}
	}

	r, err := db.runnerFor(ctx, modeRead)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	held(1)
	r.idle()
	held(0)
	if err := r.close(ctx, nil); err != nil {
		t.Fatal(err)
	}

	err = Read(ctx, db, func(ctx context.Context) error {
		r, err := db.runnerFor(ctx, modeRead)
		if err != nil {
			return err
		}
		if _, err := r.acquire(ctx); err != nil {
			return err
		}
		held(1)
		r.idle()
		held(0)
		if _, err := QueryValue[int](ctx, db, "SELECT 1"); err != nil {
			return err
		}
		r.idle()
		held(1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
