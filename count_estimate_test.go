package keel_test

import (
	"context"
	"fmt"
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// inexact fails the test unless c is an estimate above upTo.
func inexact(t *testing.T, c keel.RowCount, upTo int64) {
	t.Helper()
	if c.Exact || c.N <= upTo {
		t.Fatalf("count %+v: want an estimate above %d", c, upTo)
	}
}

func TestCountEstimate(t *testing.T) {
	d := db(t)
	w := seed(t, 12)
	s := fleetdb.Shipments

	eq(t, must(s.Query().CountEstimate(w.ctx, d, 100))(t), keel.RowCount{N: 12, Exact: true})
	eq(t, must(s.Query().CountEstimate(w.ctx, d, 12))(t), keel.RowCount{N: 12, Exact: true})
	inexact(t, must(s.Query().CountEstimate(w.ctx, d, 11))(t), 11)
	inexact(t, must(s.Query().CountEstimate(w.ctx, d, 0))(t), 0)

	hazmat := func() *keel.Query[*fleet.Shipment] { return s.Where(s.Hazmat.IsTrue()) }
	want := must(hazmat().Count(w.ctx, d))(t)
	eq(t, must(hazmat().CountEstimate(w.ctx, d, 100))(t), keel.RowCount{N: want, Exact: true})
	inexact(t, must(hazmat().CountEstimate(w.ctx, d, want-1))(t), want-1)

	none := s.Where(s.ProNumber.Eq("nope"))
	eq(t, must(none.CountEstimate(w.ctx, d, 0))(t), keel.RowCount{N: 0, Exact: true})

	ordered := s.Order(s.ProNumber.Desc()).Limit(3).Offset(2)
	eq(t, must(ordered.CountEstimate(w.ctx, d, 50))(t), keel.RowCount{N: 12, Exact: true})

	groups := func() *keel.Query[*fleet.Shipment] { return s.Group(s.Status) }
	eq(t, must(groups().CountEstimate(w.ctx, d, 10))(t), keel.RowCount{N: 4, Exact: true})
	inexact(t, must(groups().CountEstimate(w.ctx, d, 2))(t), 2)

	if _, err := s.Query().CountEstimate(w.ctx, d, -1); err == nil {
		t.Fatal("a negative limit succeeded")
	}
	if err := keel.Batch(w.ctx, d, keel.CountEstimateInto(s.Query(), 5, nil)); err == nil {
		t.Fatal("a nil destination succeeded")
	}

	ok(t, keel.Read(w.ctx, d, func(ctx context.Context) error {
		c, err := s.Query().CountEstimate(ctx, d, 4)
		if err != nil {
			return err
		}
		inexact(t, c, 4)
		return nil
	}))
}

func TestCountEstimateStatements(t *testing.T) {
	rec := &recorder{}
	d := openDB(t, func(cfg *keel.Config) {
		cfg.Observers = []keel.Observer{rec}
		cfg.ObserveArgs = true
	})
	w := seed(t, 8)
	s := fleetdb.Shipments

	rec.reset()
	c := must(s.Where(s.Hazmat.IsFalse(), s.ProNumber.Ne("nope")).CountEstimate(w.ctx, d, 3))(t)
	inexact(t, c, 3)
	eq(t, rec.statements(), 2)
	eq(t, rec.trips(), 1)

	var count, plan keel.QueryEvent
	for _, e := range rec.events {
		switch e.Op {
		case "count":
			count = e
		case "explain":
			plan = e
		}
	}
	rows, found := strings.CutPrefix(plan.SQL, "EXPLAIN (FORMAT JSON) SELECT 1 FROM ")
	if !found {
		t.Fatalf("explain statement: %s", plan.SQL)
	}
	eq(t, count.SQL, "SELECT count(*) FROM (SELECT 1 FROM "+rows+" LIMIT 4) c")
	eq(t, strings.Contains(plan.SQL, "LIMIT"), false)
	if count.Args == 0 || !reflect.DeepEqual(count.Values, plan.Values) {
		t.Fatalf("arguments differ: count %v, explain %v", count.Values, plan.Values)
	}

	var items []*fleet.Shipment
	var total int64
	var est keel.RowCount
	rec.reset()
	ok(t, keel.Batch(w.ctx, d,
		keel.ListInto(s.Order(s.ProNumber.Asc()).Limit(2), &items),
		keel.CountInto(s.Query(), &total),
		keel.CountEstimateInto(s.Query(), 100, &est),
	))
	eq(t, len(items), 2)
	eq(t, est, keel.RowCount{N: total, Exact: true})
	eq(t, rec.statements(), 4)
	eq(t, rec.trips(), 1)
}

func TestPageEstimate(t *testing.T) {
	rec := &recorder{}
	d := openDB(t, func(cfg *keel.Config) { cfg.Observers = []keel.Observer{rec} })
	w := seed(t, 12)
	s := fleetdb.Shipments
	paged := func() *keel.Query[*fleet.Shipment] { return s.Order(s.ProNumber.Asc()) }

	plain := must(paged().Page(w.ctx, d, keel.After("", 5)))(t)
	eq(t, plain.Total, int64(0))
	eq(t, plain.TotalExact, false)

	capped := must(paged().Page(w.ctx, d, keel.After("", 5).Total(10)))(t)
	eq(t, capped.Total, int64(10))
	eq(t, capped.TotalExact, false)

	rec.reset()
	p := must(paged().Page(w.ctx, d, keel.After("", 5).Total(10).Estimate()))(t)
	eq(t, len(p.Items), 5)
	eq(t, p.TotalExact, false)
	if p.Total <= 10 {
		t.Fatalf("estimated total %d: want more than 10", p.Total)
	}
	eq(t, rec.statements(), 3)
	eq(t, rec.trips(), 1)

	exact := must(paged().Page(w.ctx, d, keel.After(p.Next, 5).Total(100).Estimate()))(t)
	eq(t, exact.Total, int64(12))
	eq(t, exact.TotalExact, true)
	eq(t, exact.Items[0].ProNumber, "PRO-0005")

	boundary := must(paged().Page(w.ctx, d, keel.After("", 5).Total(12).Estimate()))(t)
	eq(t, boundary.Total, int64(12))
	eq(t, boundary.TotalExact, true)

	var page keel.Page[*fleet.Shipment]
	ok(t, keel.Batch(w.ctx, d, keel.PageInto(paged(), keel.After("", 4).Total(3).Estimate(), &page)))
	eq(t, len(page.Items), 4)
	eq(t, page.TotalExact, false)
	if page.Total <= 3 {
		t.Fatalf("estimated total %d: want more than 3", page.Total)
	}

	if _, err := paged().Page(w.ctx, d, keel.After("", 5).Estimate()); err == nil {
		t.Fatal("Estimate without Total succeeded")
	}
}

func TestDynamicCountEstimate(t *testing.T) {
	rec := &recorder{}
	d := openDB(t, func(cfg *keel.Config) { cfg.Observers = []keel.Observer{rec} })
	w := seed(t, 9)
	reg := registry(t)
	build := func() *keel.DynQuery {
		q := reg.Query("shipments")
		return q.Select(q.Path("pro_number"))
	}

	eq(t, must(build().CountEstimate(w.ctx, d, 100))(t), keel.RowCount{N: 9, Exact: true})
	eq(t, must(build().CountEstimate(w.ctx, d, 9))(t), keel.RowCount{N: 9, Exact: true})
	rec.reset()
	inexact(t, must(build().CountEstimate(w.ctx, d, 4))(t), 4)
	eq(t, rec.statements(), 2)
	eq(t, rec.trips(), 1)

	eq(t, must(build().Limit(3).CountEstimate(w.ctx, d, 100))(t), keel.RowCount{N: 3, Exact: true})

	var n int64
	var c keel.RowCount
	ok(t, keel.Batch(w.ctx, d, keel.DynCountInto(build(), &n), keel.DynCountEstimateInto(build(), 50, &c)))
	eq(t, c, keel.RowCount{N: n, Exact: true})

	if err := keel.Batch(w.ctx, d, keel.DynCountEstimateInto(nil, 5, &c)); err == nil {
		t.Fatal("a nil dynamic query succeeded")
	}
	if _, err := build().CountEstimate(w.ctx, d, -1); err == nil {
		t.Fatal("a negative limit succeeded")
	}
}

func ExampleQuery_CountEstimate() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments

	c, err := s.Where(s.Status.In(fleet.StatusNew, fleet.StatusAssigned)).CountEstimate(ctx, db, 1000)
	if err != nil {
		log.Fatal(err)
	}
	if c.Exact {
		fmt.Println(c.N, "shipments")
	} else {
		fmt.Println("about", c.N, "shipments")
	}
}

func TestCountsHonourDistinct(t *testing.T) {
	d := db(t)
	w := seed(t, 8)
	s := fleetdb.Shipments

	byCustomer := func() *keel.Query[*fleet.Shipment] { return s.DistinctOn(s.CustomerID) }
	eq(t, must(byCustomer().Count(w.ctx, d))(t), int64(3))
	eq(t, must(byCustomer().CountUpTo(w.ctx, d, 2))(t), int64(2))
	eq(t, must(byCustomer().CountEstimate(w.ctx, d, 10))(t), keel.RowCount{N: 3, Exact: true})

	prefixes := func() *keel.Query[*fleet.Shipment] { return s.Pick(s.ProNumber.Left(7)) }
	eq(t, must(prefixes().Count(w.ctx, d))(t), int64(8))
	eq(t, must(prefixes().Distinct().Count(w.ctx, d))(t), int64(1))
	eq(t, must(prefixes().Distinct().CountUpTo(w.ctx, d, 5))(t), int64(1))
	eq(t, must(prefixes().Distinct().CountEstimate(w.ctx, d, 5))(t),
		keel.RowCount{N: 1, Exact: true})
}
