package keel_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

func TestBudgetCounts(t *testing.T) {
	d := db(t)
	w := seed(t, 3)
	s := fleetdb.Shipments

	ctx := keel.Budget(w.ctx)
	page := must(s.Order(s.ProNumber.Asc()).Page(ctx, d, keel.After("", 2).Total(100)))(t)
	eq(t, len(page.Items), 2)
	r, found := keel.BudgetReportFrom(ctx)
	eq(t, found, true)
	eq(t, r.RoundTrips, 1)
	eq(t, r.Statements, 2)
	eq(t, len(r.Repeats), 0)

	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		return fleetdb.Customers.Create(ctx, d, &fleet.Customer{Name: "Budgeted", Code: "BUD"})
	}))
	r, _ = keel.BudgetReportFrom(ctx)
	eq(t, r.RoundTrips, 2)
	eq(t, r.Statements, 3)

	_, found = keel.BudgetReportFrom(w.ctx)
	eq(t, found, false)
}

func TestBudgetLimitsRoundTrips(t *testing.T) {
	d := db(t)
	w := seed(t, 2)
	c := fleetdb.Customers
	ctx := keel.Budget(w.ctx, keel.MaxRoundTrips(1))
	_ = must(c.Query().Count(ctx, d))(t)

	_, err := c.Query().Count(ctx, d)
	var be *keel.BudgetError
	if !errors.As(err, &be) || !errors.Is(err, keel.ErrBudgetExceeded) {
		t.Fatalf("got %v, want a *BudgetError", err)
	}
	eq(t, be.Limit, "round trips")
	eq(t, be.Max, 1)
	eq(t, be.Report.RoundTrips, 2)

	_, err = c.Query().Count(ctx, d)
	if !errors.Is(err, keel.ErrBudgetExceeded) {
		t.Fatalf("a later terminal under the exceeded budget: got %v", err)
	}
	r, _ := keel.BudgetReportFrom(ctx)
	eq(t, r.RoundTrips, 2)
}

func TestBudgetFindsRepeatsAndTheirCallSites(t *testing.T) {
	d := db(t)
	w := seed(t, 3)
	s := fleetdb.Shipments
	ctx := keel.Budget(w.ctx, keel.MaxRepeats(2))
	var err error
	for i, sh := range w.shipments {
		_, err = s.Get(ctx, d, sh.ID)
		if i < 2 {
			ok(t, err)
		}
	}
	var be *keel.BudgetError
	if !errors.As(err, &be) {
		t.Fatalf("got %v, want a *BudgetError", err)
	}
	eq(t, be.Limit, "repeats")
	eq(t, len(be.Report.Repeats), 1)
	rep := be.Report.Repeats[0]
	eq(t, rep.Count, 3)
	if !strings.Contains(rep.SQL, "shipments") {
		t.Fatalf("repeated text %q", rep.SQL)
	}
	if len(rep.CallSites) != 1 || !strings.Contains(rep.CallSites[0], "budget_test.go:") {
		t.Fatalf("call sites %v, want this file", rep.CallSites)
	}
}

func TestBudgetOnExceeded(t *testing.T) {
	d := db(t)
	w := seed(t, 1)
	c := fleetdb.Customers
	var calls int
	var got keel.BudgetReport
	ctx := keel.Budget(w.ctx, keel.MaxStatements(1), keel.OnExceeded(func(_ context.Context, r keel.BudgetReport) {
		calls++
		got = r
	}))
	for range 3 {
		_ = must(c.Query().Count(ctx, d))(t)
	}
	eq(t, calls, 1)
	eq(t, got.Statements, 2)
	r, _ := keel.BudgetReportFrom(ctx)
	eq(t, r.Statements, 3)
	eq(t, len(r.Repeats), 1)
}

func TestNestedAndConcurrentBudgets(t *testing.T) {
	d := db(t)
	w := seed(t, 1)
	c := fleetdb.Customers
	outer := keel.Budget(w.ctx)
	inner := keel.Budget(outer, keel.MaxRoundTrips(20))
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Query().Count(inner, d); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	_ = must(c.Query().Count(outer, d))(t)
	ri, _ := keel.BudgetReportFrom(inner)
	ro, _ := keel.BudgetReportFrom(outer)
	eq(t, ri.RoundTrips, 10)
	eq(t, ro.RoundTrips, 11)

	none := keel.Budget(w.ctx, keel.MaxRoundTrips(0))
	if _, err := c.Query().Count(none, d); !errors.Is(err, keel.ErrBudgetExceeded) {
		t.Fatalf("MaxRoundTrips(0): got %v", err)
	}
}
