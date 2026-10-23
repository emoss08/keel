package keel_test

import (
	"testing"
	"time"

	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

func TestNullableFieldsDecodeThroughCachedPlans(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := newCustomer(t, ctx, "N")
	w, canceled := int64(7), int64(99)
	at := time.Date(2026, 10, 23, 8, 30, 0, 250000000, time.UTC)
	ok(t, fleetdb.Shipments.CreateMany(ctx, d, []*fleet.Shipment{
		{CustomerID: c.ID, ProNumber: "N1", Status: fleet.StatusNew, BillToID: &c.ID, Weight: &w, CanceledAt: &canceled},
		{CustomerID: c.ID, ProNumber: "N2", Status: fleet.StatusNew},
	}))
	ok(t, fleetdb.Stops.CreateMany(ctx, d, []*fleet.Stop{
		{MoveID: "mov_n", LocationID: "loc_n", Sequence: 1, Kind: "pickup", ArrivedAt: &at},
		{MoveID: "mov_n", LocationID: "loc_n", Sequence: 2, Kind: "delivery"},
	}))
	s, st := fleetdb.Shipments, fleetdb.Stops
	for range 3 {
		ships := must(s.Order(s.ProNumber.Asc()).All(ctx, d))(t)
		a, b := ships[0], ships[1]
		eq(t, *a.BillToID, c.ID)
		eq(t, *a.Weight, w)
		eq(t, *a.CanceledAt, canceled)
		eq(t, b.BillToID == nil && b.Weight == nil && b.CanceledAt == nil, true)
		stops := must(st.Order(st.Sequence.Asc()).All(ctx, d))(t)
		eq(t, stops[0].ArrivedAt.Equal(at), true)
		eq(t, stops[1].ArrivedAt == nil, true)
	}
}
