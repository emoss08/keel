package keel_test

import (
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/internal/testmodels/ids"
)

func TestUnnestBulkUpdate(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Contacts
	rows := []*fleet.Contact{{Name: "a", Score: 1}, {Name: "b", Score: 2}, {Name: "c", Score: 3}}
	ok(t, c.CreateMany(ctx, d, rows))
	get := func(name string) *fleet.Contact { return must(c.Where(c.Name.Eq(name)).One(ctx, d))(t) }

	out := keel.Unnest("outcome", []string{"id", "phone", "score"},
		[]ids.ID{rows[0].ID, rows[1].ID}, []string{"555-1", "555-2"}, []int64{10, 20})
	n := must(c.With(out).Join(out, out.Col("id").EqCol(c.ID)).Where(c.Score.Lt(3)).
		Update(ctx, d, c.Phone.ToExpr(out.Col("phone")), c.Score.ToExpr(out.Col("score"))))(t)
	eq(t, n, 2)
	eq(t, get("a").Phone, "555-1")
	eq(t, get("b").Score, int64(20))
	eq(t, get("c").Score, int64(3))
	eq(t, get("a").Version, int64(1))

	gone := keel.Unnest("gone", []string{"id"}, []ids.ID{rows[2].ID})
	n = must(c.With(gone).Join(gone, gone.Col("id").EqCol(c.ID)).Where(keel.True()).Delete(ctx, d))(t)
	eq(t, n, 1)
	eq(t, must(c.Query().Count(ctx, d))(t), 2)

	org, bu := tenantOf(ctx)
	sys := systemCtx("unnest")
	all := keel.Unnest("all_out", []string{"id", "floor"}, []ids.ID{rows[0].ID}, []int16{7})
	n = must(c.With(all).Join(all, all.Col("id").EqCol(c.ID)).AllTenants().
		Where(c.OrganizationID.Eq(org), c.BusinessUnitID.Eq(bu)).Update(sys, d, c.Floor.ToExpr(all.Col("floor"))))(t)
	eq(t, n, 1)
	eq(t, get("a").Floor, int16(7))
}

func TestBulkWriteJoinsTables(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c, cu := fleetdb.Contacts, fleetdb.Customers
	acme := newCustomer(t, ctx, "ACMEJ")
	ok(t, c.CreateMany(ctx, d, []*fleet.Contact{{Name: "x", CustomerID: acme.ID}, {Name: "y"}}))
	n := must(c.Join(cu, cu.ID.EqCol(c.CustomerID)).Where(cu.Code.Eq("ACMEJ")).
		Update(ctx, d, c.Phone.ToExpr(cu.Code)))(t)
	eq(t, n, 1)
	eq(t, must(c.Where(c.Phone.Eq("ACMEJ")).Count(ctx, d))(t), 1)
	_, err := c.LeftJoin(cu, cu.ID.EqCol(c.CustomerID)).Where(keel.True()).Update(ctx, d, c.Phone.To("z"))
	if err == nil || !strings.Contains(err.Error(), "inner joins of tables and CTEs only") {
		t.Fatalf("got %v", err)
	}
}

func TestUnnestErrors(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Contacts
	for name, cte := range map[string]*keel.CTETable{
		"lengths": keel.Unnest("u", []string{"a", "b"}, []string{"x"}, []int64{1, 2}),
		"columns": keel.Unnest("u", []string{"a", "b"}, []string{"x"}),
		"type":    keel.Unnest("u", []string{"a"}, []map[string]int{{}}),
		"slice":   keel.Unnest("u", []string{"a"}, "x"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.With(cte).Join(cte, cte.Col("a").EqCol(c.Name)).Where(keel.True()).
				Update(ctx, d, c.Phone.To("p"))
			if err == nil || !strings.Contains(err.Error(), "Unnest") {
				t.Fatalf("got %v", err)
			}
		})
	}
}
