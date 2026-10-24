package keel_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// computeFixture creates two customers, the first with two shipments, and a contact of each plus
// one without a customer.
func computeFixture(t *testing.T) (context.Context, *keel.DB) {
	t.Helper()
	d := db(t)
	ctx := newTenant()
	acme := newCustomer(t, ctx, "ACME")
	glob := newCustomer(t, ctx, "GLOB")
	ok(t, fleetdb.Shipments.CreateMany(ctx, d, []*fleet.Shipment{
		{CustomerID: acme.ID, ProNumber: "P1", Status: fleet.StatusNew},
		{CustomerID: acme.ID, ProNumber: "P2", Status: fleet.StatusNew},
	}))
	ok(t, fleetdb.Contacts.CreateMany(ctx, d, []*fleet.Contact{
		{Name: "ann", CustomerID: acme.ID}, {Name: "bob", CustomerID: glob.ID}, {Name: "cy"},
	}))
	return ctx, d
}

// loadsOf is the number of shipments of the contact's customer.
func loadsOf(c *fleetdb.ContactsTable) keel.Expr {
	s := fleetdb.Shipments
	return keel.Scalar(s.Pick(keel.Count()).Where(s.CustomerID.EqCol(c.CustomerID)))
}

func TestComputeFillsFields(t *testing.T) {
	ctx, d := computeFixture(t)
	c := fleetdb.Contacts
	rows := must(c.Compute(c.CustomerName, c.Customer().Name).Compute(c.Loads, loadsOf(c)).
		Order(c.Loads.Desc(), c.Name.Asc()).All(ctx, d))(t)
	eq(t, len(rows), 3)
	eq(t, rows[0].Name, "ann")
	eq(t, rows[0].CustomerName, "Customer ACME")
	eq(t, rows[0].Loads, int64(2))
	eq(t, rows[1].Name, "bob")
	eq(t, rows[1].Loads, int64(0))
	eq(t, rows[2].CustomerName, "")

	busy := must(c.Compute(c.Loads, loadsOf(c)).Where(c.Loads.Gt(0)).All(ctx, d))(t)
	eq(t, len(busy), 1)
	eq(t, busy[0].Name, "ann")
	eq(t, must(c.Compute(c.Loads, loadsOf(c)).Where(c.Loads.Eq(0)).Count(ctx, d))(t), 2)

	plain := must(c.Where(c.Name.Eq("ann")).One(ctx, d))(t)
	eq(t, plain.Loads, int64(0))
	eq(t, plain.CustomerName, "")

	picked := must(c.Pick(c.Name, c.Loads).Compute(c.Loads, loadsOf(c)).Where(c.Name.Eq("ann")).One(ctx, d))(t)
	eq(t, picked.Loads, int64(2))
	eq(t, picked.Phone, "")
}

func TestComputePagesAndLoads(t *testing.T) {
	ctx, d := computeFixture(t)
	c := fleetdb.Contacts
	var names []string
	cursor := ""
	for {
		page := must(c.Compute(c.Loads, loadsOf(c)).Order(c.Loads.Desc()).Page(ctx, d, keel.After(cursor, 2)))(t)
		for _, r := range page.Items {
			names = append(names, r.Name+":"+string(rune('0'+r.Loads)))
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	eq(t, len(names), 3)
	eq(t, names[0], "ann:2")

	type contactLoads struct {
		Name  string
		Loads int64
	}
	sum := must(keel.Into[contactLoads](ctx, d, c.Compute(c.Loads, loadsOf(c)).Pick(c.Name, c.Loads).
		Where(c.Name.Eq("ann"))))(t)
	eq(t, len(sum), 1)
	eq(t, sum[0].Loads, int64(2))
}

func TestComputeErrors(t *testing.T) {
	ctx, d := computeFixture(t)
	c := fleetdb.Contacts
	cases := map[string]struct {
		run  func() error
		want string
	}{
		"not computed": {func() error {
			_, err := c.Where(c.Loads.Gt(0)).All(ctx, d)
			return err
		}, "contacts.loads is computed; fill it with Compute"},
		"stored column": {func() error {
			_, err := c.Compute(c.Name, keel.Raw("'x'")).All(ctx, d)
			return err
		}, "contacts.name is a column; Compute fills fields tagged computed"},
		"type mismatch": {func() error {
			_, err := c.Compute(c.Loads, c.Name).All(ctx, d)
			return err
		}, "Compute contacts.loads: the field is bigint, the expression is text"},
		"twice": {func() error {
			_, err := c.Compute(c.Loads, keel.Count()).Compute(c.Loads, keel.Count()).All(ctx, d)
			return err
		}, "contacts.loads is computed twice"},
		"other table": {func() error {
			_, err := fleetdb.Shipments.Compute(c.Loads, keel.Count()).All(ctx, d)
			return err
		}, "Compute fills a field of contacts, not of the query's table shipments"},
		"update": {func() error {
			_, err := c.Where(keel.True()).Update(ctx, d, c.Loads.To(1))
			return err
		}, "Update assigns the computed field contacts.loads"},
		"upsert": {func() error {
			_, err := c.Upsert(ctx, d, []*fleet.Contact{{Name: "ann"}},
				keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Name).Update(c.Loads))
			return err
		}, "OnConflict assigns the computed field contacts.loads"},
		"save": {func() error {
			return c.Save(ctx, d, &fleet.Contact{Name: "x"}, c.Loads)
		}, "Save cannot write contacts.loads"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.run()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestComputeWritesIgnoreFields(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Contacts
	con := &fleet.Contact{Name: "w", Loads: 9, CustomerName: "ignored"}
	ok(t, c.Create(ctx, d, con))
	ok(t, c.CreateMany(ctx, d, []*fleet.Contact{{Name: "w2", Loads: 1}, {Name: "w3"}}))
	n := must(c.Copy(ctx, d, []*fleet.Contact{{Name: "w4", Loads: 3}}))(t)
	eq(t, n, 1)
	con.Phone = "1"
	ok(t, c.Save(ctx, d, con))
	eq(t, must(c.Where(c.Name.HasPrefix("w")).Count(ctx, d))(t), 4)
	if fleetdb.ContactsSchema().Columns[fleetdb.Contacts.Loads.Index].Report != nil {
		t.Fatal("a computed field of a model without reports carries report facts")
	}
}

func TestComputeDynamic(t *testing.T) {
	ctx, d := computeFixture(t)
	q := keel.DynFrom(fleetdb.ContactsSchema())
	s := fleetdb.Shipments
	c := fleetdb.Contacts
	q.Compute("loads", keel.DynOf(keel.Scalar(s.Pick(keel.Count()).Where(s.CustomerID.EqCol(c.CustomerID))))).
		Where(q.Path("loads").Gt(0))
	res := must(q.Run(ctx, d))(t)
	eq(t, res.Rows, 1)
	loads := res.Column("loads")
	if loads == nil || loads.Int64[0] != 2 {
		t.Fatalf("computed column: %+v", res.Columns)
	}

	bad := keel.DynFrom(fleetdb.ContactsSchema()).Compute("name", keel.DynLit(1))
	if err := bad.Err(); err == nil || !strings.Contains(err.Error(), "is a column") {
		t.Fatalf("got %v", err)
	}
	missing := keel.DynFrom(fleetdb.ContactsSchema())
	missing.Select(missing.Path("loads"))
	if _, err := missing.Run(ctx, d); err == nil || !errors.Is(err, err) ||
		!strings.Contains(err.Error(), "fill it with Compute") {
		t.Fatalf("got %v", err)
	}
}
