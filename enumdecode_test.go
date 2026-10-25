package keel_test

import (
	"testing"
	"unsafe"

	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/schema"
)

// sharesConstant fails unless v is one of c's enum constants and holds that constant's own string.
func sharesConstant(t *testing.T, c *schema.Column, v string) {
	t.Helper()
	for _, e := range c.Enum {
		if e.Value != v {
			continue
		}
		if unsafe.StringData(v) != unsafe.StringData(e.Value) {
			t.Fatalf("%s %q decoded into a string of its own", c.Name, v)
		}
		return
	}
	t.Fatalf("%q is not a constant of %s", v, c.Name)
}

func TestEnumColumnsDecodeToTheirConstants(t *testing.T) {
	d := db(t)
	w := seed(t, 4)
	s := fleetdb.Shipments
	rows := must(s.Order(s.ProNumber.Asc()).All(w.ctx, d))(t)
	eq(t, len(rows), 4)
	eq(t, rows[2].Status, fleet.StatusInTransit)
	for _, r := range rows {
		sharesConstant(t, &s.Ref.Table.Columns[s.Status.Index], string(r.Status))
	}

	c := fleetdb.Contacts
	ok(t, c.CreateMany(w.ctx, d, []*fleet.Contact{{Name: "a", Status: fleet.StatusDelivered}, {Name: "b"}}))
	contacts := must(c.Order(c.Name.Asc()).All(w.ctx, d))(t)
	eq(t, len(contacts), 2)
	eq(t, contacts[0].Status, fleet.StatusDelivered)
	sharesConstant(t, &c.Ref.Table.Columns[c.Status.Index], string(contacts[0].Status))
	eq(t, contacts[1].Status, fleet.Status(""))
}
