package keel_test

import (
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

func TestOmitLeavesColumnsUnread(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Contacts
	cust := newCustomer(t, ctx, "OMIT")
	full := fullContact("omit", cust.ID)
	ok(t, c.Create(ctx, d, full))

	got := must(c.Omit(c.Avatar, c.Prefs, c.ID).Where(c.Name.Eq("omit")).One(ctx, d))(t)
	eq(t, got.Avatar == nil && got.Prefs == nil, true)
	eq(t, got.ID, full.ID)
	eq(t, got.Phone, full.Phone)
	sql, _ := must2(c.Omit(c.Avatar).Where(c.Name.Eq("omit")).ToSQL(ctx, d))(t)
	if strings.Contains(sql, `"avatar"`) {
		t.Fatalf("an omitted column is read: %s", sql)
	}

	picked := must(c.Pick(c.Name, c.Phone).Omit(c.Phone).Where(c.Name.Eq("omit")).One(ctx, d))(t)
	eq(t, picked.Phone, "")
	eq(t, picked.Name, "omit")

	s := fleetdb.Shipments
	ok(t, s.Create(ctx, d, &fleet.Shipment{CustomerID: cust.ID, ProNumber: "OM-1", Status: fleet.StatusNew,
		Labels: []string{"a"}}))
	cs := fleetdb.Customers
	sh := s.Customer()
	rows := must(s.Load(sh.Omit(sh.Email, sh.Name)).Omit(s.Labels).Where(s.ProNumber.Eq("OM-1")).All(ctx, d))(t)
	eq(t, rows[0].Labels == nil, true)
	eq(t, rows[0].Customer.Code, "OMIT")
	eq(t, rows[0].Customer.Name, "")

	_, err := c.Omit(cs.Name).Where(c.Name.Eq("omit")).All(ctx, d)
	if err == nil || !strings.Contains(err.Error(), "Omit names a column of customers on a query of contacts") {
		t.Fatalf("got %v", err)
	}

	q := keel.DynFrom(fleetdb.ContactsSchema()).Omit("avatar", "Prefs")
	res := must(q.Where(q.Path("name").Eq("omit")).Run(ctx, d))(t)
	if res.Column("avatar") != nil || res.Column("prefs") != nil || res.Column("phone") == nil {
		t.Fatalf("dynamic columns %+v", res.Columns)
	}
}

func TestSaveExcept(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Contacts
	cust := newCustomer(t, ctx, "SAVEX")
	full := fullContact("savex", cust.ID)
	ok(t, c.Create(ctx, d, full))

	stale := must(c.Get(ctx, d, full.ID))(t)
	_, err := c.Where(c.ID.Eq(full.ID)).Update(ctx, d, c.Phone.To("555-9999"), c.Version.Add(0))
	ok(t, err)
	fresh := must(c.Get(ctx, d, full.ID))(t)
	fresh.Name, fresh.Phone = "renamed", "stale"
	ok(t, c.SaveExcept(ctx, d, fresh, c.Phone))
	got := must(c.Get(ctx, d, full.ID))(t)
	eq(t, got.Name, "renamed")
	eq(t, got.Phone, "555-9999")
	eq(t, stale.Phone, "555-0100")

	if err := c.SaveExcept(ctx, d, got, c.ID); err == nil || !strings.Contains(err.Error(), "Save cannot write contacts.id") {
		t.Fatalf("got %v", err)
	}
}
