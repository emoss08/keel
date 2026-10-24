package keel_test

import (
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// samePros fails unless got and want hold the same shipments in the same order.
func samePros(t *testing.T, got, want []*fleet.Shipment) {
	t.Helper()
	g, w := strings.Join(pros(got), ","), strings.Join(pros(want), ",")
	if g != w {
		t.Fatalf("paged order\n%s\nwant\n%s", g, w)
	}
}

func TestPathMemoIsPerHandleType(t *testing.T) {
	s := fleetdb.Shipments
	rel := fleetdb.ShipmentsSchema().Relation("BillTo")
	ref := keel.Path(s.Ref, rel, func(r *keel.Ref) *keel.Ref { return r })
	typed := s.BillTo()
	if typed.Ref != ref {
		t.Fatal("a handle type of its own got another Ref for the same relation")
	}
	again := keel.Path(s.Ref, rel, func(r *keel.Ref) *keel.Ref { return r })
	eq(t, again, ref)
	eq(t, s.BillTo(), typed)

	wk := fleetdb.Workers
	aref := keel.Alias(wk.Ref, "boss", func(r *keel.Ref) *keel.Ref { return r })
	eq(t, wk.As("boss").Ref, aref)
}

func TestResolvePath(t *testing.T) {
	d := db(t)
	w := seed(t, 9)
	s := fleetdb.Shipments

	name := keel.ResolvePath(s, "bill_to", "name")
	eq(t, name.Err(), nil)
	eq(t, name.Label(), "bill_to.name")
	got := must(s.Where(name.Eq("Customer C0"), keel.ResolvePath(s, "Status").Ne(fleet.StatusDelivered)).
		Order(keel.ResolvePath(s, "pro_number").Asc()).Limit(100).List(w.ctx, d))(t)
	want := must(s.Where(s.BillTo().Name.Eq("Customer C0"), s.Status.Ne(fleet.StatusDelivered)).
		Order(s.ProNumber.Asc()).Limit(100).List(w.ctx, d))(t)
	samePros(t, got, want)
	if len(got) == 0 {
		t.Fatal("no rows")
	}

	sql, _, err := s.Where(name.IsNotNull(), s.BillTo().Code.Eq("C0")).
		Order(keel.ResolvePath(s, "BillTo", "Code").Desc()).ToSQL(w.ctx, d)
	ok(t, err)
	eq(t, strings.Count(sql, "JOIN"), 1)

	m := fleetdb.Moves
	deep := must(m.Where(keel.ResolvePath(m, "shipment", "customer", "code").Eq("C0")).Count(w.ctx, d))(t)
	eq(t, deep, must(m.Where(m.Shipment().Customer().Code.Eq("C0")).Count(w.ctx, d))(t))

	moves := s.Moves()
	any := must(s.Where(moves.Any(keel.ResolvePath(moves, "sequence").Eq(1))).Count(w.ctx, d))(t)
	eq(t, any, int64(6))

	for _, segs := range [][]string{{"nope", "name"}, {"customer", "nope"}, {"moves", "sequence"}, {}} {
		e := keel.ResolvePath(s, segs...)
		if e.Err() == nil {
			t.Fatalf("ResolvePath(%v) resolved", segs)
		}
		if _, err := s.Where(e.IsNull()).Count(w.ctx, d); err == nil {
			t.Fatalf("query with ResolvePath(%v) ran", segs)
		}
	}
	if keel.ResolvePath(nil, "id").Err() == nil {
		t.Fatal("ResolvePath of nil resolved")
	}
}
