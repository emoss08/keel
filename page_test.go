package keel_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// pageAll reads every page of the query built by q, size rows at a time, and returns the rows in
// page order, checking that every cursor of every page resumes right after its row.
func pageAll(t *testing.T, ctx context.Context, q func() *keel.Query[*fleet.Shipment], size int) []*fleet.Shipment {
	t.Helper()
	d := db(t)
	var all []*fleet.Shipment
	cursor := ""
	for range 100 {
		p := must(q().Page(ctx, d, keel.After(cursor, size)))(t)
		for i := range p.Items {
			if i+1 < len(p.Items) {
				next := must(q().Page(ctx, d, keel.After(p.CursorAt(i), 1)))(t)
				if len(next.Items) != 1 || next.Items[0].ID != p.Items[i+1].ID {
					t.Fatalf("CursorAt(%d) of %s did not resume at %s", i, p.Items[i].ProNumber,
						p.Items[i+1].ProNumber)
				}
			}
		}
		if p.Next != "" {
			eq(t, p.CursorAt(len(p.Items)-1), p.Next)
		}
		all = append(all, p.Items...)
		if p.Next == "" {
			return all
		}
		cursor = p.Next
	}
	t.Fatal("pages did not end")
	return nil
}

func TestPageCursorAt(t *testing.T) {
	d := db(t)
	w := seed(t, 13)
	s := fleetdb.Shipments
	q := func() *keel.Query[*fleet.Shipment] { return s.Order(s.Weight.Desc(), s.ProNumber.Asc()) }
	got := pageAll(t, w.ctx, q, 4)
	want := must(q().Limit(100).List(w.ctx, d))(t)
	samePros(t, got, want)

	last := must(s.Order(s.ProNumber.Asc()).Page(w.ctx, d, keel.After("", 20)))(t)
	eq(t, last.Next, "")
	end := must(s.Order(s.ProNumber.Asc()).Page(w.ctx, d, keel.After(last.CursorAt(len(last.Items)-1), 5)))(t)
	eq(t, len(end.Items), 0)
	eq(t, keel.Page[*fleet.Shipment]{Items: last.Items}.CursorAt(0), "")
}

func TestPageRelationOrder(t *testing.T) {
	d := db(t)
	w := seed(t, 14)
	s := fleetdb.Shipments
	cases := []struct {
		name  string
		order []keel.Order
		tie   keel.Order
	}{
		{"belongs", []keel.Order{s.Customer().Name.Desc()}, s.ID.Desc()},
		{"nullable belongs", []keel.Order{s.BillTo().Name.Asc()}, s.ID.Asc()},
		{"nullable belongs desc", []keel.Order{s.BillTo().Name.Desc()}, s.ID.Desc()},
		{"nulls first", []keel.Order{s.BillTo().Name.Asc().NullsFirst()}, s.ID.Asc()},
		{"nulls last desc", []keel.Order{s.BillTo().Name.Desc().NullsLast(), s.Customer().Code.Asc()}, s.ID.Asc()},
		{"mixed with base", []keel.Order{s.BillTo().Code.Asc(), s.Priority.Desc()}, s.ID.Desc()},
		{"pick", []keel.Order{s.DestinationStop().Kind.Asc()}, s.ID.Asc()},
		{"expression", []keel.Order{keel.Desc(keel.Raw("nullif(? % 3, 0)", s.Priority))}, s.ID.Desc()},
		{"string expression", []keel.Order{s.Customer().Name.Lower().Asc(), keel.Asc(keel.Raw("? > 1200", s.Weight))},
			s.ID.Asc()},
		{"resolved path", []keel.Order{keel.ResolvePath(s, "bill_to", "code").Desc()}, s.ID.Desc()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := func() *keel.Query[*fleet.Shipment] { return s.Load(s.Customer()).Order(c.order...) }
			got := pageAll(t, w.ctx, q, 3)
			want := must(s.Order(append(c.order, c.tie)...).Limit(100).List(w.ctx, d))(t)
			samePros(t, got, want)
			for _, sh := range got {
				if sh.Customer == nil {
					t.Fatalf("%s: customer not loaded", sh.ProNumber)
				}
			}
		})
	}

	p := must(s.Where(s.Hazmat.IsFalse()).Order(s.BillTo().Name.Asc()).
		Page(w.ctx, d, keel.After("", 4).Total(100)))(t)
	eq(t, p.Total, int64(11))
	eq(t, len(p.Items), 4)

	if _, err := s.Order(s.Moves().Sequence.Asc()).Page(w.ctx, d, keel.After("", 2)); err == nil ||
		!strings.Contains(err.Error(), "to-many") {
		t.Fatalf("order by a to-many relation: %v", err)
	}
	if _, err := s.Order(keel.ResolvePath(s, "bill_to", "nope").Asc()).Page(w.ctx, d, keel.After("", 2)); err == nil {
		t.Fatal("order by an unknown column succeeded")
	}
}

func TestPageRelationOrderSQL(t *testing.T) {
	d := db(t)
	w := seed(t, 4)
	s := fleetdb.Shipments
	first := must(s.Order(s.BillTo().Name.Asc()).Page(w.ctx, d, keel.After("", 1)))(t)
	sql, _, err := s.Order(s.BillTo().Name.Asc()).Load(s.BillTo()).ToSQL(w.ctx, d)
	ok(t, err)
	eq(t, strings.Count(sql, "JOIN"), 1)
	if first.Next == "" {
		t.Fatal("no next page")
	}
}

func TestDynamicPageCursorAt(t *testing.T) {
	d := db(t)
	w := seed(t, 12)
	reg := registry(t)
	build := func() *keel.DynQuery {
		q := reg.Query("shipments")
		return q.Select(q.Path("pro_number")).Order(q.Path("bill_to", "name").Asc(), q.Path("weight").Desc())
	}
	var all []string
	cursor := ""
	for {
		res := must(build().Page(w.ctx, d, keel.After(cursor, 5)))(t)
		col := res.Column("pro_number").String
		for i := range res.Rows - 1 {
			next := must(build().Page(w.ctx, d, keel.After(res.CursorAt(i), 1)))(t)
			eq(t, next.Column("pro_number").String[0], col[i+1])
		}
		if res.Next != "" {
			eq(t, res.CursorAt(res.Rows-1), res.Next)
		}
		all = append(all, col...)
		if res.Next == "" {
			break
		}
		cursor = res.Next
	}
	eq(t, len(all), 12)
	run := must(build().Run(w.ctx, d))(t)
	eq(t, run.CursorAt(0), "")
}

func TestPageFloat32Order(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Contacts
	rows := []*fleet.Contact{}
	for i, wt := range []float32{2.5, 0, 1.25, 2.5, 0, 3.75, 1.25} {
		rows = append(rows, &fleet.Contact{Name: fmt.Sprintf("c%d", i), Weight: wt})
	}
	ok(t, c.CreateMany(ctx, d, rows))
	var got []string
	cursor := ""
	for {
		p := must(c.Order(c.Weight.Desc()).Page(ctx, d, keel.After(cursor, 2)))(t)
		for _, r := range p.Items {
			got = append(got, r.Name)
		}
		if p.Next == "" {
			break
		}
		cursor = p.Next
	}
	want := must(c.Order(c.Weight.Desc(), c.ID.Desc()).Limit(10).List(ctx, d))(t)
	names := make([]string, len(want))
	for i, r := range want {
		names[i] = r.Name
	}
	eq(t, strings.Join(got, ","), strings.Join(names, ","))
}
