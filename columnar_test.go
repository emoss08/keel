package keel_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

func TestCreateManyEveryKind(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := newCustomer(t, ctx, "K")
	w := int64(42)
	tricky := []string{`plain`, `with "quotes"`, `back\slash`, `comma,brace}`, ``}
	rows := []*fleet.Shipment{
		{CustomerID: c.ID, ProNumber: "A", Status: fleet.StatusNew, Weight: &w, Total: fleet.Money{Cents: 1234},
			Labels: tricky, Attributes: map[string]any{"k": "v"}, BillToID: &c.ID, Priority: 2, Hazmat: true},
		{CustomerID: c.ID, ProNumber: "B", Status: fleet.StatusDelivered},
	}
	ok(t, fleetdb.Shipments.CreateMany(ctx, d, rows))
	eq(t, rows[0].StageRank, int16(1))
	eq(t, rows[1].StageRank, int16(4))

	s := fleetdb.Shipments
	got := must(s.Order(s.ProNumber.Asc()).All(ctx, d))(t)
	a, b := got[0], got[1]
	eq(t, *a.Weight, int64(42))
	eq(t, a.Total.Cents, int64(1234))
	eq(t, *a.BillToID, c.ID)
	eq(t, a.Priority, fleet.Priority(2))
	eq(t, a.Hazmat, true)
	eq(t, a.Attributes["k"], any("v"))
	if !slices.Equal(a.Labels, tricky) {
		t.Fatalf("labels %q", a.Labels)
	}
	eq(t, b.Weight == nil, true)
	eq(t, b.BillToID == nil, true)
	eq(t, len(b.Labels), 0)
	eq(t, len(b.Attributes), 0)
	eq(t, b.Status, fleet.StatusDelivered)
}

func TestCreateManyChunks(t *testing.T) {
	d := db(t)
	rec := &recorder{}
	sd := scopedDB(t, rec)
	_ = d
	ctx := newTenant()
	rows := make([]*fleet.Customer, 10_005)
	for i := range rows {
		rows[i] = &fleet.Customer{Name: "bulk", Code: fmt.Sprintf("B%05d", i)}
	}
	ok(t, fleetdb.Customers.CreateMany(ctx, sd, rows))
	eq(t, must(fleetdb.Customers.Query().Count(ctx, sd))(t), int64(10_005))
	inserts := 0
	var sql []string
	rec.mu.Lock()
	for _, e := range rec.events {
		if e.Op == "insert" {
			inserts++
			sql = append(sql, e.SQL)
		}
	}
	rec.mu.Unlock()
	eq(t, inserts, 2)
	eq(t, sql[0], sql[1])
	eq(t, rec.trips() <= 3, true)
}

func TestUpsertManyThroughUnnest(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Customers
	existing := []*fleet.Customer{{Name: "one", Code: "U1"}, {Name: "two", Code: "U2"}}
	ok(t, c.CreateMany(ctx, d, existing))
	rows := []*fleet.Customer{{Name: "one!", Code: "U1"}, {Name: "three", Code: "U3"}, {Name: "two!", Code: "U2"}}
	ins := must(c.Upsert(ctx, d, rows, keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Code).Update(c.Name)))(t)
	if !slices.Equal(ins, []bool{false, true, false}) {
		t.Fatalf("inserted %v", ins)
	}
	eq(t, rows[0].Version, int64(1))
	got := must(c.Where(c.Code.Eq("U2")).One(ctx, d))(t)
	eq(t, got.Name, "two!")
}
